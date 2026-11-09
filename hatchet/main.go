// Command openipdata-worker runs the openipdata workflows as a Hatchet worker,
// or runs a workflow once locally without Hatchet.
//
//	openipdata-worker                            # serve: connect to Hatchet (HATCHET_CLIENT_TOKEN)
//	openipdata-worker run-asn [-skip-publish]    # build-asn-data
//	openipdata-worker run-pdb                    # sync-peeringdb
//	openipdata-worker run-domains                # update-asn-domains
//	openipdata-worker run-geofeeds [-skip-upload | -force-upload] [-rdap-budget N] # sync-geofeeds
//
// Environment: PGSQL (Postgres DSN), PDB_KEY (PeeringDB API key),
// R2 (https://<key-id>:<secret>@<account-id>.r2.cloudflarestorage.com/<bucket>),
// GITHUB_TOKEN, GITHUB_REPO (default shingrus/openipdata.org), HC_PING_ASN
// (optional), OPENIPDATA_DATA_DIR (default /var/lib/openipdata).
// mmdbbuilder is expected next to this binary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	hatchet "github.com/hatchet-dev/hatchet/sdks/go"

	"openipdata.org/hatchet/asn"
	"openipdata.org/hatchet/domains"
	"openipdata.org/hatchet/geofeeds"
	"openipdata.org/hatchet/pdb"
)

// BuildASNInput is the build-asn-data input; cron runs use the zero value.
type BuildASNInput struct {
	// SkipPublish builds everything but uploads nothing (for manual test runs).
	SkipPublish bool `json:"skip_publish"`
}

// The other workflows take no input options.
type (
	SyncPeeringDBInput    struct{}
	UpdateASNDomainsInput struct{}
)

