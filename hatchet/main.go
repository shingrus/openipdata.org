// Command openipdata-worker runs the openipdata workflows as a Hatchet worker,
// or runs a workflow once locally without Hatchet.
//
//	openipdata-worker version                    # print the build version and exit (no environment needed)
//	openipdata-worker                            # serve: connect to Hatchet (HATCHET_CLIENT_TOKEN)
//	openipdata-worker run-asn [-skip-publish] [-scores-only]    # build-asn-data
//	openipdata-worker run-pdb                    # sync-peeringdb
//	openipdata-worker run-domains                # update-asn-domains
//	openipdata-worker run-geofeeds [-skip-upload | -force-upload] [-rdap-budget N] [-domain-budget N] # sync-geofeeds
//	openipdata-worker run-bgp [-skip-upload | -force-upload] [-mrt file] # sync-bgp (-mrt: read a local dump, no download, no upload)
//	openipdata-worker run-mlab [-sites] [-file path] [-skip-upload | -force-upload] # import-mlab (-file: any local CSV instead of the newest in the inbox); -sites: import-mlab-sites instead (layer 35-speedtest-site, from the sites inbox or -file)
//	openipdata-worker run-hosting-ping [-skip-upload | -force-upload] # sync-hosting-ping (the tasks targets, ping and publish in order; needs GCP_PING_KEY)
//	openipdata-worker run-arin [-skip-upload] [-force-upload] [-force-export] # sync-arin-whois
//	openipdata-worker run-geo [-skip-publish] [-force-publish] # build-geo-data
//	openipdata-worker run-rdns [-force-replace]  # resolve-ips
//	openipdata-worker run-rdns-rules [-reject domain,...] # learn-rdns-rules (task learn)
//	openipdata-worker run-rdns-rules -codes [-domains a,b] [-force] # learn-rdns-rules, only the task propose-codes
//	openipdata-worker run-rdns-rules -hoiho # learn-rdns-rules, only the task import-hoiho
//	openipdata-worker run-rdns-rules -reject-hoiho a.net,b.net # switch off the Hoiho rules of these suffixes and of every name under them
//	openipdata-worker run-rdns-rules -accept-hoiho a.net,b.net # switch them on again
//
// Environment: PGSQL (Postgres DSN; sync-geofeeds keeps its URL list in
// public.geofeed_urls, and its probe-domains task reads public.asn_domain and public.asn;
// resolve-ips reads public.generated_rules, public.rdns_city_rules and public.asn, reads and deletes rows of
// public.visitor_ips (the websites insert them), and writes public.network_traces,
// public.traced_networks, public.ip_ptr_names, public.customer_name_networks and public.rdns_geo_layer;
// import-mlab reads public.generated_rules and public.rdns_city_rules and writes public.mlab_hop_names;
// sync-arin-whois uses no table (its records are files on the VM with a daily copy in R2);
// learn-rdns-rules reads the names of every traced IP (targets, candidates, hops) and ASNs, and writes public.rdns_city_rules
// and public.rdns_llm_answers),
// OPENROUTER_API_KEY (optional, a secret: without it propose-codes is skipped), OPENROUTER_MODEL (default
// openai/gpt-6.1-terra), OPENROUTER_SECOND_MODEL (default openai/gpt-6-luna) and OPENROUTER_FALLBACK_MODEL (default
// anthropic/claude-haiku-5.5, asked when the main model gives no rule; "off" = none),
// PDB_KEY (PeeringDB API key),
// R2 (https://<key-id>:<secret>@<account-id>.r2.cloudflarestorage.com/<bucket>),
// GITHUB_TOKEN (the release uploads; also read by sync-geofeeds, for the GitHub repository search only),
// GITHUB_REPO (default shingrus/openipdata.org), HC_PING_ASN and
// HC_PING_GEO, HC_PING_BGP and HC_PING_HOSTING (optional), GCP_PING_KEY (a secret: a Google Cloud service-account JSON key,
// base64; without it sync-hosting-ping does nothing) and GCP_PING_PROJECT (optional, default the project of the key), MAXMIND_LICENSE_KEY (optional secret: the
// MaxMind GeoLite2 City reference of build-geo-data's evaluate task), OPENIPDATA_DATA_DIR (default /var/lib/openipdata).
// PTR_RESOLVER (optional, host:port of a local recursive resolver on loopback, e.g. 127.0.0.1:5335: the PTR
// lookups of resolve-ips and import-mlab ask it first, then DoH; empty = the system resolver; checked at start),
// mmdbbuilder and scamper (resolve-ips; built without privilege separation) are expected next to this binary.
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

	"openipdata.org/hatchet/arin"
	"openipdata.org/hatchet/asn"
	"openipdata.org/hatchet/bgp"
	"openipdata.org/hatchet/domains"
	"openipdata.org/hatchet/geo"
	"openipdata.org/hatchet/geofeeds"
	"openipdata.org/hatchet/mlab"
	"openipdata.org/hatchet/pdb"
	"openipdata.org/hatchet/ping"
	"openipdata.org/hatchet/rdns"
	"openipdata.org/hatchet/rdnsrules"
)

// BuildASNInput is the build-asn-data input; cron runs use the zero value.
type BuildASNInput struct {
	// SkipPublish builds everything but uploads nothing (for manual test runs).
	SkipPublish bool `json:"skip_publish"`
	// ScoresOnly is the light run, started by the publish task of build-geo-data: it
	// refreshes only the geo_scores table of the last full build's asn.sqlite3 and publishes
	// only that file (no git fetch, no Postgres writes, no MMDB, no health-check ping).
	// Cron runs use the zero value, a full run.
	ScoresOnly bool `json:"scores_only,omitempty"`
}

// The other workflows take no input options.
type (
	SyncPeeringDBInput    struct{}
	UpdateASNDomainsInput struct{}
)

// SyncGeofeedsInput is the sync-geofeeds input; cron runs use the zero value.
type SyncGeofeedsInput struct {
	// SkipUpload writes the rows files but does not upload them (for manual test runs).
	SkipUpload bool `json:"skip_upload"`
	// ForceUpload uploads even if the row count dropped more than 10% (for a
	// manual run after checking that the drop is real); it also lets the whois
	// geoloc rows go up with fewer than the first-upload minimum.
	ForceUpload bool `json:"force_upload"`
	// RDAPBudget, if positive, caps the RDAP lookups per registry (for test runs).
	RDAPBudget int `json:"rdap_budget"`
	// DomainBudget, if positive, sets how many ASN website domains probe-domains
	// probes per run (default 5000).
	DomainBudget int `json:"domain_budget"`
}

// SyncBGPInput is the sync-bgp input; cron runs use the zero value.
type SyncBGPInput struct {
	// SkipUpload writes the communities file but does not upload it (for manual test runs).
	SkipUpload bool `json:"skip_upload"`
	// ForceUpload sends the bytes even when R2 already holds the same file.
	ForceUpload bool `json:"force_upload"`
}

// ImportMLabInput is the import-mlab input; there is no cron, the workflow is started by hand.
type ImportMLabInput struct {
	// File is the base name of a .csv or .csv.gz file in the inbox (/var/lib/openipdata/mlab/inbox);
	// empty means the newest one there.
	File string `json:"file"`
	// ForceUpload sends the layer even when R2 already holds the same bytes.
	ForceUpload bool `json:"force_upload"`
}

// ImportMLabSitesInput is the import-mlab-sites input; there is no cron, the workflow is started by hand.
type ImportMLabSitesInput struct {
	// File is the base name of a .csv or .csv.gz file in the sites inbox
	// (/var/lib/openipdata/mlab/sites-inbox); empty means the newest one there, and with no file
	// the task does nothing.
	File string `json:"file"`
	// ForceUpload sends the layer even when R2 already holds the same bytes.
	ForceUpload bool `json:"force_upload"`
}

// SyncHostingPingInput is the sync-hosting-ping input; cron runs use the zero value.
type SyncHostingPingInput struct {
	// ForceUpload sends the file even when R2 already holds the same bytes.
	ForceUpload bool `json:"force_upload"`
}

