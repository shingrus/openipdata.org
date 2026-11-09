// Package download fetches a file over HTTP unless the local copy is current.
// The caller passes the HTTP client, which decides which addresses may be
// reached; each workflow keeps its downloads in its own directory.
package download

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// UserAgent is sent with every request the worker makes.
const UserAgent = "openipdata-worker/1.0 (+https://github.com/shingrus/openipdata.org)"

// File results.
const (
	Downloaded = "downloaded"
	Cached     = "cached" // 304: the local copy is current
)

// File downloads rawURL to path unless the local copy is current. The file's
// modification time holds the server's Last-Modified and <path>.etag its
// ETag; both go back as If-Modified-Since and If-None-Match. The file is
// replaced through a temporary file, so a failed download leaves the old copy.
func File(ctx context.Context, client *http.Client, rawURL, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", UserAgent)
	info, statErr := os.Stat(path)
	if statErr == nil {
		req.Header.Set("If-Modified-Since", info.ModTime().UTC().Format(http.TimeFormat))
		if etag, err := os.ReadFile(path + ".etag"); err == nil {
			req.Header.Set("If-None-Match", strings.TrimSpace(string(etag)))
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified && statErr == nil:
		return Cached, nil
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	tmp := path + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	if _, err := io.Copy(file, resp.Body); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if modified, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		if err := os.Chtimes(tmp, modified, modified); err != nil {
			return "", err
		}
	}
	// The old ETag goes before the new file arrives and the new one comes
	// after, so a run cut short here never pairs an ETag with the wrong file:
	// at worst the next run downloads again.
	os.Remove(path + ".etag")
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	if etag := resp.Header.Get("ETag"); etag != "" {
		if err := os.WriteFile(path+".etag", []byte(etag), 0o640); err != nil {
			return "", err
		}
	}
	return Downloaded, nil
}
