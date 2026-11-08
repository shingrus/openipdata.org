package pdb

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDownloadDatasetPages(t *testing.T) {
	const total = 1201 // three pages of 500
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RawQuery)
		if r.Header.Get("Authorization") != "Api-Key secret" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		skip, _ := strconv.Atoi(r.URL.Query().Get("skip"))
		var rows []string
		for id := skip; id < total && id < skip+pageLimit; id++ {
			rows = append(rows, fmt.Sprintf(`{"id":%d}`, id))
		}
		fmt.Fprintf(w, `{"data":[%s],"meta":{"total":%d}}`, strings.Join(rows, ","), total)
	}))
	defer server.Close()

	c := &client{base: server.URL, apiKey: "secret", http: server.Client(), log: func(string, ...any) {}}
	path := filepath.Join(t.TempDir(), "net.jsonl")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows, err := c.downloadDataset(ctx, "net", "id,asn", path)
	if err != nil {
		t.Fatal(err)
	}
	if rows != total || len(requests) != 3 {
		t.Fatalf("rows=%d requests=%d (%v), want %d rows in 3 requests", rows, len(requests), requests, total)
	}
	if !strings.Contains(requests[0], "fields=id%2Casn") || !strings.Contains(requests[2], "skip=1000") {
		t.Errorf("requests = %v", requests)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(data), "\n"); lines != total {
		t.Errorf("file has %d lines, want %d", lines, total)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp file left behind: %v", err)
	}
}

func TestDownloadDatasetFailsOnAuthError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"invalid key"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	c := &client{base: server.URL, apiKey: "bad", http: server.Client(), log: func(string, ...any) {}}
	path := filepath.Join(t.TempDir(), "org.jsonl")
	_, err := c.downloadDataset(context.Background(), "org", "id", path)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("err = %v, want HTTP 401", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("output file written despite the error")
	}
}