// SyncARINWhoisInput is the sync-arin-whois input; cron runs use the zero value.
type SyncARINWhoisInput struct {
	// ForceUpload sends the layer even when R2 already holds the same bytes, and copies the records
	// file to R2 now instead of once a day (a file with less than half the records of the last copy
	// is still refused).
	ForceUpload bool `json:"force_upload"`
	// ForceExport writes the layer even when the file is younger than six hours.
	ForceExport bool `json:"force_export"`
}

// BuildGeoInput is the build-geo-data input; cron runs use the zero value.
type BuildGeoInput struct {
	// SkipPublish builds and evaluates the MMDB but uploads nothing (for manual test runs).
	SkipPublish bool `json:"skip_publish"`
	// ForcePublish publishes even if the scores dropped more than the gate allows
	// (for a manual run after checking that the drop is real).
	ForcePublish bool `json:"force_publish"`
	// SkipLayers leaves these optional layer files out of the build and the holdout build,
	// e.g. ["40-hosting.csv.gz"]; the optional layers are 25-bgp, 35-rdns-wide, 35-speedtest-site, 36-mlab, 37-whois-arin, 37-whois-text, 38-asn-geofeed, 39-github-geofeeds, 40-hosting, 50-ix, 60-whois,
	// 65-rdns, 67-rdns-names and 68-cloud-regions (for manual test runs).
	SkipLayers []string `json:"skip_layers"`
}

// ResolveIPsInput is the resolve-ips input; cron runs use the zero value.
type ResolveIPsInput struct {
	// ForceReplace replaces public.rdns_geo_layer even if it would have more than 10% fewer
	// rows than now (for a manual run after checking that the drop is real).
	ForceReplace bool `json:"force_replace"`
}

// LearnRDNSRulesInput defaults to learning. RejectDomains withdraws current
// versions only, without source downloads or learning.
type LearnRDNSRulesInput struct {
	RejectDomains []string `json:"reject_domains"`
	// RejectHoihoDomains switches off the Hoiho rules of these suffixes and of every name under
	// them; AcceptHoihoDomains switches them on again (a domain in both ends up accepted). A run with
	// either one only does that (and reject_domains, when given too): nothing is learned, imported
	// or asked.
	RejectHoihoDomains []string `json:"reject_hoiho_domains"`
	AcceptHoihoDomains []string `json:"accept_hoiho_domains"`
	// Domains restricts the questions of propose-codes to these registered domains.
	Domains []string `json:"domains"`
	// Force makes propose-codes ask again whatever its record says.
	Force bool `json:"force"`
}

// switchesHoiho reports a run that only switches Hoiho suffixes off or on.
func (in LearnRDNSRulesInput) switchesHoiho() bool {
	return len(in.RejectHoihoDomains) > 0 || len(in.AcceptHoihoDomains) > 0
}

// version is set at build time (-ldflags "-X main.version=..."); "dev" for a plain go build.
var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if command == "version" {
		// Needs no environment: the updater runs it as an unprivileged user.
		fmt.Println(version)
		return
	}
	var err error
	if command == "serve" || command == "run-rdns" || command == "run-mlab" {
		err = rdns.CheckPTRResolver(env("PTR_RESOLVER"))
	}
	if err != nil {
		slog.Error("openipdata-worker failed", "err", err)
		os.Exit(1)
	}
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
	case "run-bgp":
		err = runBGP(ctx, os.Args[2:])
	case "run-mlab":
		err = runMLab(ctx, os.Args[2:])
	case "run-hosting-ping":
		err = runHostingPing(ctx, os.Args[2:])
	case "run-arin":
		err = runARIN(ctx, os.Args[2:])
	case "run-geo":
		err = runGeo(ctx, os.Args[2:])
	case "run-rdns":
		err = runRDNS(ctx, os.Args[2:])
	case "run-rdns-rules":
		err = runRDNSRules(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q (want serve, version, run-asn, run-pdb, run-domains, run-geofeeds, run-bgp, run-mlab, run-hosting-ping, run-arin, run-geo, run-rdns, or run-rdns-rules)", command)
	}
	if err != nil {
		slog.Error("openipdata-worker failed", "err", err)
		os.Exit(1)
	}
}

// newPipeline reads the environment; progress receives log lines.
func newPipeline(progress func(string)) (*asn.Pipeline, error) {
	builder, err := mmdbBuilder()
	if err != nil {
		return nil, err
	}
	return &asn.Pipeline{
		DataDir:     dataDir(),
		PGSQL:       pgsql(),
		MMDBBuilder: builder,
		Progress:    progress,
		R2:          env("R2"),
		GitHubToken: env("GITHUB_TOKEN"),
		GitHubRepo:  githubRepo(),
		HCPing:      env("HC_PING_ASN"),
	}, nil
}

// newBGPPipeline reads the environment; progress receives log lines.
func newBGPPipeline(progress func(string)) *bgp.Pipeline {
	return &bgp.Pipeline{DataDir: dataDir(), Progress: progress, R2: env("R2"), HCPing: env("HC_PING_BGP")}
}

// newMLabPipeline reads the environment; progress receives log lines. mmdbbuilder is the binary
// next to this one (empty when unknown: locate then skips the MMDB part).
func newMLabPipeline(progress func(string)) *mlab.Pipeline {
	builder, _ := mmdbBuilder()
	return &mlab.Pipeline{DataDir: dataDir(), PGSQL: pgsql(), Progress: progress, MMDBBuilder: builder, R2: env("R2"), PTRResolver: env("PTR_RESOLVER")}
}

// newPingPipeline reads the environment; progress receives log lines.
func newPingPipeline(progress func(string)) *ping.Pipeline {
	return &ping.Pipeline{
		DataDir: dataDir(), Progress: progress, R2: env("R2"), PGSQL: pgsql(),
		GCPKey: env("GCP_PING_KEY"), GCPProject: env("GCP_PING_PROJECT"), HCPing: env("HC_PING_HOSTING"),
	}
}

// newARINPipeline reads the environment; progress receives log lines. mmdbbuilder is the binary
// next to this one (empty when unknown: lookup then skips, it needs the gap list).
func newARINPipeline(progress func(string)) *arin.Pipeline {
	builder, _ := mmdbBuilder()
	return &arin.Pipeline{DataDir: dataDir(), Progress: progress, MMDBBuilder: builder, R2: env("R2")}
}

// newGeoPipeline reads the environment; progress receives log lines.
func newGeoPipeline(progress func(string)) (*geo.Pipeline, error) {
	builder, err := mmdbBuilder()
	if err != nil {
		return nil, err
	}
	return &geo.Pipeline{
		DataDir:     dataDir(),
		PGSQL:       pgsql(),
		MMDBBuilder: builder,
		Progress:    progress,
		R2:          env("R2"),
		GitHubToken: env("GITHUB_TOKEN"),
		GitHubRepo:  githubRepo(),
		HCPing:      env("HC_PING_GEO"),
		MaxMindKey:  env("MAXMIND_LICENSE_KEY"),
	}, nil
}

// mmdbBuilder is the mmdbbuilder binary next to this one.
func mmdbBuilder() (string, error) { return nextToExecutable("mmdbbuilder") }

// nextToExecutable is the path of a helper binary in the directory of this one.
func nextToExecutable(name string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exe), name), nil
}

func githubRepo() string {
	if repo := env("GITHUB_REPO"); repo != "" {
		return repo
	}
	return "shingrus/openipdata.org"
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
	return &geofeeds.Pipeline{DataDir: dataDir(), Progress: progress, R2: env("R2"), PGSQL: pgsql(), GitHubToken: env("GITHUB_TOKEN")}
}

// newRDNSPipeline reads the environment; progress receives log lines. scamper and
// mmdbbuilder are the binaries next to this one, never one from the PATH (an empty path, when the
// executable is unknown, makes the trace task fail and locate skip the MMDB hop fallback).
func newRDNSPipeline(progress func(string)) *rdns.Pipeline {
	scamper, _ := nextToExecutable("scamper")
	builder, _ := mmdbBuilder() // empty when unknown: locate then skips the MMDB hop fallback
	return &rdns.Pipeline{DataDir: dataDir(), PGSQL: pgsql(), Progress: progress, Scamper: scamper, MMDBBuilder: builder, R2: env("R2"), PTRResolver: env("PTR_RESOLVER")}
}

