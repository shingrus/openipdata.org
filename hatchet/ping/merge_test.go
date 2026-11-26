package ping

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"openipdata.org/hatchet/publish"
)

// regionFiles writes the given region files (region index -> body) and returns the path list.
func regionFiles(t *testing.T, files map[int]string) []string {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, len(regions))
	for i, body := range files {
		paths[i] = filepath.Join(dir, regions[i].Name+".txt.gz")
		gz(t, paths[i], body)
	}
	return paths
}

func mergeText(t *testing.T, paths []string) (string, mergeStats) {
	t.Helper()
	var b bytes.Buffer
	stats, err := mergeRegions(context.Background(), paths, &b)
	if err != nil {
		t.Fatal(err)
	}
	return b.String(), stats
}

// row builds the expected row: cells by region index.
func row(prefix string, cells map[int]string) string {
	f := make([]string, len(regions))
	for i, c := range cells {
		f[i] = c
	}
	return prefix + "," + strings.Join(f, ",") + "\n"
}

func TestMerge(t *testing.T) {
	paths := regionFiles(t, map[int]string{
		0: "20.0.0.1 : 12.34\n20.0.1.1 : 5.0\n20.0.1.1 : 3.04\n20.0.2.1 : -\n20.0.3.1 : 0.04\n20.0.4.1 : duplicate warning\n",
		1: "20.0.0.1 : 7.0\n20.0.1.1 : 9.99\n20.0.2.1 : 4.0\n20.0.3.1 : 2.0\n",
		5: "garbage\n20.0.0.1 : 30\n20.0.0.1 : x\n20.0.1.1 : 1.25\n20.0.2.1 : 6.0\n20.0.3.1 : -\n19.0.0.1 : 1.0\n21.0.0.1 : 8.0\n",
		// Region 2 has no answers at all and region 3 no file: empty columns.
		2: "20.0.0.1 : -\n",
	})
	got, stats := mergeText(t, paths)
	if !strings.HasPrefix(got, allSitesHeader()+"\n") {
		t.Fatalf("header: %q", strings.SplitN(got, "\n", 2)[0])
	}
	body := strings.TrimPrefix(got, allSitesHeader()+"\n")
	// 20.0.2.0: region 1 and 5 answer, 0 does not: 2 answers, dropped. 20.0.3.0: 0.04 gives 0.1, but 2 answers: dropped.
	// 21.0.0.0: one answer: dropped.
	wantRows := row("20.0.0.0/24", map[int]string{0: "12.3", 1: "7.0", 5: "30.0"}) +
		row("20.0.1.0/24", map[int]string{0: "3.0", 1: "10.0", 5: "1.2"})
	if body != wantRows {
		t.Errorf("rows:\n%s\nwant:\n%s", body, wantRows)
	}
	if stats.Rows != 2 || stats.Short != 3 || stats.Ignored != 4 {
		t.Errorf("%+v", stats)
	}
	// ignored: "duplicate", "garbage", "x" and the out-of-order 19.0.0.1.
	if stats.Answers[regions[0].Name] != 4 || stats.Answers[regions[1].Name] != 4 || stats.Answers[regions[5].Name] != 4 || stats.Answers[regions[2].Name] != 0 {
		t.Errorf("answers %v", stats.Answers)
	}
	if len(stats.Answers) != 27 {
		t.Errorf("answers has %d regions", len(stats.Answers))
	}
}

func TestMergeFormatAndOrder(t *testing.T) {
	paths := regionFiles(t, map[int]string{
		0: "20.0.0.1 : 0.04\n90.0.0.1 : 1.0\n",
		1: "20.0.0.1 : 2000\n30.0.0.1 : 1.04\n90.0.0.1 : 2.0\n",
		2: "20.0.0.1 : 5\n30.0.0.1 : 1.06\n90.0.0.1 : 3.0\n",
		3: "30.0.0.1 : 1.0\n",
	})
	got, _ := mergeText(t, paths)
	body := strings.TrimPrefix(got, allSitesHeader()+"\n")
	want := row("20.0.0.0/24", map[int]string{0: "0.1", 1: "2000.0", 2: "5.0"}) +
		row("30.0.0.0/24", map[int]string{1: "1.0", 2: "1.1", 3: "1.0"}) +
		row("90.0.0.0/24", map[int]string{0: "1.0", 1: "2.0", 2: "3.0"})
	if body != want {
		t.Errorf("rows:\n%s\nwant:\n%s", body, want)
	}
}

func TestMergeNoFiles(t *testing.T) {
	got, stats := mergeText(t, make([]string, len(regions)))
	if got != allSitesHeader()+"\n" || stats.Rows != 0 {
		t.Errorf("%q %+v", got, stats)
	}
}

func TestMergeDamaged(t *testing.T) {
	paths := regionFiles(t, map[int]string{0: "20.0.0.1 : 1.0\n"})
	os.WriteFile(paths[0], []byte("not gzip"), 0o640)
	if _, err := mergeRegions(context.Background(), paths, io.Discard); err == nil {
		t.Error("a damaged result was accepted")
	}
}

