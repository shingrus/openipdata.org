// Package bgp is the sync-bgp workflow: it downloads a public RIPE RIS routing table
// dump, reads the BGP community values of every IPv4 prefix and uploads them (OutFile)
// to the private R2 bucket, where build-geo-data turns them into the 25-bgp layer.
// Some networks tag routes with a community value that means "learned in city X";
// the learning (value to city, from prefixes whose city a geofeed gives) happens in
// build-geo-data, which has the geofeeds. This package only reads routing data.
package bgp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"openipdata.org/hatchet/download"
	"openipdata.org/hatchet/publish"
)

const (
	// SourceURL is the newest full table of the RIPE RIS collector rrc00 (public data, about 430 MB).
	SourceURL = "https://data.ris.ripe.net/rrc00/latest-bview.gz"

	mrtFile       = "latest-bview.gz"
	checkedSuffix = ".checked"
	// downloadGate: the dump is made every 8 hours; a run within this time of the last check
	// does not ask again.
	downloadGate = 6 * time.Hour
	// maxDownload caps the bytes read from the server.
	maxDownload = 2 << 30
)

// Pipeline runs the BGP community sync. Each method is one task; the Hatchet
// workflow and the local run-bgp command call them in the same order.
type Pipeline struct {
	DataDir  string
	Progress func(string)
	R2       string // private bucket (CommunitiesKey)
	HCPing   string // optional healthchecks.io ping URL (HC_PING_BGP); never logged

	// Tests only.
	url         string
	client      *http.Client
	now         func() time.Time
	minPrefixes int
	upload      func(ctx context.Context, key, path string, opts publish.PutOptions) (string, publish.PutStatus, error)
}

func (p *Pipeline) dir() string { return filepath.Join(p.DataDir, "bgp") }

func (p *Pipeline) log(format string, args ...any) {
	if p.Progress != nil {
		p.Progress(fmt.Sprintf(format, args...))
	}
}

func (p *Pipeline) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// CheckUpload fails when the R2 setting is missing or invalid, so a run stops before the download.
func (p *Pipeline) CheckUpload() error {
	_, err := publish.NewR2(p.R2)
	return err
}