// SyncGeofeedsInput is the sync-geofeeds input; cron runs use the zero value.
type SyncGeofeedsInput struct {
	// SkipUpload writes the rows file but does not upload it (for manual test runs).
	SkipUpload bool `json:"skip_upload"`
	// ForceUpload uploads even if the row count dropped more than 10% (for a
	// manual run after checking that the drop is real).
	ForceUpload bool `json:"force_upload"`
	// RDAPBudget, if positive, caps the RDAP lookups per registry (for test runs).
	RDAPBudget int `json:"rdap_budget"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	var err error
	switch command {
	case "serve":
		err = serve(ctx)
	case "run-asn":
		err = runASN(ctx, os.Args[2:])
	case "run-pdb":
		err = runPDB(ctx)
	case "run-domains":
		err = runDomains(ctx)
	case "run-geofeeds":
		err = runGeofeeds(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q (want serve, run-asn, run-pdb, run-domains, or run-geofeeds)", command)
	}
	if err != nil {
		slog.Error("openipdata-worker failed", "err", err)
		os.Exit(1)
	}
}

// newPipeline reads the environment; progress receives log lines.
func newPipeline(progress func(string)) (*asn.Pipeline, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	repo := env("GITHUB_REPO")
	if repo == "" {
		repo = "shingrus/openipdata.org"
	}
	return &asn.Pipeline{
		DataDir:     dataDir(),
		PGSQL:       pgsql(),
		MMDBBuilder: filepath.Join(filepath.Dir(exe), "mmdbbuilder"),
		Progress:    progress,
		R2:          env("R2"),
		GitHubToken: env("GITHUB_TOKEN"),
		GitHubRepo:  repo,
		HCPing:      env("HC_PING_ASN"),
	}, nil
}

func env(key string) string { return strings.TrimSpace(os.Getenv(key)) }

// newPDBPipeline reads the environment; progress receives log lines.
func newPDBPipeline(progress func(string)) *pdb.Pipeline {
	return &pdb.Pipeline{
		DataDir:  dataDir(),
		PGSQL:    pgsql(),
		APIKey:   env("PDB_KEY"),
		Progress: progress,
	}
}

// newDomainsPipeline reads the environment; progress receives log lines.
func newDomainsPipeline(progress func(string)) *domains.Pipeline {
	return &domains.Pipeline{PGSQL: pgsql(), Progress: progress}
}

// newGeofeedsPipeline reads the environment; progress receives log lines.
func newGeofeedsPipeline(progress func(string)) *geofeeds.Pipeline {
	return &geofeeds.Pipeline{DataDir: dataDir(), Progress: progress, R2: env("R2")}
}

func pgsql() string { return env("PGSQL") }

func dataDir() string {
	if dir := env("OPENIPDATA_DATA_DIR"); dir != "" {
		return dir
	}
	return "/var/lib/openipdata"
}

// taskLog sends a log line to the worker log and to the Hatchet run.
func taskLog(ctx hatchet.Context) func(string) {
	return func(msg string) {
		slog.Info(msg)
		ctx.Log(msg)
	}
}

func cliLog(msg string) { slog.Info(msg) }

func serve(ctx context.Context) error {
	client, err := hatchet.NewClient()
	if err != nil {
		return fmt.Errorf("create Hatchet client: %w", err)
	}
	worker, err := client.NewWorker("openipdata-worker",
		hatchet.WithWorkflows(
			buildASNWorkflow(client),
			syncPeeringDBWorkflow(client),
			updateASNDomainsWorkflow(client),
			syncGeofeedsWorkflow(client),
		),
		hatchet.WithSlots(4),
	)
	if err != nil {
		return fmt.Errorf("create worker: %w", err)
	}
	slog.Info("worker starting")
	return worker.StartBlocking(ctx)
}

// asnDBLimit serializes every workflow that writes the ASN tables.
func asnDBLimit() hatchet.Concurrency { return oneRunLimit("asn-db") }

// oneRunLimit lets one run at a time hold the named limit.
func oneRunLimit(name string) hatchet.Concurrency {
	maxRuns := int32(1)
	strategy := hatchet.GroupRoundRobin
	return hatchet.Concurrency{
		Name:           name,
		IsTenantScoped: true,
		Expression:     "'" + name + "'",
		MaxRuns:        &maxRuns,
		LimitStrategy:  &strategy,
	}
}

// taskRetry is the retry policy shared by the workflow tasks.
func taskRetry(opts ...hatchet.TaskOption) []hatchet.TaskOption {
	return append([]hatchet.TaskOption{hatchet.WithRetries(2), hatchet.WithRetryBackoff(2, 300)}, opts...)
}

func buildASNWorkflow(client *hatchet.Client) *hatchet.Workflow {
	wf := client.NewWorkflow("build-asn-data",
		hatchet.WithWorkflowDescription("ipverse/asn-ip -> public.asn -> ip2asn MMDB + asn.sqlite3"),
		hatchet.WithWorkflowCron("0 6,18 * * *"),
		hatchet.WithWorkflowConcurrency(asnDBLimit()),
	)
	sync := wf.NewTask("sync-source", func(ctx hatchet.Context, input BuildASNInput) (asn.Source, error) {
		p, err := newPipeline(taskLog(ctx))
		if err != nil {
			return asn.Source{}, err
		}
		if !input.SkipPublish {
			// Fail before the build, and without retries: only an env change fixes it.
			if err := p.CheckPublish(); err != nil {
				return asn.Source{}, hatchet.NewNonRetryableError(err)
			}
		}
		return p.SyncSource(ctx, time.Now())
	}, taskRetry(hatchet.WithExecutionTimeout(30*time.Minute))...)

	// withSource returns the pipeline and the snapshot sync-source recorded.
	withSource := func(ctx hatchet.Context) (*asn.Pipeline, asn.Source, error) {
		var src asn.Source
		if err := ctx.ParentOutput(sync, &src); err != nil {
			return nil, src, err
		}
		p, err := newPipeline(taskLog(ctx))
		return p, src, err
	}

	ingest := wf.NewTask("ingest", func(ctx hatchet.Context, _ BuildASNInput) (asn.IngestStats, error) {
		p, src, err := withSource(ctx)
		if err != nil {
			return asn.IngestStats{}, err
		}
		return p.Ingest(ctx, src)
	}, taskRetry(hatchet.WithParents(sync), hatchet.WithExecutionTimeout(30*time.Minute))...)

	mmdb := wf.NewTask("build-mmdb", func(ctx hatchet.Context, _ BuildASNInput) (asn.BuildMMDBOutput, error) {
		p, src, err := withSource(ctx)
		if err != nil {
			return asn.BuildMMDBOutput{}, err
		}
		return p.BuildMMDB(ctx, src)
	}, taskRetry(hatchet.WithParents(sync, ingest), hatchet.WithExecutionTimeout(30*time.Minute))...)

	export := wf.NewTask("export-sqlite", func(ctx hatchet.Context, _ BuildASNInput) (asn.ExportOutput, error) {
		p, src, err := withSource(ctx)
		if err != nil {
			return asn.ExportOutput{}, err
		}
		return p.ExportSQLite(ctx, src)
	}, taskRetry(hatchet.WithParents(sync, ingest), hatchet.WithExecutionTimeout(30*time.Minute))...)

	wf.NewTask("publish", func(ctx hatchet.Context, input BuildASNInput) (asn.PublishOutput, error) {
		if input.SkipPublish {
			taskLog(ctx)("publish: skipped (skip_publish=true)")
			return asn.PublishOutput{Skipped: true}, nil
		}
		p, src, err := withSource(ctx)
		if err != nil {
			return asn.PublishOutput{}, err
		}
		var built asn.BuildMMDBOutput
		if err := ctx.ParentOutput(mmdb, &built); err != nil {
			return asn.PublishOutput{}, err
		}
		var exported asn.ExportOutput
		if err := ctx.ParentOutput(export, &exported); err != nil {
			return asn.PublishOutput{}, err
		}
		return p.Publish(ctx, src, built.Path, exported.Path)
	}, taskRetry(hatchet.WithParents(sync, mmdb, export), hatchet.WithExecutionTimeout(30*time.Minute))...)

	return wf
}

func syncPeeringDBWorkflow(client *hatchet.Client) *hatchet.Workflow {
	wf := client.NewWorkflow("sync-peeringdb",
		hatchet.WithWorkflowDescription("PeeringDB -> per-ASN country, city, website -> public.asn_geo_pdb"),
		hatchet.WithWorkflowCron("0 3 * * *"),
		hatchet.WithWorkflowConcurrency(asnDBLimit()),
	)

	download := wf.NewTask("download", func(ctx hatchet.Context, _ SyncPeeringDBInput) (pdb.DownloadOutput, error) {
		return newPDBPipeline(taskLog(ctx)).Download(ctx)
	}, taskRetry(hatchet.WithExecutionTimeout(time.Hour))...)

	wf.NewTask("compute-and-store", func(ctx hatchet.Context, _ SyncPeeringDBInput) (pdb.StoreStats, error) {
		return newPDBPipeline(taskLog(ctx)).ComputeAndStore(ctx)
	}, taskRetry(hatchet.WithParents(download), hatchet.WithExecutionTimeout(30*time.Minute))...)

	return wf
}

func updateASNDomainsWorkflow(client *hatchet.Client) *hatchet.Workflow {
	wf := client.NewWorkflow("update-asn-domains",
		hatchet.WithWorkflowDescription("public.asn_geo_pdb + RDAP -> public.asn_domain"),
		hatchet.WithWorkflowCron("0 4,16 * * *"),
		hatchet.WithWorkflowConcurrency(asnDBLimit()),
	)

	pdbDomains := wf.NewTask("pdb-domains", func(ctx hatchet.Context, _ UpdateASNDomainsInput) (domains.PDBStats, error) {
		return newDomainsPipeline(taskLog(ctx)).PDBDomains(ctx)
	}, taskRetry(hatchet.WithExecutionTimeout(15*time.Minute))...)

	// Each result is stored as soon as it is known, so a retry resumes where it stopped.
	wf.NewTask("rdap-domains", func(ctx hatchet.Context, _ UpdateASNDomainsInput) (domains.RDAPStats, error) {
		return newDomainsPipeline(taskLog(ctx)).RDAPDomains(ctx)
	}, taskRetry(hatchet.WithParents(pdbDomains), hatchet.WithExecutionTimeout(2*time.Hour))...)

	return wf
}

// syncGeofeedsWorkflow has no schedule yet: it runs only when triggered. The
// URL lists stay on the VM; the checked rows go to the private R2 bucket for
// build-geo-data. Nothing is published.
func syncGeofeedsWorkflow(client *hatchet.Client) *hatchet.Workflow {
	wf := client.NewWorkflow("sync-geofeeds",
		hatchet.WithWorkflowDescription("RDAP + whois + IRR dumps + feed lists -> checked geofeed rows in private R2"),
		// Its own limit, not asn-db: it reads and writes no ASN table.
		hatchet.WithWorkflowConcurrency(oneRunLimit("geofeeds")),
	)

	// Each result is appended as it comes in, so a retry resumes where it stopped.
	rdap := wf.NewTask("rdap-lookups", func(ctx hatchet.Context, input SyncGeofeedsInput) (geofeeds.RDAPOutput, error) {
		p := newGeofeedsPipeline(taskLog(ctx))
		if !input.SkipUpload {
			// Fail before the lookups, and without retries: only an env change fixes it.
			if err := p.CheckUpload(); err != nil {
				return geofeeds.RDAPOutput{}, hatchet.NewNonRetryableError(err)
			}
		}
		return p.LookupRDAP(ctx, input.RDAPBudget)
	}, taskRetry(hatchet.WithExecutionTimeout(3*time.Hour))...)

	discover := wf.NewTask("discover", func(ctx hatchet.Context, _ SyncGeofeedsInput) (geofeeds.DiscoverOutput, error) {
		return newGeofeedsPipeline(taskLog(ctx)).Discover(ctx)
	}, taskRetry(hatchet.WithParents(rdap), hatchet.WithExecutionTimeout(time.Hour))...)

	// Each feed's result is saved as it comes in, so a retry resumes where it stopped.
	check := wf.NewTask("check-feeds", func(ctx hatchet.Context, _ SyncGeofeedsInput) (geofeeds.CheckOutput, error) {
		return newGeofeedsPipeline(taskLog(ctx)).CheckFeeds(ctx)
	}, taskRetry(hatchet.WithParents(discover), hatchet.WithExecutionTimeout(2*time.Hour))...)

	wf.NewTask("export-rows", func(ctx hatchet.Context, input SyncGeofeedsInput) (geofeeds.ExportOutput, error) {
		p := newGeofeedsPipeline(taskLog(ctx))
		if !input.SkipUpload {
			if err := p.CheckUpload(); err != nil {
				return geofeeds.ExportOutput{}, hatchet.NewNonRetryableError(err)
			}
		}
		out, err := p.ExportRows(ctx, !input.SkipUpload, input.ForceUpload)
		if errors.Is(err, geofeeds.ErrGate) {
			err = hatchet.NewNonRetryableError(err)
		}
		return out, err
	}, taskRetry(hatchet.WithParents(check), hatchet.WithExecutionTimeout(time.Hour))...)

	return wf
}

// runASN runs the build-asn-data tasks once, in order, without Hatchet.
func runASN(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("run-asn", flag.ExitOnError)
	skipPublish := flags.Bool("skip-publish", false, "build everything but upload nothing")
	flags.Parse(args)
	if flags.NArg() > 0 {
		return fmt.Errorf("run-asn: unexpected argument %q", flags.Arg(0))
	}

	p, err := newPipeline(cliLog)
	if err != nil {
		return err
	}
	if !*skipPublish {
		if err := p.CheckPublish(); err != nil {
			return err
		}
	}
	src, err := p.SyncSource(ctx, time.Now())
	if err != nil {
		return err
	}
	if _, err := p.Ingest(ctx, src); err != nil {
		return err
	}
	built, err := p.BuildMMDB(ctx, src)
	if err != nil {
		return err
	}
	exported, err := p.ExportSQLite(ctx, src)
	if err != nil {
		return err
	}
	if *skipPublish {
		cliLog("publish: skipped (-skip-publish)")
		return nil
	}
	_, err = p.Publish(ctx, src, built.Path, exported.Path)
	return err
}

// runPDB runs the sync-peeringdb tasks once, in order, without Hatchet.
func runPDB(ctx context.Context) error {
	p := newPDBPipeline(cliLog)
	if _, err := p.Download(ctx); err != nil {
		return err
	}
	_, err := p.ComputeAndStore(ctx)
	return err
}

// runDomains runs the update-asn-domains tasks once, in order, without Hatchet.
func runDomains(ctx context.Context) error {
	p := newDomainsPipeline(cliLog)
	if _, err := p.PDBDomains(ctx); err != nil {
		return err
	}
	_, err := p.RDAPDomains(ctx)
	return err
}

// runGeofeeds runs the sync-geofeeds tasks once, in order, without Hatchet.
func runGeofeeds(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("run-geofeeds", flag.ExitOnError)
	skipUpload := flags.Bool("skip-upload", false, "write the rows file but do not upload it")
	forceUpload := flags.Bool("force-upload", false, "upload even if the row count dropped more than 10%")
	rdapBudget := flags.Int("rdap-budget", 0, "cap the RDAP lookups per registry (0: the default budgets)")
	flags.Parse(args)
	if flags.NArg() > 0 {
		return fmt.Errorf("run-geofeeds: unexpected argument %q", flags.Arg(0))
	}

	p := newGeofeedsPipeline(cliLog)
	if !*skipUpload {
		if err := p.CheckUpload(); err != nil {
			return err
		}
	}
	if _, err := p.LookupRDAP(ctx, *rdapBudget); err != nil {
		return err
	}
	if _, err := p.Discover(ctx); err != nil {
		return err
	}
	if _, err := p.CheckFeeds(ctx); err != nil {
		return err
	}
	_, err := p.ExportRows(ctx, !*skipUpload, *forceUpload)
	return err
}