func publishPipe(t *testing.T) (*Pipeline, *fakeStore, *logs) {
	t.Helper()
	store, lg := newFakeStore(), &logs{}
	var b bytes.Buffer
	for i := 0; i < 3; i++ {
		b.Reset()
		w := gzip.NewWriter(&b)
		for n := 0; n < 40; n++ {
			w.Write([]byte("20.0." + itoa(n) + ".1 : " + itoa(3+i) + ".5\n"))
		}
		w.Close()
		store.set(ResultKey(regions[i].Name), append([]byte(nil), b.Bytes()...))
	}
	store.set(ProgressKey("us-east1"), []byte("done"))
	return &Pipeline{DataDir: t.TempDir(), GCPKey: "k", Progress: lg.add, store: store, minRegions: 3, minValid: 10}, store, lg
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var s string
	for ; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	return s
}

func TestPublish(t *testing.T) {
	var pings atomic.Int32
	hc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/check" {
			pings.Add(1)
		}
	}))
	defer hc.Close()
	p, store, lg := publishPipe(t)
	p.HCPing = hc.URL + "/check"
	out, err := p.Publish(context.Background(), PublishOptions{ForceUpload: true})
	if err != nil || out.Regions != 3 || out.Rows != 40 || out.Valid != 40 || out.Bad != 0 || out.Sites != 27 || out.Upload != "uploaded" ||
		out.Addresses != 120 || out.ShortRows != 0 || out.AnswersPerRegion["us-east1"] != 40 || out.Bytes == 0 {
		t.Fatalf("%+v %v", out, err)
	}
	if !store.has(RTTKey) || !store.putOpts[RTTKey].Force || store.putOpts[RTTKey].Refresh {
		t.Errorf("upload opts %+v", store.putOpts[RTTKey])
	}
	// The run's objects go after the upload, and the file itself stays.
	if store.has(ResultKey("us-east1")) || store.has(TargetsKey) || out.RunObjectsGone != 4 {
		t.Errorf("run objects: gone %d", out.RunObjectsGone)
	}
	if store.events[0] != "put "+RTTKey || store.events[1] != "delete "+RunPrefix {
		t.Errorf("order %v", store.events)
	}
	if pings.Load() != 1 {
		t.Errorf("health check pings %d", pings.Load())
	}
	if _, err := os.Stat(p.outDir()); err == nil {
		t.Error("the downloaded results were not removed")
	}
	if strings.Contains(lg.text(), hc.URL) {
		t.Error("the health check URL is logged")
	}
	// The merged file is valid.
	if _, err := p.checkFile(context.Background(), p.mergedPath(), 10); err != nil {
		t.Error(err)
	}
}

func TestPublishSkipUploadKeepsRunObjects(t *testing.T) {
	p, store, _ := publishPipe(t)
	out, err := p.Publish(context.Background(), PublishOptions{SkipUpload: true})
	if err != nil || out.Upload != "skipped" || store.has(RTTKey) || !store.has(ResultKey("us-east1")) {
		t.Errorf("%+v %v", out, err)
	}
}

func TestPublishRefusals(t *testing.T) {
	p, store, _ := publishPipe(t)
	p.minRegions = 4
	if _, err := p.Publish(context.Background(), PublishOptions{}); err == nil || store.has(RTTKey) {
		t.Errorf("too few regions: %v", err)
	}
	p.minRegions = 3
	p.minValid = 100
	if _, err := p.Publish(context.Background(), PublishOptions{}); !errors.Is(err, ErrPermanent) || store.has(RTTKey) {
		t.Errorf("too few rows: %v", err)
	}
	p.GCPKey = ""
	if out, err := p.Publish(context.Background(), PublishOptions{}); err != nil || out.Skipped != "no GCP key" {
		t.Errorf("%+v %v", out, err)
	}
}

func TestFailPing(t *testing.T) {
	var path atomic.Value
	hc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { path.Store(r.URL.Path) }))
	defer hc.Close()
	lg := &logs{}
	p := &Pipeline{HCPing: hc.URL + "/abc/", Progress: lg.add}
	p.Fail(context.Background())
	if path.Load() != "/abc/fail" {
		t.Errorf("path %v", path.Load())
	}
	(&Pipeline{}).Fail(context.Background()) // no URL: nothing
}

func TestSettings(t *testing.T) {
	if err := (&Pipeline{}).CheckSettings(true); err != nil {
		t.Errorf("no key means nothing to check: %v", err)
	}
	if err := (&Pipeline{GCPKey: "x", R2: ""}).CheckSettings(false); err == nil {
		t.Error("no R2")
	}
	_, doc := testKey(t)
	r2 := "https://id:secret@acc.r2.cloudflarestorage.com/bucket"
	good := &Pipeline{GCPKey: base64Of(doc), R2: r2, PGSQL: "postgres://x"}
	if err := good.CheckSettings(true); err != nil {
		t.Errorf("%v", err)
	}
	bad := &Pipeline{GCPKey: "garbage", R2: r2}
	if err := bad.CheckSettings(false); !errors.Is(err, ErrInvalidGCPKey) {
		t.Errorf("%v", err)
	}
	good.PGSQL = ""
	if err := good.CheckSettings(true); err == nil {
		t.Error("no PGSQL")
	}
	_ = publish.ErrInvalidR2
}

func base64Of(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
