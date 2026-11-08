// Command openipdata-worker runs the openipdata workflows as a Hatchet worker,
// or runs a workflow once locally without Hatchet.
//
//	openipdata-worker              # serve: connect to Hatchet (HATCHET_CLIENT_TOKEN)
//	openipdata-worker run-asn      # build-asn-data
//	openipdata-worker run-pdb      # sync-peeringdb
//	openipdata-worker run-domains  # update-asn-domains
//
// Environment: PGSQL (Postgres DSN), PDB_KEY (PeeringDB API key),
// OPENIPDATA_DATA_DIR (default /var/lib/openipdata).
// mmdbbuilder is expected next to this binary.
package main

import (
	"context"
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
	"openipdata.org/hatchet/pdb"
)

// The workflows take no input options; cron runs and manual runs do the same.
type (
	BuildASNInput         struct{}
	SyncPeeringDBInput    struct{}
	UpdateASNDomainsInput struct{}
)

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
		err = runASN(ctx)
	case "run-pdb":
		err = runPDB(ctx)
	case "run-domains":
		err = runDomains(ctx)
	default:
		err = fmt.Errorf("unknown command %q (want serve, run-asn, run-pdb, or run-domains)", command)
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
	return &asn.Pipeline{
		DataDir:     dataDir(),
		PGSQL:       pgsql(),
		MMDBBuilder: filepath.Join(filepath.Dir(exe), "mmdbbuilder"),
		Progress:    progress,
	}, nil
}

// newPDBPipeline reads the environment; progress receives log lines.
func newPDBPipeline(progress func(string)) *pdb.Pipeline {
	return &pdb.Pipeline{
		DataDir:  dataDir(),
		PGSQL:    pgsql(),
		APIKey:   strings.TrimSpace(os.Getenv("PDB_KEY")),
		Progress: progress,
	}
}

// newDomainsPipeline reads the environment; progress receives log lines.
func newDomainsPipeline(progress func(string)) *domains.Pipeline {
	return &domains.Pipeline{PGSQL: pgsql(), Progress: progress}
}

func pgsql() string { return strings.TrimSpace(os.Getenv("PGSQL")) }

func dataDir() string {
	if dir := strings.TrimSpace(os.Getenv("OPENIPDATA_DATA_DIR")); dir != "" {
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
func asnDBLimit() hatchet.Concurrency {
	maxRuns := int32(1)
	strategy := hatchet.GroupRoundRobin
	return hatchet.Concurrency{
		Name:           "asn-db",
		IsTenantScoped: true,
		Expression:     "'asn-db'",
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
	sync := wf.NewTask("sync-source", func(ctx hatchet.Context, _ BuildASNInput) (asn.Source, error) {
		p, err := newPipeline(taskLog(ctx))
		if err != nil {
			return asn.Source{}, err
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

	wf.NewTask("build-mmdb", func(ctx hatchet.Context, _ BuildASNInput) (asn.BuildMMDBOutput, error) {
		p, src, err := withSource(ctx)
		if err != nil {
			return asn.BuildMMDBOutput{}, err
		}
		return p.BuildMMDB(ctx, src)
	}, taskRetry(hatchet.WithParents(sync, ingest), hatchet.WithExecutionTimeout(30*time.Minute))...)

	wf.NewTask("export-sqlite", func(ctx hatchet.Context, _ BuildASNInput) (asn.ExportOutput, error) {
		p, src, err := withSource(ctx)
		if err != nil {
			return asn.ExportOutput{}, err
		}
		return p.ExportSQLite(ctx, src)
	}, taskRetry(hatchet.WithParents(sync, ingest), hatchet.WithExecutionTimeout(30*time.Minute))...)

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

// runASN runs the build-asn-data tasks once, in order, without Hatchet.
func runASN(ctx context.Context) error {
	p, err := newPipeline(cliLog)
	if err != nil {
		return err
	}
	src, err := p.SyncSource(ctx, time.Now())
	if err != nil {
		return err
	}
	if _, err := p.Ingest(ctx, src); err != nil {
		return err
	}
	if _, err := p.BuildMMDB(ctx, src); err != nil {
		return err
	}
	_, err = p.ExportSQLite(ctx, src)
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