func newRDNSRulesPipeline(progress func(string)) *rdnsrules.Pipeline {
	model := env("OPENROUTER_MODEL")
	if model == "" {
		model = rdnsrules.DefaultModel
	}
	second := env("OPENROUTER_SECOND_MODEL")
	if second == "" {
		second = rdnsrules.DefaultSecondModel
	}
	return &rdnsrules.Pipeline{DataDir: dataDir(), PGSQL: pgsql(), Progress: progress,
		OpenRouterKey: env("OPENROUTER_API_KEY"), OpenRouterModel: model, OpenRouterSecondModel: second,
		OpenRouterFallbackModel: env("OPENROUTER_FALLBACK_MODEL")} // empty = default, "off" = none
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
			syncBGPWorkflow(client),
			importMLabWorkflow(client),
			importMLabSitesWorkflow(client),
			syncHostingPingWorkflow(client),
			syncARINWhoisWorkflow(client),
			buildGeoWorkflow(client),
			resolveIPsWorkflow(client),
			learnRDNSRulesWorkflow(client),
		),
		hatchet.WithSlots(4),
	)
	if err != nil {
		return fmt.Errorf("create worker: %w", err)
	}
	// The marker file tells the updater that a task runs; a leftover of a killed process goes first.
	busy := newBusyMarker(dataDir())
	busy.reset()
	defer busy.reset()
	worker.Use(busy.middleware)
	slog.Info("worker starting", "version", version)
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
	return append([]hatchet.TaskOption{hatchet.WithRetries(taskRetries), hatchet.WithRetryBackoff(2, 300)}, opts...)
}

// taskRetries is the number of retries taskRetry gives a task (sync-hosting-ping pings its
// health check's /fail only on the last attempt).
const taskRetries = 2

// buildASNName is the name of the build-asn-data workflow (the publish task of build-geo-data starts a run by name).
const buildASNName = "build-asn-data"

