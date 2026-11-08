// Command openipdata-worker runs the openipdata workflows as a Hatchet worker,
// or runs a workflow once locally without Hatchet.
//
//	openipdata-worker                       # serve: connect to Hatchet (HATCHET_CLIENT_TOKEN)
//	openipdata-worker run-asn [-skip-domains]
//	openipdata-worker run-pdb
//
// Environment: PGSQL (Postgres DSN), PDB_KEY (PeeringDB API key),
// OPENIPDATA_DATA_DIR (default /var/lib/openipdata).
// mmdbbuilder is expected next to this binary.
package main

import (
	"context"
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
	"openipdata.org/hatchet/pdb"
)

// BuildASNInput is the workflow input; cron runs use the zero value.
type BuildASNInput struct {
	// SkipDomains skips the RDAP/PeeringDB domain lookups (for manual test runs).
	SkipDomains bool `json:"skip_domains"`
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
	default:
		err = fmt.Errorf("unknown command %q (want serve, run-asn, or run-pdb)", command)
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
		PGSQL:       strings.TrimSpace(os.Getenv("PGSQL")),
		MMDBBuilder: filepath.Join(filepath.Dir(exe), "mmdbbuilder"),
		Progress:    progress,
	}, nil
}

// newPDBPipeline reads the environment; progress receives log lines.
func newPDBPipeline(progress func(string)) *pdb.Pipeline {
	return &pdb.Pipeline{
		DataDir:  dataDir(),
		PGSQL:    strings.TrimSpace(os.Getenv("PGSQL")),
		APIKey:   strings.TrimSpace(os.Getenv("PDB_KEY")),
		Progress: progress,
	}
}

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

func serve(ctx context.Context) error {
	client, err := hatchet.NewClient()
	if err != nil {
		return fmt.Errorf("create Hatchet client: %w", err)
	}
	worker, err := client.NewWorker("openipdata-worker",
		hatchet.WithWorkflows(buildASNWorkflow(client), syncPeeringDBWorkflow(client)),
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

func pipelineFor(ctx hatchet.Context) (*asn.Pipeline, error) {
	return newPipeline(taskLog(ctx))
}

// taskRetry is the retry policy shared by the workflow tasks.
func taskRetry(opts ...hatchet.TaskOption) []hatchet.TaskOption {
	return append([]hatchet.TaskOption{hatchet.WithRetries(2), hatchet.WithRetryBackoff(2, 300)}, opts...)
}

func buildASNWorkflow(client *hatchet.Client) *hatchet.Workflow {
	wf := client.NewWorkflow("build-asn-data",
		hatchet.WithWorkflowDescription("ipverse/asn-ip -> Postgres -> ip2asn MMDB + asn.sqlite3"),
		hatchet.WithWorkflowCron("15 */12 * * *"),
		hatchet.WithWorkflowConcurrency(asnDBLimit()),
	)
	sync := wf.NewTask("sync-source", func(ctx hatchet.Context, _ BuildASNInput) (asn.Source, error) {
		p, err := pipelineFor(ctx)
		if err != nil {
			return asn.Source{}, err
		}
		return p.SyncSource(ctx, time.Now())
	}, taskRetry(hatchet.WithExecutionTimeout(30*time.Minute))...)

	source := func(ctx hatchet.Context) (asn.Source, error) {
		var src asn.Source
		err := ctx.ParentOutput(sync, &src)
		return src, err
	}

	ingest := wf.NewTask("ingest", func(ctx hatchet.Context, _ BuildASNInput) (asn.IngestStats, error) {
		src, err := source(ctx)
		if err != nil {
			return asn.IngestStats{}, err
		}
		p, err := pipelineFor(ctx)
		if err != nil {
			return asn.IngestStats{}, err
		}
		return p.Ingest(ctx, src)
	}, taskRetry(hatchet.WithParents(sync), hatchet.WithExecutionTimeout(30*time.Minute))...)

	// The first run is ~1 day of ARIN lookups; later runs take minutes. Each
	// result is written to Postgres immediately, so a retry resumes where it stopped.
	domains := wf.NewTask("domains", func(ctx hatchet.Context, input BuildASNInput) (asn.DomainStats, error) {
		p, err := pipelineFor(ctx)
		if err != nil {
			return asn.DomainStats{}, err
		}
		if input.SkipDomains {
			p.Progress("domains: skipped (skip_domains=true)")
			return asn.DomainStats{}, nil
		}
		return p.Domains(ctx)
	}, taskRetry(hatchet.WithParents(ingest), hatchet.WithExecutionTimeout(24*time.Hour))...)

	wf.NewTask("build-mmdb", func(ctx hatchet.Context, _ BuildASNInput) (asn.BuildMMDBOutput, error) {
		src, err := source(ctx)
		if err != nil {
			return asn.BuildMMDBOutput{}, err
		}
		p, err := pipelineFor(ctx)
		if err != nil {
			return asn.BuildMMDBOutput{}, err
		}
		return p.BuildMMDB(ctx, src)
	}, taskRetry(hatchet.WithParents(sync, ingest), hatchet.WithExecutionTimeout(30*time.Minute))...)

	wf.NewTask("export-sqlite", func(ctx hatchet.Context, _ BuildASNInput) (asn.ExportOutput, error) {
		src, err := source(ctx)
		if err != nil {
			return asn.ExportOutput{}, err
		}
		p, err := pipelineFor(ctx)
		if err != nil {
			return asn.ExportOutput{}, err
		}
		return p.ExportSQLite(ctx, src)
	}, taskRetry(hatchet.WithParents(sync, domains), hatchet.WithExecutionTimeout(30*time.Minute))...)

	return wf
}

// SyncPeeringDBInput is the workflow input; it has no options yet.
type SyncPeeringDBInput struct{}

func syncPeeringDBWorkflow(client *hatchet.Client) *hatchet.Workflow {
	wf := client.NewWorkflow("sync-peeringdb",
		hatchet.WithWorkflowDescription("PeeringDB -> per-ASN country, city, domain -> public.asn_geo_pdb"),
		hatchet.WithWorkflowCron("0 22 * * *"),
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

// runPDB runs the PeeringDB tasks once, in order, without Hatchet.
func runPDB(ctx context.Context) error {
	p := newPDBPipeline(func(msg string) { slog.Info(msg) })
	if _, err := p.Download(ctx); err != nil {
		return err
	}
	_, err := p.ComputeAndStore(ctx)
	return err
}

// runASN runs the same tasks once, in order, without Hatchet.
func runASN(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("run-asn", flag.ExitOnError)
	skipDomains := flags.Bool("skip-domains", false, "skip RDAP/PeeringDB domain lookups")
	flags.Parse(args)

	p, err := newPipeline(func(msg string) { slog.Info(msg) })
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
	if !*skipDomains {
		if _, err := p.Domains(ctx); err != nil {
			return err
		}
	}
	if _, err := p.BuildMMDB(ctx, src); err != nil {
		return err
	}
	_, err = p.ExportSQLite(ctx, src)
	return err
}
