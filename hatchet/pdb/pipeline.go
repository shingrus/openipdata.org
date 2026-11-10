package pdb

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Pipeline runs the PeeringDB sync. Each method is one task; the Hatchet
// workflow and the local run-pdb command call them in the same order.
type Pipeline struct {
	DataDir  string
	PGSQL    string
	APIKey   string
	Progress func(string)
}

func (p *Pipeline) dir() string { return filepath.Join(p.DataDir, "pdb") }

func (p *Pipeline) log(format string, args ...any) {
	if p.Progress != nil {
		p.Progress(fmt.Sprintf(format, args...))
	}
}

// DownloadOutput counts the downloaded rows per dataset and the API requests.
type DownloadOutput struct {
	Rows            map[string]int `json:"rows"`
	Requests        int            `json:"requests"`
	RateLimited     int            `json:"rate_limited"` // HTTP 429 answers
	DurationSeconds float64        `json:"duration_seconds"`
}

// Download fetches every dataset fresh into <data>/pdb/<name>.jsonl.
func (p *Pipeline) Download(ctx context.Context) (DownloadOutput, error) {
	started := time.Now()
	out := DownloadOutput{Rows: map[string]int{}}
	if p.APIKey == "" {
		return out, fmt.Errorf("PDB_KEY is not set")
	}
	if err := os.MkdirAll(p.dir(), 0o750); err != nil {
		return out, err
	}
	c := &client{base: apiBase, apiKey: p.APIKey, http: &http.Client{Timeout: 60 * time.Second}, log: p.log}
	for i, ds := range datasets {
		p.log("download: [%d/%d] %s", i+1, len(datasets), ds.name)
		rows, err := c.downloadDataset(ctx, ds.name, ds.fields, datasetPath(p.dir(), ds.name))
		if err != nil {
			return out, err
		}
		p.log("download: %s: %d rows", ds.name, rows)
		out.Rows[ds.name] = rows
	}
	out.Requests, out.RateLimited = c.requests, c.rateLimited
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	p.log("download: done in %s, %d requests, %d rate limited", time.Since(started).Round(time.Second), out.Requests, out.RateLimited)
	return out, nil
}

// ComputeAndStore derives the per-ASN rows and the exchange prefix rows and
// replaces public.asn_geo_pdb and public.geo_ix_pdb in one transaction.
func (p *Pipeline) ComputeAndStore(ctx context.Context) (StoreStats, error) {
	if p.PGSQL == "" {
		return StoreStats{}, fmt.Errorf("PGSQL is not set")
	}
	rows, err := Compute(p.dir())
	if err != nil {
		return StoreStats{}, err
	}
	withCity, withDomain := 0, 0
	for _, r := range rows {
		if r.City != nil {
			withCity++
		}
		if r.Domain != nil {
			withDomain++
		}
	}
	p.log("compute: %d ASNs (%d with city, %d with domain)", len(rows), withCity, withDomain)
	ixRows, err := ComputeIX(p.dir())
	if err != nil {
		return StoreStats{}, err
	}
	p.log("compute: %d exchange prefixes", len(ixRows))
	stats, err := Replace(ctx, p.PGSQL, rows, ixRows)
	stats.WithCity, stats.WithDomain = withCity, withDomain
	if err != nil {
		return stats, err
	}
	if stats.IXKept != "" {
		p.log("store: replaced asn_geo_pdb (%d rows before, %d now); WARNING kept the old geo_ix_pdb (%d rows): %s",
			stats.Previous, stats.Rows, stats.IXPrevious, stats.IXKept)
		return stats, nil
	}
	p.log("store: replaced asn_geo_pdb (%d rows before, %d now) and geo_ix_pdb (%d before, %d now)",
		stats.Previous, stats.Rows, stats.IXPrevious, stats.IXRows)
	return stats, nil
}