// DownloadOutput is the download task's result.
type DownloadOutput struct {
	Status          string  `json:"status"` // downloaded, cached (the server's copy is the one we have) or recent (checked less than 6 hours ago)
	Bytes           int64   `json:"bytes"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// MRTPath is where Download keeps the dump: only the latest one.
func (p *Pipeline) MRTPath() string { return filepath.Join(p.dir(), mrtFile) }

// OutPath is where Extract writes the communities file.
func (p *Pipeline) OutPath() string { return filepath.Join(p.dir(), OutFile) }

// Download fetches the latest dump into the workflow's own directory with the shared
// conditional download. A run less than downloadGate after the last successful check
// asks nobody. The file is replaced through a temporary file, so there is only ever one.
// A newly downloaded dump is read through once (checkGzip): a damaged one is deleted and
// fetched a second time, and when that one is damaged too the task fails with no dump
// left, so the next run starts clean.
func (p *Pipeline) Download(ctx context.Context) (DownloadOutput, error) {
	started := time.Now()
	var out DownloadOutput
	if err := os.MkdirAll(p.dir(), 0o750); err != nil {
		return out, err
	}
	path := p.MRTPath()
	stamp := path + checkedSuffix
	if fi, err := os.Stat(path); err == nil {
		out.Bytes = fi.Size()
		if si, err := os.Stat(stamp); err == nil && p.clock().Sub(si.ModTime()) < downloadGate && si.ModTime().Before(p.clock().Add(time.Minute)) {
			out.Status = "recent"
			p.log("download: %s checked %s ago, not asked again", mrtFile, p.clock().Sub(si.ModTime()).Round(time.Minute))
			return out, nil
		}
	}
	url, client := p.url, p.client
	if url == "" {
		url = SourceURL
	}
	if client == nil {
		client = newHTTPClient()
	}
	status, err := p.fetch(ctx, client, url, path)
	if err != nil {
		return out, fmt.Errorf("download %s: %w", mrtFile, err)
	}
	now := p.clock()
	if f, err := os.OpenFile(stamp, os.O_CREATE|os.O_WRONLY, 0o640); err == nil {
		f.Close()
		os.Chtimes(stamp, now, now)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return out, err
	}
	out.Status, out.Bytes = status, fi.Size()
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	p.log("download: %s %s, %d bytes", mrtFile, status, out.Bytes)
	return out, nil
}

// fetchTries is how often Download fetches a dump that arrives damaged.
const fetchTries = 2

// fetch is the conditional download plus the check of a new file. A file the server did
// not send again (cached) was checked when it came.
func (p *Pipeline) fetch(ctx context.Context, client *http.Client, url, path string) (string, error) {
	for try := 1; ; try++ {
		status, err := download.File(ctx, client, url, path)
		if err != nil || status != download.Downloaded {
			return status, err
		}
		if err = checkGzip(ctx, path); err == nil {
			return status, nil
		}
		if !errors.Is(err, ErrDamaged) {
			return "", err // cancelled, or the disk could not be read: the file stays
		}
		p.removeDump()
		if try == fetchTries {
			return "", err
		}
		p.log("download: %s arrived damaged, fetching it again", mrtFile)
	}
}

// removeDump deletes Download's dump with its ETag and its checked stamp, so the next
// Download asks the server for the whole file whatever the age gate says.
func (p *Pipeline) removeDump() {
	path := p.MRTPath()
	for _, name := range []string{path, path + ".etag", path + checkedSuffix} {
		os.Remove(name)
	}
}

// Extract reads the dump at mrtPath (Download's file when empty) and writes OutPath.
// When Download's own dump turns out damaged it is deleted (removeDump): reading it again
// cannot work, and the next run then downloads a new one. A file named by the caller is
// never deleted.
func (p *Pipeline) Extract(ctx context.Context, mrtPath string) (ExtractOutput, error) {
	own := mrtPath == ""
	if own {
		mrtPath = p.MRTPath()
	}
	if err := os.MkdirAll(p.dir(), 0o750); err != nil {
		return ExtractOutput{}, err
	}
	min := p.minPrefixes
	if min == 0 {
		min = minFilePrefixes
	}
	out, err := ExtractFile(ctx, mrtPath, p.OutPath(), min)
	if err != nil {
		if own && errors.Is(err, ErrDamaged) {
			p.removeDump()
			p.log("extract: %s is damaged and was deleted; the next run downloads it again", mrtFile)
		}
		return out, err
	}
	p.log("extract: %d prefixes from %d peers, %d of %d values kept (%d common, %d rare), %d pairs, %d bytes",
		out.Prefixes, out.Peers, out.ValuesKept, out.ValuesSeen, out.ValuesCommon, out.ValuesRare, out.Pairs, out.Bytes)
	return out, nil
}

// UploadOutput is the upload task's result.
type UploadOutput struct {
	Status          publish.PutStatus `json:"status"` // uploaded, unchanged or refreshed
	Bytes           int64             `json:"bytes"`
	DurationSeconds float64           `json:"duration_seconds"`
}

// Upload puts OutPath at CommunitiesKey with its sha256 in the object's metadata, as
// sync-geofeeds does with its layers: an object that already holds the same bytes is not
// sent again, and a server-side copy renews its modified time (build-geo-data skips a
// layer older than 3 days). force always sends the bytes. After a successful upload the
// health check is pinged; a failed ping only logs.
func (p *Pipeline) Upload(ctx context.Context, force bool) (UploadOutput, error) {
	started := time.Now()
	var out UploadOutput
	fi, err := os.Stat(p.OutPath())
	if err != nil {
		return out, fmt.Errorf("no %s (has extract run?): %w", OutFile, err)
	}
	put := p.upload
	if put == nil {
		r2, err := publish.NewR2(p.R2)
		if err != nil {
			return out, err
		}
		put = r2.Put
	}
	_, status, err := put(ctx, CommunitiesKey, p.OutPath(), publish.PutOptions{Force: force, Refresh: true})
	if err != nil {
		return out, err
	}
	out.Status, out.Bytes = status, fi.Size()
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	p.log("upload: %s (%d bytes)", status, out.Bytes)
	if err := publish.Ping(ctx, p.HCPing); err != nil {
		p.log("upload: %v", err) // the upload succeeded; a missed ping only delays the alert
	}
	return out, nil
}

// newHTTPClient is a plain client: the server is a fixed public host, so no address
// filter is needed, but every wait is bounded and the body is capped.
func newHTTPClient() *http.Client {
	base := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	return &http.Client{Transport: &capTransport{rt: base, max: maxDownload}, Timeout: 90 * time.Minute}
}

// errTooLarge is the error of a body over the cap.
var errTooLarge = errors.New("response is larger than the download limit")

// capTransport stops a response that says it is, or turns out to be, larger than max.
type capTransport struct {
	rt  http.RoundTripper
	max int64
}

func (t *capTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > t.max {
		resp.Body.Close()
		return nil, errTooLarge
	}
	resp.Body = &capBody{rc: resp.Body, left: t.max + 1}
	return resp, nil
}

type capBody struct {
	rc   io.ReadCloser
	left int64
}

func (b *capBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	if b.left -= int64(n); b.left <= 0 {
		return n, errTooLarge
	}
	return n, err
}

func (b *capBody) Close() error { return b.rc.Close() }