// build-asn-data runs from cron at 06:00 and 18:00 UTC (a full build). After each
// successful build-geo-data publish it is also started with scores_only: the light run
// refreshes only the geo_scores table of the last full build's asn.sqlite3 and publishes
// only that file. It is the one exception to "output files are rebuilt from scratch": it
// rewrites one table of an existing file. It shares the asn-db limit, so it never overlaps
// a full run.
func buildASNWorkflow(client *hatchet.Client) *hatchet.Workflow {
	wf := client.NewWorkflow(buildASNName,
		hatchet.WithWorkflowDescription("ipverse/asn-ip -> public.asn -> ip2asn MMDB + asn.sqlite3 (also reads public.asn_domain, public.asn_geo_pdb and the published rows of public.geo_eval, last 60 days, into the geo_scores table); with scores_only (started after each build-geo-data publish) only the geo_scores table of the last build's asn.sqlite3 is rewritten and published"),
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
		if input.ScoresOnly {
			return p.SyncScoresOnly(), nil
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
		hatchet.WithWorkflowDescription("PeeringDB -> per-ASN country, city, website -> public.asn_geo_pdb; exchange prefixes -> public.geo_ix_pdb"),
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

// syncGeofeedsWorkflow runs daily, ahead of build-geo-data. The URL list is
// public.geofeed_urls; the URL lists written for people stay on the VM; the checked rows go to the private R2 bucket for build-geo-data.
// Nothing is published.
func syncGeofeedsWorkflow(client *hatchet.Client) *hatchet.Workflow {
	wf := client.NewWorkflow("sync-geofeeds",
		hatchet.WithWorkflowDescription("RDAP + whois + IRR dumps + feed lists + ASN website probes + GitHub repositories -> checked geofeed rows, whois geoloc rows and whois text rows in private R2"),
		hatchet.WithWorkflowCron("0 0 * * *"),
		// Its own limit, not asn-db: it reads and writes no ASN table.
		hatchet.WithWorkflowConcurrency(oneRunLimit("geofeeds")),
	)

	// rdap-lookups has no child: it runs beside discover -> check-feeds -> export-rows
	// (about 40 minutes), so the rows do not wait for the RDAP pass (up to 2 hours).
	// discover reads whatever rdap.jsonl holds when it gets there; results found in
	// this run reach the rows in the next one. The run ends when all tasks end.
	// Each result is appended as it comes in, so a retry resumes where it stopped.
	// probe-domains and search-github are the same: no parent, no child, results for the next run.
	wf.NewTask("rdap-lookups", func(ctx hatchet.Context, input SyncGeofeedsInput) (geofeeds.RDAPOutput, error) {
		p := newGeofeedsPipeline(taskLog(ctx))
		if !input.SkipUpload {
			// It runs for up to 2 hours: a bad R2 setting must stop the run now, not after it.
			if err := p.CheckUpload(); err != nil {
				return geofeeds.RDAPOutput{}, hatchet.NewNonRetryableError(err)
			}
		}
		return p.LookupRDAP(ctx, input.RDAPBudget)
	}, taskRetry(hatchet.WithExecutionTimeout(3*time.Hour))...)

	// Looks for feeds that whois does not point to, on the websites of the ASNs
	// (public.asn_domain, read only).
	wf.NewTask("probe-domains", func(ctx hatchet.Context, input SyncGeofeedsInput) (geofeeds.DomainsOutput, error) {
		p := newGeofeedsPipeline(taskLog(ctx))
		// Only an env change fixes either setting: no retries.
		if err := p.CheckPGSQL(); err != nil {
			return geofeeds.DomainsOutput{}, hatchet.NewNonRetryableError(err)
		}
		if !input.SkipUpload {
			if err := p.CheckUpload(); err != nil {
				return geofeeds.DomainsOutput{}, hatchet.NewNonRetryableError(err)
			}
		}
		return p.ProbeDomains(ctx, input.DomainBudget)
	}, taskRetry(hatchet.WithExecutionTimeout(3*time.Hour))...)

	// Looks for geofeed files in public GitHub repositories (no database; the token is optional).
	// It never fails for a GitHub problem, only for a file error.
	wf.NewTask("search-github", func(ctx hatchet.Context, _ SyncGeofeedsInput) (geofeeds.GitHubOutput, error) {
		return newGeofeedsPipeline(taskLog(ctx)).SearchGitHub(ctx)
	}, taskRetry(hatchet.WithExecutionTimeout(time.Hour))...)

	discover := wf.NewTask("discover", func(ctx hatchet.Context, input SyncGeofeedsInput) (geofeeds.DiscoverOutput, error) {
		p := newGeofeedsPipeline(taskLog(ctx))
		// Start of the chain that ends in the upload: fail early, and
		// without retries, since only an env change fixes it.
		if err := p.CheckPGSQL(); err != nil {
			return geofeeds.DiscoverOutput{}, hatchet.NewNonRetryableError(err)
		}
		if !input.SkipUpload {
			if err := p.CheckUpload(); err != nil {
				return geofeeds.DiscoverOutput{}, hatchet.NewNonRetryableError(err)
			}
		}
		return p.Discover(ctx)
	}, taskRetry(hatchet.WithExecutionTimeout(time.Hour))...)

	// Each feed's result is saved as it comes in, so a retry resumes where it stopped.
	check := wf.NewTask("check-feeds", func(ctx hatchet.Context, _ SyncGeofeedsInput) (geofeeds.CheckOutput, error) {
		p := newGeofeedsPipeline(taskLog(ctx))
		if err := p.CheckPGSQL(); err != nil {
			return geofeeds.CheckOutput{}, hatchet.NewNonRetryableError(err)
		}
		return p.CheckFeeds(ctx)
	}, taskRetry(hatchet.WithParents(discover), hatchet.WithExecutionTimeout(2*time.Hour))...)

	wf.NewTask("export-rows", func(ctx hatchet.Context, input SyncGeofeedsInput) (geofeeds.ExportOutput, error) {
		p := newGeofeedsPipeline(taskLog(ctx))
		if err := p.CheckPGSQL(); err != nil {
			return geofeeds.ExportOutput{}, hatchet.NewNonRetryableError(err)
		}
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

// syncBGPWorkflow downloads a RIPE RIS routing table, reads the BGP community values of
// every IPv4 prefix and puts them in the private R2 bucket for build-geo-data (layer
// 25-bgp). It runs before build-geo-data and reads no database.
func syncBGPWorkflow(client *hatchet.Client) *hatchet.Workflow {
	wf := client.NewWorkflow("sync-bgp",
		hatchet.WithWorkflowDescription("RIPE RIS routing table -> BGP community values per IPv4 prefix in private R2"),
		hatchet.WithWorkflowCron("40 3 * * *"),
		hatchet.WithWorkflowConcurrency(oneRunLimit("bgp")),
	)
	download := wf.NewTask("download", func(ctx hatchet.Context, input SyncBGPInput) (bgp.DownloadOutput, error) {
		p := newBGPPipeline(taskLog(ctx))
		if !input.SkipUpload {
			// Only an env change fixes it: fail before the 430 MB download, without retries.
			if err := p.CheckUpload(); err != nil {
				return bgp.DownloadOutput{}, hatchet.NewNonRetryableError(err)
			}
		}
		return p.Download(ctx)
	}, taskRetry(hatchet.WithExecutionTimeout(2*time.Hour))...)
	extract := wf.NewTask("extract", func(ctx hatchet.Context, _ SyncBGPInput) (bgp.ExtractOutput, error) {
		out, err := newBGPPipeline(taskLog(ctx)).Extract(ctx, "")
		// A retry of this task alone cannot help: a small table stays small, and a damaged
		// dump was deleted (the next run downloads a new one).
		if errors.Is(err, bgp.ErrTooFewPrefixes) || errors.Is(err, bgp.ErrDamaged) {
			err = hatchet.NewNonRetryableError(err)
		}
		return out, err
	}, taskRetry(hatchet.WithParents(download), hatchet.WithExecutionTimeout(time.Hour))...)
	wf.NewTask("upload", func(ctx hatchet.Context, input SyncBGPInput) (bgp.UploadOutput, error) {
		if input.SkipUpload {
			taskLog(ctx)("upload: skipped (skip_upload)")
			return bgp.UploadOutput{}, nil
		}
		return newBGPPipeline(taskLog(ctx)).Upload(ctx, input.ForceUpload)
	}, taskRetry(hatchet.WithParents(extract), hatchet.WithExecutionTimeout(30*time.Minute))...)
	return wf
}

// importMLabWorkflow turns the M-Lab traceroute export the owner copied into the inbox into the
// ip2geo layer 36-mlab (the city of the router hops before each client /24) in private R2, where
// build-geo-data reads it. There is no cron.
func importMLabWorkflow(client *hatchet.Client) *hatchet.Workflow {
	wf := client.NewWorkflow("import-mlab",
		hatchet.WithWorkflowDescription("M-Lab hop export (inbox) -> PTR names of the hops (public.mlab_hop_names) -> layer 36-mlab in private R2; started by hand"),
		hatchet.WithWorkflowConcurrency(oneRunLimit("mlab")),
	)
	// A file or setting problem is fixed by the owner, not by a retry.
	permanent := func(err error) error {
		if errors.Is(err, mlab.ErrPermanent) {
			return hatchet.NewNonRetryableError(err)
		}
		return err
	}
	imp := wf.NewTask("import", func(ctx hatchet.Context, input ImportMLabInput) (mlab.ImportOutput, error) {
		p := newMLabPipeline(taskLog(ctx))
		if err := p.CheckPGSQL(); err != nil {
			return mlab.ImportOutput{}, hatchet.NewNonRetryableError(err)
		}
		if err := p.CheckUpload(); err != nil {
			return mlab.ImportOutput{}, hatchet.NewNonRetryableError(err)
		}
		out, err := p.Import(ctx, input.File)
		return out, permanent(err)
	}, taskRetry(hatchet.WithExecutionTimeout(time.Hour))...)
	names := wf.NewTask("resolve-names", func(ctx hatchet.Context, _ ImportMLabInput) (mlab.NamesOutput, error) {
		out, err := newMLabPipeline(taskLog(ctx)).ResolveNames(ctx)
		return out, permanent(err)
	}, taskRetry(hatchet.WithParents(imp), hatchet.WithExecutionTimeout(3*time.Hour+15*time.Minute))...)
	locate := wf.NewTask("locate", func(ctx hatchet.Context, _ ImportMLabInput) (mlab.LocateOutput, error) {
		out, err := newMLabPipeline(taskLog(ctx)).Locate(ctx)
		return out, permanent(err)
	}, taskRetry(hatchet.WithParents(names), hatchet.WithExecutionTimeout(time.Hour))...)
	wf.NewTask("upload", func(ctx hatchet.Context, input ImportMLabInput) (mlab.UploadOutput, error) {
		return newMLabPipeline(taskLog(ctx)).Upload(ctx, input.ForceUpload)
	}, taskRetry(hatchet.WithParents(locate), hatchet.WithExecutionTimeout(30*time.Minute))...)
	return wf
}

// importMLabSitesWorkflow turns the M-Lab reply time export the owner copied into the sites inbox
// (the lowest reply time between M-Lab's speed test servers and each client /24) into the ip2geo
// layer 35-speedtest-site in private R2, where build-geo-data reads it. It is its own workflow, so
// a new reply time export does not run the hop tasks of import-mlab again. There is no cron.
func importMLabSitesWorkflow(client *hatchet.Client) *hatchet.Workflow {
	wf := client.NewWorkflow("import-mlab-sites",
		hatchet.WithWorkflowDescription("M-Lab reply time export (sites inbox) -> layer 35-speedtest-site in private R2; started by hand"),
		hatchet.WithWorkflowConcurrency(oneRunLimit("mlab-sites")),
	)
	wf.NewTask("site-layer", func(ctx hatchet.Context, input ImportMLabSitesInput) (mlab.SiteLayerOutput, error) {
		p := newMLabPipeline(taskLog(ctx))
		if err := p.CheckUpload(); err != nil {
			return mlab.SiteLayerOutput{}, hatchet.NewNonRetryableError(err)
		}
		out, err := p.SiteLayer(ctx, input.File, mlab.SiteLayerOptions{ForceUpload: input.ForceUpload})
		// A file or setting problem is fixed by the owner, not by a retry.
		if errors.Is(err, mlab.ErrPermanent) {
			err = hatchet.NewNonRetryableError(err)
		}
		return out, err
	}, taskRetry(hatchet.WithExecutionTimeout(30*time.Minute))...)
	return wf
}

// syncHostingPingWorkflow pings the .1 of every /24 of the hosting networks from small Google
// Cloud VMs in about 27 regions, once a month, and uploads the lowest times to private R2, where
// build-geo-data reads them for the speed-of-light country check (-ping-country). The tasks:
// targets (the address list, from public.asn), ping (the VMs) and publish (merge, check, upload).
// Without GCP_PING_KEY every task skips.
func syncHostingPingWorkflow(client *hatchet.Client) *hatchet.Workflow {
	wf := client.NewWorkflow("sync-hosting-ping",
		hatchet.WithWorkflowDescription("hosting networks -> ping times from Google Cloud regions -> hosting-rtt.csv.gz in private R2; monthly"),
		hatchet.WithWorkflowCron("0 2 1 * *"),
		hatchet.WithWorkflowConcurrency(oneRunLimit("hosting-ping")),
	)
	// A setting problem is fixed by the owner, not by a retry; a failed run pings the health check's /fail.
	fixed := func(p *ping.Pipeline, ctx hatchet.Context, err error) error {
		if err == nil {
			return nil
		}
		final := errors.Is(err, ping.ErrPermanent) || errors.Is(err, ping.ErrInvalidGCPKey)
		// /fail only when no retry follows (a non-retryable error or the last attempt), so a
		// failure that a retry fixes does not alert.
		if ctx.Err() == nil && (final || ctx.RetryCount() >= taskRetries) {
			p.Fail(ctx)
		}
		if final {
			return hatchet.NewNonRetryableError(err)
		}
		return err
	}
	targets := wf.NewTask("targets", func(ctx hatchet.Context, _ SyncHostingPingInput) (ping.TargetsOutput, error) {
		p := newPingPipeline(taskLog(ctx))
		if err := p.CheckSettings(true); err != nil {
			return ping.TargetsOutput{}, hatchet.NewNonRetryableError(err)
		}
		out, err := p.Targets(ctx)
		return out, fixed(p, ctx, err)
	}, taskRetry(hatchet.WithExecutionTimeout(time.Hour))...)
	pinged := wf.NewTask("ping", func(ctx hatchet.Context, _ SyncHostingPingInput) (ping.PingOutput, error) {
		p := newPingPipeline(taskLog(ctx))
		if err := p.CheckSettings(false); err != nil {
			return ping.PingOutput{}, hatchet.NewNonRetryableError(err)
		}
		out, err := p.Ping(ctx)
		return out, fixed(p, ctx, err)
	}, taskRetry(hatchet.WithParents(targets), hatchet.WithExecutionTimeout(4*time.Hour))...)
	wf.NewTask("publish", func(ctx hatchet.Context, input SyncHostingPingInput) (ping.PublishOutput, error) {
		p := newPingPipeline(taskLog(ctx))
		if err := p.CheckSettings(false); err != nil {
			return ping.PublishOutput{}, hatchet.NewNonRetryableError(err)
		}
		out, err := p.Publish(ctx, ping.PublishOptions{ForceUpload: input.ForceUpload})
		return out, fixed(p, ctx, err)
	}, taskRetry(hatchet.WithParents(pinged), hatchet.WithExecutionTimeout(time.Hour))...)
	return wf
}

// syncARINWhoisWorkflow asks ARIN's RDAP service about the gap /24s in ARIN's space (two lookups a
// second, a batch at a time), keeps the records in a file on the VM (copied to private R2 once a day) and
// writes the ip2geo layer 37-whois-arin to private R2, where build-geo-data reads it.
func syncARINWhoisWorkflow(client *hatchet.Client) *hatchet.Workflow {
	wf := client.NewWorkflow("sync-arin-whois",
		hatchet.WithWorkflowDescription("ARIN RDAP lookups for the gap /24s (records file on the VM, daily copy in private R2) -> layer 37-whois-arin in private R2"),
		// Every 30 minutes: a lookup run lasts at most 24 minutes, the next one goes on from the saved position.
		hatchet.WithWorkflowCron("*/30 * * * *"),
		hatchet.WithWorkflowConcurrency(oneRunLimit("arin")),
	)
	lookup := wf.NewTask("lookup", func(ctx hatchet.Context, _ SyncARINWhoisInput) (arin.LookupOutput, error) {
		p := newARINPipeline(taskLog(ctx))
		if err := p.CheckUpload(); err != nil {
			return arin.LookupOutput{}, hatchet.NewNonRetryableError(err)
		}
		return p.Lookup(ctx)
	}, taskRetry(hatchet.WithExecutionTimeout(30*time.Minute))...)
	export := wf.NewTask("export", func(ctx hatchet.Context, input SyncARINWhoisInput) (arin.ExportOutput, error) {
		return newARINPipeline(taskLog(ctx)).Export(ctx, input.ForceExport)
	}, taskRetry(hatchet.WithParents(lookup), hatchet.WithExecutionTimeout(15*time.Minute))...)
	wf.NewTask("upload", func(ctx hatchet.Context, input SyncARINWhoisInput) (arin.UploadOutput, error) {
		return newARINPipeline(taskLog(ctx)).Upload(ctx, input.ForceUpload)
	}, taskRetry(hatchet.WithParents(export), hatchet.WithExecutionTimeout(15*time.Minute))...)
	return wf
}

func buildGeoWorkflow(client *hatchet.Client) *hatchet.Workflow {
	wf := client.NewWorkflow("build-geo-data",
		hatchet.WithWorkflowDescription("RIR stats + geo-ip-blocks + public.asn_geo_pdb + public.geo_ix_pdb + whois geoloc rows (R2) + whois text rows (R2) + geofeed rows (R2) -> ip2geo MMDB, scored against DB-IP, MaxMind GeoLite2 and RIPE Atlas (country, city), and ours also on a geofeed holdout; a successful publish starts a build-asn-data scores-only run (refreshes only the geo_scores table)"),
		// After build-asn-data (06:00, 18:00) and sync-peeringdb (03:00), whose tables it reads.
		hatchet.WithWorkflowCron("0 7,19 * * *"),
		// Its own limit, not asn-db: it only reads the ASN tables.
		hatchet.WithWorkflowConcurrency(oneRunLimit("geo")),
	)

	download := wf.NewTask("download", func(ctx hatchet.Context, input BuildGeoInput) (geo.Source, error) {
		p, err := newGeoPipeline(taskLog(ctx))
		if err != nil {
			return geo.Source{}, err
		}
		// Fail before the downloads, and without retries: only an env change fixes it.
		if err := p.CheckSettings(!input.SkipPublish); err != nil {
			return geo.Source{}, hatchet.NewNonRetryableError(err)
		}
		if err := geo.ValidateSkipLayers(input.SkipLayers); err != nil {
			return geo.Source{}, hatchet.NewNonRetryableError(err)
		}
		src, err := p.Download(ctx, time.Now())
		if err != nil {
			return src, err
		}
		if err := p.SkipLayers(&src, input.SkipLayers); err != nil {
			return src, err
		}
		return src, nil
	}, taskRetry(hatchet.WithExecutionTimeout(time.Hour))...)

	// withSource returns the pipeline and what download prepared.
	withSource := func(ctx hatchet.Context) (*geo.Pipeline, geo.Source, error) {
		var src geo.Source
		if err := ctx.ParentOutput(download, &src); err != nil {
			return nil, src, err
		}
		p, err := newGeoPipeline(taskLog(ctx))
		return p, src, err
	}

	mmdb := wf.NewTask("build-mmdb", func(ctx hatchet.Context, _ BuildGeoInput) (geo.BuildMMDBOutput, error) {
		p, src, err := withSource(ctx)
		if err != nil {
			return geo.BuildMMDBOutput{}, err
		}
		return p.BuildMMDB(ctx, src)
	}, taskRetry(hatchet.WithParents(download), hatchet.WithExecutionTimeout(time.Hour))...)

	// The scores go to public.geo_eval; publish refuses a database that scores
	// clearly worse than the last published one.
	eval := wf.NewTask("evaluate", func(ctx hatchet.Context, _ BuildGeoInput) (geo.EvalOutput, error) {
		p, src, err := withSource(ctx)
		if err != nil {
			return geo.EvalOutput{}, err
		}
		var built geo.BuildMMDBOutput
		if err := ctx.ParentOutput(mmdb, &built); err != nil {
			return geo.EvalOutput{}, err
		}
		return p.Evaluate(ctx, src, built.Path)
	}, taskRetry(hatchet.WithParents(download, mmdb), hatchet.WithExecutionTimeout(time.Hour))...)

	wf.NewTask("publish", func(ctx hatchet.Context, input BuildGeoInput) (geo.PublishOutput, error) {
		if input.SkipPublish {
			taskLog(ctx)("publish: skipped (skip_publish=true)")
			return geo.PublishOutput{Skipped: true}, nil
		}
		p, src, err := withSource(ctx)
		if err != nil {
			return geo.PublishOutput{}, err
		}
		var built geo.BuildMMDBOutput
		if err := ctx.ParentOutput(mmdb, &built); err != nil {
			return geo.PublishOutput{}, err
		}
		var scores geo.EvalOutput
		if err := ctx.ParentOutput(eval, &scores); err != nil {
			return geo.PublishOutput{}, err
		}
		out, err := p.Publish(ctx, src, built.Path, scores, input.ForcePublish)
		if errors.Is(err, geo.ErrGate) {
			err = hatchet.NewNonRetryableError(err)
		}
		if err == nil {
			// The light run refreshes only the geo_scores table of asn.sqlite3; the new
			// top-level run is queued by the asn-db limit if a build-asn-data run is going.
			startASNRun(&out, func() error {
				_, err := client.RunNoWait(ctx.GetContext(), buildASNName, BuildASNInput{ScoresOnly: true})
				return err
			}, taskLog(ctx))
		}
		return out, err
	}, taskRetry(hatchet.WithParents(download, mmdb, eval), hatchet.WithExecutionTimeout(45*time.Minute))...)

	return wf
}

// startASNRun starts a scores-only build-asn-data run through trigger after a publish
// that put files in place (not a skipped output) and records the result in out.ASNRun. A
// trigger that fails is logged (the error is the Hatchet client's and holds no secret) and
// never fails the task: the next full build-asn-data run refreshes the table anyway.
func startASNRun(out *geo.PublishOutput, trigger func() error, log func(string)) {
	if out.Skipped {
		return
	}
	if err := trigger(); err != nil {
		log("publish: could not start a build-asn-data scores-only run: " + err.Error())
		out.ASNRun = "failed"
		return
	}
	log("publish: a build-asn-data scores-only run is queued")
	out.ASNRun = "started"
}

// resolveIPsName is the name of the resolve-ips workflow (its last task starts the next run by name).
const resolveIPsName = "resolve-ips"

func learnRDNSRulesWorkflow(client *hatchet.Client) *hatchet.Workflow {
	wf := client.NewWorkflow("learn-rdns-rules",
		hatchet.WithWorkflowDescription("traced IP PTR names + public place dictionaries -> public.rdns_city_rules (up to 3 learned candidate rules per domain; propose-codes adds an LLM rule as rank 4 and records its questions in public.rdns_llm_answers; import-hoiho imports the router-name rules of CAIDA's Hoiho into public.rdns_hoiho_rules)"),
		hatchet.WithWorkflowCron("0 5 * * *"),
		hatchet.WithWorkflowConcurrency(oneRunLimit("rdns-rules")),
	)
	learn := wf.NewTask("learn", func(ctx hatchet.Context, input LearnRDNSRulesInput) (rdnsrules.Output, error) {
		p := newRDNSRulesPipeline(taskLog(ctx))
		if err := p.CheckPGSQL(); err != nil {
			return rdnsrules.Output{}, hatchet.NewNonRetryableError(err)
		}
		if len(input.RejectDomains) > 0 {
			return p.Reject(ctx, input.RejectDomains)
		}
		if input.switchesHoiho() {
			return rdnsrules.Output{Skipped: "hoiho_domains"}, nil
		}
		return p.Learn(ctx)
	}, taskRetry(hatchet.WithExecutionTimeout(time.Hour))...)
	// The imported Hoiho rules. The tasks share the dictionary cache files (cities1000.zip and
	// admin1CodesASCII.txt are fetched through the same .tmp name), so they run one after the other:
	// learn, import-hoiho, propose-codes. A file or GeoNames problem only skips the import; only a
	// database error fails the task.
	importHoiho := wf.NewTask("import-hoiho", func(ctx hatchet.Context, input LearnRDNSRulesInput) (rdnsrules.HoihoOutput, error) {
		if !input.switchesHoiho() && len(input.RejectDomains) > 0 {
			return rdnsrules.HoihoOutput{Skipped: "reject_domains"}, nil
		}
		p := newRDNSRulesPipeline(taskLog(ctx))
		if err := p.CheckPGSQL(); err != nil {
			return rdnsrules.HoihoOutput{}, hatchet.NewNonRetryableError(err)
		}
		if input.switchesHoiho() {
			// reject_hoiho_domains / accept_hoiho_domains: only the switch, no import.
			out := rdnsrules.HoihoOutput{Skipped: "hoiho_domains"}
			var err error
			out.Rejection, out.Acceptance, err = p.SwitchHoiho(ctx, input.RejectHoihoDomains, input.AcceptHoihoDomains)
			if errors.Is(err, rdnsrules.ErrHoihoDomain) {
				err = hatchet.NewNonRetryableError(err)
			}
			return out, err
		}
		return p.ImportHoiho(ctx)
	}, taskRetry(hatchet.WithParents(learn), hatchet.WithExecutionTimeout(30*time.Minute))...)
	// The LLM questions run after import-hoiho. API trouble never fails the task (it is counted); only
	// a database error and an invalid key do, and an invalid key is not retried.
	wf.NewTask("propose-codes", func(ctx hatchet.Context, input LearnRDNSRulesInput) (rdnsrules.ProposeOutput, error) {
		if len(input.RejectDomains) > 0 {
			return rdnsrules.ProposeOutput{Skipped: "reject_domains"}, nil
		}
		if input.switchesHoiho() {
			return rdnsrules.ProposeOutput{Skipped: "hoiho_domains"}, nil
		}
		p := newRDNSRulesPipeline(taskLog(ctx))
		if err := p.CheckPGSQL(); err != nil {
			return rdnsrules.ProposeOutput{}, hatchet.NewNonRetryableError(err)
		}
		out, err := p.ProposeCodes(ctx, rdnsrules.ProposeOptions{Domains: input.Domains, Force: input.Force})
		if errors.Is(err, rdnsrules.ErrInvalidKey) {
			err = hatchet.NewNonRetryableError(err)
		}
		return out, err
	}, taskRetry(hatchet.WithParents(importHoiho), hatchet.WithExecutionTimeout(8*time.Hour))...)
	return wf
}

// resolveIPsWorkflow traces visitor /24s and turns router names into cities.
// Visitor IPs stay in Postgres (and the batch file on the VM): every task
// returns counts only.
func resolveIPsWorkflow(client *hatchet.Client) *hatchet.Workflow {
	wf := client.NewWorkflow(resolveIPsName,
		hatchet.WithWorkflowDescription("public.visitor_ips -> scamper traces -> PTR names -> public.network_traces, public.traced_networks, public.ip_ptr_names, public.customer_name_networks, public.rdns_geo_layer (reads public.generated_rules and public.asn; deletes visitor IPs it no longer needs)"),
		hatchet.WithWorkflowCron("*/30 * * * *"),
		// Its own limit: it writes no ASN table, and a run must not overlap the next one.
		hatchet.WithWorkflowConcurrency(oneRunLimit("rdns")),
	)

	pick := wf.NewTask("pick-networks", func(ctx hatchet.Context, _ ResolveIPsInput) (rdns.PickOutput, error) {
		p := newRDNSPipeline(taskLog(ctx))
		// Only an env change fixes it: no retries.
		if err := p.CheckPGSQL(); err != nil {
			return rdns.PickOutput{}, hatchet.NewNonRetryableError(err)
		}
		return p.PickNetworks(ctx)
	}, taskRetry(hatchet.WithExecutionTimeout(10*time.Minute))...)

	trace := wf.NewTask("trace", func(ctx hatchet.Context, _ ResolveIPsInput) (rdns.TraceOutput, error) {
		return newRDNSPipeline(taskLog(ctx)).Trace(ctx)
	}, taskRetry(hatchet.WithParents(pick), hatchet.WithExecutionTimeout(time.Hour))...)

	names := wf.NewTask("resolve-names", func(ctx hatchet.Context, _ ResolveIPsInput) (rdns.NamesOutput, error) {
		return newRDNSPipeline(taskLog(ctx)).ResolveNames(ctx)
	}, taskRetry(hatchet.WithParents(trace), hatchet.WithExecutionTimeout(30*time.Minute))...)

	// Customer PTR names of gap /24s (counts only). A DNS problem never fails it.
	sample := wf.NewTask("sample-names", func(ctx hatchet.Context, _ ResolveIPsInput) (rdns.SampleOutput, error) {
		return newRDNSPipeline(taskLog(ctx)).SampleNames(ctx)
	}, taskRetry(hatchet.WithParents(names), hatchet.WithExecutionTimeout(30*time.Minute))...)

	wf.NewTask("locate", func(ctx hatchet.Context, input ResolveIPsInput) (rdns.LocateOutput, error) {
		log := taskLog(ctx)
		out, err := newRDNSPipeline(log).Locate(ctx, input.ForceReplace)
		if errors.Is(err, rdns.ErrShrink) {
			err = hatchet.NewNonRetryableError(err)
		}
		if err != nil {
			return out, err
		}
		// Every task of this run succeeded: a run that picked its full limit starts the next
		// one at once. The run is a new top-level one (not a child of this run), so the
		// "rdns" limit queues it behind this one.
		var pickOut rdns.PickOutput
		var traceOut rdns.TraceOutput
		if err := ctx.ParentOutput(pick, &pickOut); err != nil {
			log("locate: could not read the pick-networks output, no chaining")
		} else if err := ctx.ParentOutput(trace, &traceOut); err != nil {
			log("locate: could not read the trace output, no chaining")
		} else {
			out.Chained = chainNext(pickOut, traceOut, func() error {
				_, err := client.RunNoWait(ctx.GetContext(), resolveIPsName, ResolveIPsInput{})
				return err
			}, log)
		}
		return out, nil
	}, taskRetry(hatchet.WithParents(names, sample, pick, trace), hatchet.WithExecutionTimeout(30*time.Minute))...)

	return wf
}

// chainNext starts the next resolve-ips run through trigger when the finished run
// picked its full limit and stored traces (rdns.ShouldChain). A trigger that fails is
// logged (the error is the Hatchet client's and holds no IP) and the run still succeeds:
// its rows are stored, and the cron starts the next run anyway.
func chainNext(pick rdns.PickOutput, trace rdns.TraceOutput, trigger func() error, log func(string)) bool {
	if !rdns.ShouldChain(pick, trace) {
		return false
	}
	if err := trigger(); err != nil {
		log("locate: could not start the next resolve-ips run: " + err.Error())
		return false
	}
	log("locate: the run picked its full limit, the next resolve-ips run is queued")
	return true
}

// runASN runs the build-asn-data tasks once, in order, without Hatchet.
func runASN(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("run-asn", flag.ExitOnError)
	skipPublish := flags.Bool("skip-publish", false, "build everything but upload nothing")
	scoresOnly := flags.Bool("scores-only", false, "only refresh the geo_scores table of the last build's asn.sqlite3 and publish that file")
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
	if *scoresOnly {
		src := p.SyncScoresOnly()
		exported, err := p.ExportSQLite(ctx, src)
		if err != nil {
			return err
		}
		if *skipPublish {
			cliLog("publish: skipped (-skip-publish)")
			return nil
		}
		_, err = p.Publish(ctx, src, "", exported.Path)
		return err
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

// runGeofeeds runs the sync-geofeeds tasks once, without Hatchet: rdap-lookups and
// probe-domains beside the discover -> check-feeds -> export-rows chain, after search-github.
func runGeofeeds(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("run-geofeeds", flag.ExitOnError)
	skipUpload := flags.Bool("skip-upload", false, "write the rows file but do not upload it")
	forceUpload := flags.Bool("force-upload", false, "upload even if the row count dropped more than 10%")
	rdapBudget := flags.Int("rdap-budget", 0, "cap the RDAP lookups per registry (0: the default budgets)")
	domainBudget := flags.Int("domain-budget", 0, "cap the ASN website domains probed (0: the default budget)")
	flags.Parse(args)
	if flags.NArg() > 0 {
		return fmt.Errorf("run-geofeeds: unexpected argument %q", flags.Arg(0))
	}

	p := newGeofeedsPipeline(cliLog)
	if err := p.CheckPGSQL(); err != nil {
		return err
	}
	if !*skipUpload {
		if err := p.CheckUpload(); err != nil {
			return err
		}
	}
	// The GitHub search comes before discover, which reads what it saved. A problem is
	// printed and the run goes on.
	_, githubErr := p.SearchGitHub(ctx)
	if githubErr != nil {
		cliLog(fmt.Sprintf("geofeeds: search-github: %v", githubErr))
	}
	// Like the workflow: the RDAP lookups and the domain probes run beside discover ->
	// check-feeds -> export-rows, which use the results saved so far. Wait for all
	// and report every error.
	rdapDone, domainsDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := p.LookupRDAP(ctx, *rdapBudget)
		rdapDone <- err
	}()
	go func() {
		_, err := p.ProbeDomains(ctx, *domainBudget)
		domainsDone <- err
	}()
	_, err := p.Discover(ctx)
	if err == nil {
		_, err = p.CheckFeeds(ctx)
	}
	if err == nil {
		_, err = p.ExportRows(ctx, !*skipUpload, *forceUpload)
	}
	if err != nil {
		cliLog(fmt.Sprintf("geofeeds: %v; waiting for the RDAP lookups and domain probes to finish", err))
	}
	return errors.Join(githubErr, err, <-rdapDone, <-domainsDone)
}

// runBGP runs the sync-bgp tasks once, in order, without Hatchet. With -mrt it reads a
// local dump and uploads nothing.
func runBGP(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("run-bgp", flag.ExitOnError)
	skipUpload := flags.Bool("skip-upload", false, "write the communities file but do not upload it")
	forceUpload := flags.Bool("force-upload", false, "send the file even if R2 already holds the same bytes")
	mrt := flags.String("mrt", "", "read this local gzipped MRT dump instead of downloading one; nothing is uploaded")
	flags.Parse(args)
	if flags.NArg() > 0 {
		return fmt.Errorf("run-bgp: unexpected argument %q", flags.Arg(0))
	}

	p := newBGPPipeline(cliLog)
	local := *mrt != ""
	if !local && !*skipUpload {
		if err := p.CheckUpload(); err != nil {
			return err
		}
	}
	if !local {
		if _, err := p.Download(ctx); err != nil {
			return err
		}
	}
	if _, err := p.Extract(ctx, *mrt); err != nil {
		return err
	}
	if local || *skipUpload {
		cliLog("upload: skipped")
		return nil
	}
	_, err := p.Upload(ctx, *forceUpload)
	return err
}

// runMLab runs the import-mlab tasks once, in order, without Hatchet. With -file it imports that
// local CSV (any path); otherwise the newest file of the inbox. With -sites it runs the task of
// import-mlab-sites instead (-file then names its local CSV, the default is the newest file of the
// sites inbox).
func runMLab(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("run-mlab", flag.ExitOnError)
	file := flags.String("file", "", "import this local CSV (plain or .gz) instead of the newest file in the inbox")
	sites := flags.Bool("sites", false, "run only the site layer (35-speedtest-site) from the reply time export (-file: its local CSV, else the newest file in the sites inbox)")
	skipUpload := flags.Bool("skip-upload", false, "write the layer file but do not upload it")
	forceUpload := flags.Bool("force-upload", false, "send the layer even if R2 already holds the same bytes")
	flags.Parse(args)
	if flags.NArg() > 0 {
		return fmt.Errorf("run-mlab: unexpected argument %q", flags.Arg(0))
	}

	p := newMLabPipeline(cliLog)
	if *sites {
		return runMLabSites(ctx, p, *file, *skipUpload, *forceUpload)
	}
	if err := p.CheckPGSQL(); err != nil {
		return err
	}
	if !*skipUpload {
		if err := p.CheckUpload(); err != nil {
			return err
		}
	}
	var err error
	if *file != "" {
		_, err = p.ImportFile(ctx, *file)
	} else {
		_, err = p.Import(ctx, "")
	}
	if err != nil {
		return err
	}
	if _, err := p.ResolveNames(ctx); err != nil {
		return err
	}
	if _, err := p.Locate(ctx); err != nil {
		return err
	}
	if *skipUpload {
		cliLog("upload: skipped")
		return nil
	}
	_, err = p.Upload(ctx, *forceUpload)
	return err
}

// runMLabSites runs the site-layer task of import-mlab-sites (run-mlab -sites): no database is
// needed. -file names the local CSV; without it the newest file of the sites inbox is used.
func runMLabSites(ctx context.Context, p *mlab.Pipeline, file string, skipUpload, forceUpload bool) error {
	if !skipUpload {
		if err := p.CheckUpload(); err != nil {
			return err
		}
	}
	_, err := p.SiteLayer(ctx, "", mlab.SiteLayerOptions{Path: file, SkipUpload: skipUpload, ForceUpload: forceUpload})
	return err
}

// runHostingPing runs the three tasks of sync-hosting-ping once, in order, without Hatchet. It
// needs PGSQL, R2 and GCP_PING_KEY; without the key every task skips. -skip-upload merges and
// checks the file but does not upload it (and leaves the objects of the run in R2).
// Never run it while the Hatchet run is in progress: its targets task deletes that run's VMs and objects.
func runHostingPing(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("run-hosting-ping", flag.ExitOnError)
	skipUpload := flags.Bool("skip-upload", false, "merge and check the file but do not upload it")
	forceUpload := flags.Bool("force-upload", false, "send the file even if R2 already holds the same bytes")
	flags.Parse(args)
	if flags.NArg() > 0 {
		return fmt.Errorf("run-hosting-ping: unexpected argument %q", flags.Arg(0))
	}
	if *skipUpload && *forceUpload {
		return errors.New("run-hosting-ping: -skip-upload and -force-upload cannot be combined")
	}
	p := newPingPipeline(cliLog)
	if err := p.CheckSettings(true); err != nil {
		return err
	}
	if _, err := p.Targets(ctx); err != nil {
		p.Fail(ctx)
		return err
	}
	if _, err := p.Ping(ctx); err != nil {
		p.Fail(ctx)
		return err
	}
	if _, err := p.Publish(ctx, ping.PublishOptions{SkipUpload: *skipUpload, ForceUpload: *forceUpload}); err != nil {
		p.Fail(ctx)
		return err
	}
	return nil
}

// runARIN runs the sync-arin-whois tasks once, in order, without Hatchet. The lookup task works for
// at most 24 minutes, then the export and the upload follow.
func runARIN(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("run-arin", flag.ExitOnError)
	skipUpload := flags.Bool("skip-upload", false, "write the layer file but do not upload it (the daily copy of the records file to R2 is skipped too)")
	forceUpload := flags.Bool("force-upload", false, "send the layer even if R2 already holds the same bytes, and copy the records file to R2 now")
	forceExport := flags.Bool("force-export", false, "write the layer even if the file is younger than six hours")
	flags.Parse(args)
	if flags.NArg() > 0 {
		return fmt.Errorf("run-arin: unexpected argument %q", flags.Arg(0))
	}

	p := newARINPipeline(cliLog)
	// The lookups need the R2 setting (the published MMDB is read from it, the records are copied to it).
	if err := p.CheckUpload(); err != nil {
		return err
	}
	if _, err := p.Lookup(ctx); err != nil {
		return err
	}
	if _, err := p.Export(ctx, *forceExport); err != nil {
		return err
	}
	if *skipUpload {
		cliLog("upload: skipped")
		return nil
	}
	_, err := p.Upload(ctx, *forceUpload)
	return err
}

// runGeo runs the build-geo-data tasks once, in order, without Hatchet.
func runGeo(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("run-geo", flag.ExitOnError)
	skipPublish := flags.Bool("skip-publish", false, "build and evaluate the MMDB but upload nothing")
	forcePublish := flags.Bool("force-publish", false, "publish even if the scores dropped more than the gate allows")
	flags.Parse(args)
	if flags.NArg() > 0 {
		return fmt.Errorf("run-geo: unexpected argument %q", flags.Arg(0))
	}

	p, err := newGeoPipeline(cliLog)
	if err != nil {
		return err
	}
	if err := p.CheckSettings(!*skipPublish); err != nil {
		return err
	}
	src, err := p.Download(ctx, time.Now())
	if err != nil {
		return err
	}
	built, err := p.BuildMMDB(ctx, src)
	if err != nil {
		return err
	}
	scores, err := p.Evaluate(ctx, src, built.Path)
	if err != nil {
		return err
	}
	if *skipPublish {
		cliLog("publish: skipped (-skip-publish)")
		return nil
	}
	_, err = p.Publish(ctx, src, built.Path, scores, *forcePublish)
	return err
}

// runRDNS runs the resolve-ips tasks once, in order, without Hatchet.
func runRDNS(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("run-rdns", flag.ExitOnError)
	forceReplace := flags.Bool("force-replace", false, "replace rdns_geo_layer even if it would have more than 10% fewer rows")
	flags.Parse(args)
	if flags.NArg() > 0 {
		return fmt.Errorf("run-rdns: unexpected argument %q", flags.Arg(0))
	}

	p := newRDNSPipeline(cliLog)
	if err := p.CheckPGSQL(); err != nil {
		return err
	}
	if _, err := p.PickNetworks(ctx); err != nil {
		return err
	}
	if _, err := p.Trace(ctx); err != nil {
		return err
	}
	if _, err := p.ResolveNames(ctx); err != nil {
		return err
	}
	if _, err := p.SampleNames(ctx); err != nil {
		return err
	}
	_, err := p.Locate(ctx, *forceReplace)
	return err
}

func runRDNSRules(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("run-rdns-rules", flag.ContinueOnError)
	reject := flags.String("reject", "", "withdraw current rules for comma-separated registered domains, without learning")
	codes := flags.Bool("codes", false, "run only propose-codes (the LLM questions), without learning")
	domains := flags.String("domains", "", "with -codes: ask only these comma-separated registered domains")
	force := flags.Bool("force", false, "with -codes: ask again whatever the record says")
	hoiho := flags.Bool("hoiho", false, "run only import-hoiho (the router-name rules of CAIDA's Hoiho), without learning")
	rejectHoiho := flags.String("reject-hoiho", "", "switch off the Hoiho rules of these comma-separated suffixes and of every name under them")
	acceptHoiho := flags.String("accept-hoiho", "", "switch the Hoiho rules of these comma-separated suffixes on again")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return errors.New("run-rdns-rules: unexpected argument")
	}
	p := newRDNSRulesPipeline(cliLog)
	if err := p.CheckPGSQL(); err != nil {
		return err
	}
	var rejection bool
	given, setHoiho := 0, ""
	flags.Visit(func(f *flag.Flag) {
		rejection = rejection || f.Name == "reject"
		if f.Name == "reject-hoiho" || f.Name == "accept-hoiho" {
			setHoiho = f.Name
		}
		given++
	})
	if setHoiho != "" {
		if given != 1 {
			return errors.New("run-rdns-rules: -reject-hoiho and -accept-hoiho cannot be combined with any other flag")
		}
		list := *rejectHoiho
		if setHoiho == "accept-hoiho" {
			list = *acceptHoiho
		}
		_, err := p.SetHoihoRejected(ctx, strings.Split(list, ","), setHoiho == "reject-hoiho")
		return err
	}
	if !*codes && (*domains != "" || *force) {
		return errors.New("run-rdns-rules: -domains and -force need -codes")
	}
	if *hoiho {
		if rejection || *codes || *domains != "" || *force {
			return errors.New("run-rdns-rules: -hoiho cannot be combined with -reject, -codes, -domains or -force")
		}
		_, err := p.ImportHoiho(ctx)
		return err
	}
	if *codes {
		if rejection {
			return errors.New("run-rdns-rules: -codes and -reject cannot be combined")
		}
		var list []string
		for _, d := range strings.Split(*domains, ",") {
			if d = strings.TrimSpace(d); d != "" {
				list = append(list, d)
			}
		}
		_, err := p.ProposeCodes(ctx, rdnsrules.ProposeOptions{Domains: list, Force: *force})
		return err
	}
	if rejection {
		_, err := p.Reject(ctx, strings.Split(*reject, ","))
		return err
	}
	_, err := p.Learn(ctx)
	return err
}
