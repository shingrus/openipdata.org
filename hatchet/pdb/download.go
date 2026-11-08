// Package pdb is the sync-peeringdb workflow: it downloads PeeringDB datasets
// and derives per-ASN country, city, dominance, and domain into public.asn_geo_pdb.
package pdb

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	apiBase   = "https://www.peeringdb.com/api"
	pageLimit = 500
	userAgent = "openipdata-worker/1.0"

	requestDelay   = 1 * time.Second
	errorDelay     = 10 * time.Second
	rateLimitDelay = 60 * time.Second
	serverDelay    = 60 * time.Second
)

// datasets lists each PeeringDB object and the fields the computation needs.
var datasets = []struct{ name, fields string }{
	{"org", "id,city,country,website"},
	{"net", "id,asn,org_id,website"},
	{"fac", "id,city,country"},
	{"ix", "id,city,country"},
	{"netfac", "id,net_id,fac_id"},
	{"netixlan", "id,net_id,ix_id"},
}

type client struct {
	base   string // API base URL, apiBase outside tests
	apiKey string
	http   *http.Client
	log    func(string, ...any)

	requests    int // HTTP requests sent
	rateLimited int // HTTP 429 answers
}

type page struct {
	Data []json.RawMessage `json:"data"`
	Meta struct {
		Total *int `json:"total"`
	} `json:"meta"`
}

// get fetches one page, retrying rate limits, server errors, and network errors
// until ctx ends. Other HTTP errors (bad key, bad request) fail immediately.
func (c *client) get(ctx context.Context, obj string, params url.Values) (page, error) {
	endpoint := c.base + "/" + obj + "?" + params.Encode()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return page{}, err
		}
		req.Header.Set("Authorization", "Api-Key "+c.apiKey)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", userAgent)

		c.requests++
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return page{}, ctx.Err()
			}
			c.log("download: %s: request error (%v); retrying in %s", obj, err, errorDelay)
			if err := sleep(ctx, errorDelay); err != nil {
				return page{}, err
			}
			continue
		}
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			resp.Body.Close()
			c.rateLimited++
			c.log("download: %s: rate limited; retrying in %s", obj, rateLimitDelay)
			if err := sleep(ctx, rateLimitDelay); err != nil {
				return page{}, err
			}
			continue
		case resp.StatusCode >= 500:
			resp.Body.Close()
			c.log("download: %s: HTTP %d; retrying in %s", obj, resp.StatusCode, serverDelay)
			if err := sleep(ctx, serverDelay); err != nil {
				return page{}, err
			}
			continue
		case resp.StatusCode >= 400:
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
			resp.Body.Close()
			return page{}, fmt.Errorf("%s: HTTP %d: %s", obj, resp.StatusCode, body)
		}
		var p page
		err = json.NewDecoder(resp.Body).Decode(&p)
		resp.Body.Close()
		if err != nil {
			return page{}, fmt.Errorf("%s: decode response: %w", obj, err)
		}
		if err := sleep(ctx, requestDelay); err != nil {
			return page{}, err
		}
		return p, nil
	}
}

// downloadDataset writes every row of obj to path as JSON lines, via a .tmp file.
func (c *client) downloadDataset(ctx context.Context, obj, fields, path string) (int, error) {
	tmp := path + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp)
	w := bufio.NewWriter(file)

	rows := 0
	for skip := 0; ; skip += pageLimit {
		params := url.Values{
			"limit":  {strconv.Itoa(pageLimit)},
			"skip":   {strconv.Itoa(skip)},
			"fields": {fields},
		}
		p, err := c.get(ctx, obj, params)
		if err != nil {
			file.Close()
			return rows, err
		}
		if len(p.Data) == 0 {
			break
		}
		for _, row := range p.Data {
			w.Write(row)
			w.WriteByte('\n')
		}
		rows += len(p.Data)
		if p.Meta.Total != nil && skip+pageLimit >= *p.Meta.Total {
			break
		}
	}
	if err := w.Flush(); err != nil {
		file.Close()
		return rows, err
	}
	if err := file.Close(); err != nil {
		return rows, err
	}
	return rows, os.Rename(tmp, path)
}

func datasetPath(dir, name string) string {
	return filepath.Join(dir, name+".jsonl")
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
