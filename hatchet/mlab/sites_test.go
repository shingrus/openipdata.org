package mlab

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"openipdata.org/hatchet/publish"
)

const sitesHeader = "client24,site,site_country,site_city,tests,ips,min_rtt,p10_rtt,median_rtt\n"

// gnLine is one line of cities1000.txt: id, name, ascii name, alternate names, lat, lon, feature
// class and code, country, cc2, admin1, admin2, admin3, admin4, population.
func gnLine(id int, name, ascii, alts, country string) string {
	return fmt.Sprintf("%d\t%s\t%s\t%s\t1.5\t2.5\tP\tPPL\t%s\t\t00\t\t\t\t1000", id, name, ascii, alts, country)
}

func geoNamesZip(t *testing.T, lines ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, err := zw.Create("cities1000.txt")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, strings.Join(lines, "\n")+"\n")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// The default places: New York (by an alternate name), Chicago, Paris (Lutetia is an alternate
// name, the Cyrillic one has no Latin letter), Berlin and a city with a comma in its name.
func defaultZip(t *testing.T) []byte {
	return geoNamesZip(t,
		gnLine(1, "New York City", "New York City", "NYC,New York", "US"),
		gnLine(2, "Chicago", "Chicago", "", "US"),
		gnLine(3, "Paris", "Paris", "Lutetia,Париж", "FR"),
		gnLine(4, "Berlin", "Berlin", "", "DE"),
		gnLine(5, "Washington, D.C.", "Washington, D.C.", "", "US"),
		gnLine(6, "Zzyzx", "Zzyzx", "", "US"),
		"short line\tonly",
		"7\tBad\tBad\t\tx\ty\tP\tPPL\tUS\t\t00\t\t\t\t1000", // no coordinates: skipped
	)
}

type siteEnv struct {
	t       *testing.T
	p       *Pipeline
	srv     *httptest.Server
	body    []byte
	status  int
	hits    int
	logs    []string
	uploads []putCall
}

func newSiteEnv(t *testing.T) *siteEnv {
	t.Helper()
	e := &siteEnv{t: t, status: 200}
	e.body = defaultZip(t)
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits++
		if e.status != 200 {
			http.Error(w, "no", e.status)
			return
		}
		w.Write(e.body)
	}))
	t.Cleanup(e.srv.Close)
	e.p = &Pipeline{
		DataDir:          t.TempDir(),
		minSiteRows:      1,
		minSiteLayerRows: 1,
		geoURL:           e.srv.URL,
		dlClient:         e.srv.Client(),
		Progress:         func(s string) { e.logs = append(e.logs, s) },
		upload: func(ctx context.Context, key, path string, opts publish.PutOptions) (string, publish.PutStatus, error) {
			e.uploads = append(e.uploads, putCall{key, path, opts})
			return "r2://" + key, publish.PutUploaded, nil
		},
	}
	return e
}

// run writes csv into the sites inbox and runs the task on it.
func (e *siteEnv) run(csv string) (SiteLayerOutput, error) {
	e.t.Helper()
	name := "sites.csv"
	if err := os.MkdirAll(e.p.sitesInbox(), 0o750); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.p.sitesInbox(), name), []byte(csv), 0o640); err != nil {
		e.t.Fatal(err)
	}
	return e.p.SiteLayer(context.Background(), name, SiteLayerOptions{})
}

func (e *siteEnv) layer() string {
	e.t.Helper()
	f, err := os.Open(e.p.SiteLayerPath())
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(gr)
	return string(b)
}

func TestSiteLayerBestCandidate(t *testing.T) {
	rows := []string{
		"20.1.1,a,US,New York,2,1,4.0,4.0,4.0",
		"20.1.1,b,US,Chicago,2,1,4.0,4.0,4.0",              // the same time: the smaller city wins
		"20.1.1,c,US,New York,2,1,4.5,4.5,4.5",             // slower
		"20.1.2,a,FR,Paris,2,1,3.0,3.0,3.0",                //
		"20.1.2,b,US,New York,2,1,2.0,2.0,2.0",             // faster: wins
		"20.1.3,b,US,Chicago,2,1,6.0,6.0,6.0",              // over the limit
		"20.1.4,b,us,Chicago,2,1,5.0,5.0,5.0",              // exactly the limit: in; lower-case country
		"20.1.5,b,US,Chicago,2,1,5.01,5.0,5.0",             // just above: out
		"20.1.6,a,FR,Paris,2,1,3,3,3",                      // the same time and city country: DE is smaller
		"20.1.6,b,DE,Berlin,2,1,3,3,3",                     //
		"20.1.7,a,US,\"Washington, D.C.\",2,1,1.5,1.5,1.5", // a quoted city with a comma
		"20.1.8,a,FR,Lutetia,2,1,1.0,1.0,1.0",              // an alternate name with Latin letters
		"20.1.9,a,FR,Париж,2,1,1.0,1.0,1.0",                // an alternate name without one is not indexed
		"20.1.10,a,US,Nowhereville,2,1,1.0,1.0,1.0",        // not a GeoNames place
	}
	want := "prefix,country,region,city,source\n" +
		"20.1.1.0/24,US,,Chicago,speedtest-site\n" +
		"20.1.2.0/24,US,,New York,speedtest-site\n" +
		"20.1.4.0/24,US,,Chicago,speedtest-site\n" +
		"20.1.6.0/24,DE,,Berlin,speedtest-site\n" +
		"20.1.7.0/24,US,,\"Washington, D.C.\",speedtest-site\n" +
		"20.1.8.0/24,FR,,Lutetia,speedtest-site\n"

	e := newSiteEnv(t)
	out, err := e.run(sitesHeader + strings.Join(rows, "\n") + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.layer(); got != want {
		t.Errorf("layer:\n%s\nwant:\n%s", got, want)
	}
	if out.Rows != 14 || out.Valid != 14 || out.Clients != 10 || out.OverLimit != 2 || out.Unplaced != 2 || out.Located != 6 || out.Cities != 5 || out.Bad() != 0 {
		t.Errorf("%+v", out)
	}
	if out.Upload != "uploaded" || out.GeoNames != "downloaded" || out.LayerBytes == 0 || out.File != "sites.csv" {
		t.Errorf("%+v", out)
	}
	if len(e.uploads) != 2 || e.uploads[0].key != "internal/layers/35-speedtest-site.csv.gz" || e.uploads[0].path != e.p.SiteLayerPath() || e.uploads[0].opts.Refresh || e.uploads[0].opts.Force ||
		e.uploads[1].key != SiteRTTKey || e.uploads[1].path != e.p.SiteRTTPath() || e.uploads[1].opts.Refresh || e.uploads[1].opts.Force {
		t.Errorf("uploads = %+v", e.uploads)
	}

	// The same rows in the opposite order give the same file.
	slices.Reverse(rows)
	e2 := newSiteEnv(t)
	if _, err := e2.run(sitesHeader + strings.Join(rows, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
	if got := e2.layer(); got != want {
		t.Errorf("reversed:\n%s", got)
	}
}

func TestSiteLayerBadRows(t *testing.T) {
	long := "20.1.1,a,US," + strings.Repeat("x", 5000) + ",1,1,2.0"
	tests := []struct {
		name string
		line string
		get  func(SiteLayerOutput) int
	}{
		{"too few columns", "20.1.1,a,US", func(o SiteLayerOutput) int { return o.BadColumns }},
		{"unbalanced quote", "20.1.1,a,US,\"New York,2,1,2.0", func(o SiteLayerOutput) int { return o.BadColumns }},
		{"text after a quote", "20.1.1,a,US,\"New York\"x,2,1,2.0", func(o SiteLayerOutput) int { return o.BadColumns }},
		{"quote in a bare field", "20.1.1,a,US,New \"York,2,1,2.0", func(o SiteLayerOutput) int { return o.BadColumns }},
		{"too long", long, func(o SiteLayerOutput) int { return o.BadTooLong }},
		{"longer than the read buffer", "20.1.1,a,US," + strings.Repeat("x", 200_000) + ",1,1,2.0", func(o SiteLayerOutput) int { return o.BadTooLong }},
		{"short client", "20.1,a,US,Chicago,2,1,2.0", func(o SiteLayerOutput) int { return o.BadClient }},
		{"four octets", "20.1.1.1,a,US,Chicago,2,1,2.0", func(o SiteLayerOutput) int { return o.BadClient }},
		{"private client", "10.1.1,a,US,Chicago,2,1,2.0", func(o SiteLayerOutput) int { return o.NotPublicClient }},
		{"no site", "20.1.1,,,,2,1,2.0", func(o SiteLayerOutput) int { return o.NoSite }},
		{"no city", "20.1.1,a,US,,2,1,2.0", func(o SiteLayerOutput) int { return o.NoSite }},
		{"country of three letters", "20.1.1,a,USA,Chicago,2,1,2.0", func(o SiteLayerOutput) int { return o.BadCountry }},
		{"country with a digit", "20.1.1,a,U1,Chicago,2,1,2.0", func(o SiteLayerOutput) int { return o.BadCountry }},
		{"city of 65 bytes", "20.1.1,a,US," + strings.Repeat("x", 65) + ",2,1,2.0", func(o SiteLayerOutput) int { return o.BadCity }},
		{"city with a control character", "20.1.1,a,US,Chi\x01cago,2,1,2.0", func(o SiteLayerOutput) int { return o.BadCity }},
		{"city with an angle bracket", "20.1.1,a,US,Chi<cago,2,1,2.0", func(o SiteLayerOutput) int { return o.BadCity }},
		{"city that is not UTF-8", "20.1.1,a,US,Chi\xffcago,2,1,2.0", func(o SiteLayerOutput) int { return o.BadCity }},
		{"time not a number", "20.1.1,a,US,Chicago,2,1,abc", func(o SiteLayerOutput) int { return o.BadRTT }},
		{"time NaN", "20.1.1,a,US,Chicago,2,1,NaN", func(o SiteLayerOutput) int { return o.BadRTT }},
		{"time Inf", "20.1.1,a,US,Chicago,2,1,Inf", func(o SiteLayerOutput) int { return o.BadRTT }},
		{"time zero", "20.1.1,a,US,Chicago,2,1,0", func(o SiteLayerOutput) int { return o.BadRTT }},
		{"time negative", "20.1.1,a,US,Chicago,2,1,-1", func(o SiteLayerOutput) int { return o.BadRTT }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newSiteEnv(t)
			out, err := e.run(sitesHeader + "20.1.2,a,US,Chicago,2,1,2.0\n" + tc.line + "\n")
			if err != nil {
				t.Fatal(err)
			}
			if tc.get(out) != 1 || out.Bad() != 1 || out.Valid != 1 || out.Rows != 2 || out.Located != 1 {
				t.Errorf("%+v", out)
			}
		})
	}
}

func TestSiteLayerTooManyCities(t *testing.T) {
	e := newSiteEnv(t)
	var b strings.Builder
	b.WriteString(sitesHeader)
	for i := 0; i < maxSiteCities+3; i++ {
		fmt.Fprintf(&b, "20.%d.%d,a,US,City%d,2,1,9.0\n", i/250+1, i%250+1, i)
	}
	b.WriteString("20.200.1,a,US,City0,2,1,9.0\n") // a known city is still fine
	out, err := e.run(b.String())
	if err == nil || !errors.Is(err, ErrPermanent) {
		// no row is within the limit: too few layer rows
		t.Fatalf("%+v %v", out, err)
	}
	if out.TooManyCities != 3 || out.Valid != maxSiteCities+1 {
		t.Errorf("%+v", out)
	}
}

func TestSiteLayerNoFile(t *testing.T) {
	e := newSiteEnv(t)
	for _, create := range []bool{false, true} {
		if create {
			os.MkdirAll(e.p.sitesInbox(), 0o750)
			os.WriteFile(filepath.Join(e.p.sitesInbox(), "notes.txt"), []byte("x"), 0o640)
		}
		out, err := e.p.SiteLayer(context.Background(), "", SiteLayerOptions{})
		if err != nil || out.Skipped != "no file" {
			t.Fatalf("%+v %v", out, err)
		}
	}
	if _, err := os.Stat(e.p.SiteLayerPath()); err == nil {
		t.Error("a layer file was written")
	}
	if len(e.uploads) != 0 || e.hits != 0 {
		t.Errorf("uploads %v, downloads %d", e.uploads, e.hits)
	}
	// A name that does not exist is permanent.
	if _, err := e.p.SiteLayer(context.Background(), "missing.csv", SiteLayerOptions{}); !errors.Is(err, ErrPermanent) {
		t.Errorf("missing name: %v", err)
	}
	for _, name := range []string{"../x.csv", "a/b.csv", "..", "x.txt"} {
		if _, err := e.p.SiteLayer(context.Background(), name, SiteLayerOptions{}); !errors.Is(err, ErrPermanent) {
			t.Errorf("%q: %v", name, err)
		}
	}
}

func TestSiteLayerMinimumsKeepTheOldFile(t *testing.T) {
	e := newSiteEnv(t)
	if _, err := e.run(sitesHeader + "20.1.1,a,US,Chicago,2,1,2.0\n"); err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(e.p.SiteLayerPath())
	e.uploads = nil

	e.p.minSiteRows = 3
	out, err := e.run(sitesHeader + "20.1.1,a,US,Chicago,2,1,2.0\n20.1.2,a,US,Chicago,2,1,2.0\n")
	if !errors.Is(err, ErrPermanent) || out.Valid != 2 {
		t.Errorf("too few valid rows: %+v %v", out, err)
	}
	e.p.minSiteRows = 1
	e.p.minSiteLayerRows = 2
	out, err = e.run(sitesHeader + "20.1.1,a,US,Chicago,2,1,2.0\n20.1.2,a,US,Chicago,2,1,9.0\n")
	if !errors.Is(err, ErrPermanent) || out.Located != 1 {
		t.Errorf("too few layer rows: %+v %v", out, err)
	}
	if now, _ := os.ReadFile(e.p.SiteLayerPath()); !bytes.Equal(now, old) {
		t.Error("the layer file changed")
	}
	if _, err := os.Stat(e.p.SiteLayerPath() + ".tmp"); err == nil {
		t.Error("a temporary file was left")
	}
	if len(e.uploads) != 0 {
		t.Errorf("uploads = %v", e.uploads)
	}
	// A header without the needed columns.
	if _, err := e.run("a,b\n1,2\n"); !errors.Is(err, ErrPermanent) {
		t.Errorf("header: %v", err)
	}
}

func TestSiteLayerGeoNames(t *testing.T) {
	csv := sitesHeader + "20.1.1,a,US,Chicago,2,1,2.0\n"

	t.Run("download fails, no copy", func(t *testing.T) {
		e := newSiteEnv(t)
		e.status = 500
		if _, err := e.run(csv); !errors.Is(err, errNoGeoNames) || errors.Is(err, ErrPermanent) {
			t.Errorf("%v", err)
		}
		if _, err := os.Stat(e.p.SiteLayerPath()); err == nil {
			t.Error("a layer file was written")
		}
	})
	t.Run("download fails, saved copy", func(t *testing.T) {
		e := newSiteEnv(t)
		os.MkdirAll(e.p.dir(), 0o750)
		os.WriteFile(e.p.geoNamesPath(), e.body, 0o640)
		e.status = 500
		out, err := e.run(csv)
		if err != nil || out.GeoNames != "saved copy" || out.Located != 1 {
			t.Fatalf("%+v %v", out, err)
		}
		if !strings.Contains(strings.Join(e.logs, "\n"), "WARNING") {
			t.Errorf("no warning in %q", e.logs)
		}
	})
	t.Run("recent copy is not asked again", func(t *testing.T) {
		e := newSiteEnv(t)
		if _, err := e.run(csv); err != nil {
			t.Fatal(err)
		}
		out, err := e.run(csv)
		if err != nil || out.GeoNames != "recent" || e.hits != 1 {
			t.Errorf("%+v %v hits %d", out, err, e.hits)
		}
	})
	t.Run("damaged copy is removed", func(t *testing.T) {
		e := newSiteEnv(t)
		os.MkdirAll(e.p.dir(), 0o750)
		path := e.p.geoNamesPath()
		os.WriteFile(path, []byte("not a zip"), 0o640)
		os.WriteFile(path+".etag", []byte("x"), 0o640)
		os.WriteFile(path+".checked", nil, 0o640)
		_, err := e.run(csv)
		if !errors.Is(err, errBadGeoNames) || errors.Is(err, ErrPermanent) {
			t.Fatalf("%v", err)
		}
		for _, f := range []string{path, path + ".etag", path + ".checked"} {
			if _, err := os.Stat(f); err == nil {
				t.Errorf("%s was not removed", filepath.Base(f))
			}
		}
	})
	t.Run("a copy with no place is removed", func(t *testing.T) {
		e := newSiteEnv(t)
		e.body = geoNamesZip(t, "nothing\there")
		if _, err := e.run(csv); !errors.Is(err, errBadGeoNames) {
			t.Fatalf("%v", err)
		}
		if _, err := os.Stat(e.p.geoNamesPath()); err == nil {
			t.Error("the copy was not removed")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		e := newSiteEnv(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		os.MkdirAll(e.p.sitesInbox(), 0o750)
		os.WriteFile(filepath.Join(e.p.sitesInbox(), "a.csv"), []byte(csv), 0o640)
		if _, err := e.p.SiteLayer(ctx, "", SiteLayerOptions{}); !errors.Is(err, context.Canceled) {
			t.Errorf("%v", err)
		}
	})
}

func TestSiteLayerInputAndUpload(t *testing.T) {
	csv := sitesHeader + "20.1.1,a,US,Chicago,2,1,2.0\n"
	// Gzip input, a byte order mark and capital column names.
	e := newSiteEnv(t)
	path := filepath.Join(t.TempDir(), "in.csv.gz")
	var b bytes.Buffer
	gw := gzip.NewWriter(&b)
	gw.Write([]byte("\xef\xbb\xbf" + strings.ToUpper(sitesHeader) + "20.1.1,a,US,Chicago,2,1,2.0\r\n"))
	gw.Close()
	os.WriteFile(path, b.Bytes(), 0o640)
	out, err := e.p.SiteLayer(context.Background(), "ignored.csv", SiteLayerOptions{Path: path, ForceUpload: true})
	if err != nil || out.Located != 1 || out.File != "in.csv.gz" {
		t.Fatalf("%+v %v", out, err)
	}
	if len(e.uploads) != 2 || !e.uploads[0].opts.Force || e.uploads[0].opts.Refresh || !e.uploads[1].opts.Force {
		t.Errorf("uploads = %+v", e.uploads)
	}

	// Skip upload.
	e = newSiteEnv(t)
	os.MkdirAll(e.p.sitesInbox(), 0o750)
	os.WriteFile(filepath.Join(e.p.sitesInbox(), "a.csv"), []byte(csv), 0o640)
	out, err = e.p.SiteLayer(context.Background(), "", SiteLayerOptions{SkipUpload: true})
	if err != nil || out.Upload != "skipped" || len(e.uploads) != 0 {
		t.Errorf("%+v %v %v", out, err, e.uploads)
	}
	if _, err := os.Stat(e.p.SiteLayerPath()); err != nil {
		t.Error("no layer file")
	}

	// A failed upload fails the task.
	e = newSiteEnv(t)
	e.p.upload = func(context.Context, string, string, publish.PutOptions) (string, publish.PutStatus, error) {
		return "", "", errors.New("boom")
	}
	if _, err := e.run(csv); err == nil || errors.Is(err, ErrPermanent) {
		t.Errorf("upload error: %v", err)
	}
}

// Nothing from the input reaches a log line or the output.
func TestSiteLayerLogsHoldCountsOnly(t *testing.T) {
	e := newSiteEnv(t)
	out, err := e.run(sitesHeader +
		"20.77.88,siteq,US,Zzyzx,2,1,2.0\n" +
		"20.77.89,siteq,US,Qqnowhere,2,1,2.0\n" +
		"20.77.90,siteq,US,Zzyzx,2,1,xyz\n")
	if err != nil || out.Located != 1 || out.Unplaced != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	js, _ := json.Marshal(out)
	all := strings.Join(e.logs, "\n") + string(js)
	if len(e.logs) == 0 {
		t.Fatal("no log line")
	}
	for _, secret := range []string{"20.77", "Zzyzx", "Qqnowhere", "siteq", "xyz"} {
		if strings.Contains(all, secret) {
			t.Errorf("%q is in a log line or the output: %s", secret, all)
		}
	}
}

// Empty lines are not rows, but they count for the cancellation check.
func TestSiteLayerEmptyLines(t *testing.T) {
	e := newSiteEnv(t)
	gap := strings.Repeat("\n", 10)
	out, err := e.run(sitesHeader + gap + "20.1.1,a,US,Chicago,2,1,2.0\n" + gap + "20.1.2,a,US,Chicago,2,1,2.0\n" + gap)
	if err != nil || out.Rows != 2 || out.Valid != 2 || out.Located != 2 {
		t.Fatalf("%+v %v", out, err)
	}

	// A file of only empty lines after the header: a cancelled run is noticed.
	path := filepath.Join(t.TempDir(), "empty.csv")
	os.WriteFile(path, []byte(sitesHeader+strings.Repeat("\n", 70000)), 0o640)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.p.SiteLayer(ctx, "", SiteLayerOptions{Path: path}); !errors.Is(err, context.Canceled) {
		t.Errorf("empty lines: %v", err)
	}
}

func TestSplitQuoted(t *testing.T) {
	tests := []struct {
		in   string
		want []string
		ok   bool
	}{
		{"a,b,c", []string{"a", "b", "c"}, true},
		{"a,,c", []string{"a", "", "c"}, true},
		{"a,b,", []string{"a", "b", ""}, true},
		{`a,"b,c",d`, []string{"a", "b,c", "d"}, true},
		{`a,"b ""x"" c",d`, []string{"a", `b "x" c`, "d"}, true},
		{`"a",""`, []string{"a", ""}, true},
		{`a,"b`, nil, false},
		{`a,"b"c`, nil, false},
		{`a,b"c`, nil, false},
	}
	for _, tc := range tests {
		got, ok := splitQuoted([]byte(tc.in), nil)
		if ok != tc.ok {
			t.Errorf("%q: ok = %v", tc.in, ok)
			continue
		}
		if !ok {
			continue
		}
		var s []string
		for _, f := range got {
			s = append(s, string(f))
		}
		if !slices.Equal(s, tc.want) {
			t.Errorf("%q: %q, want %q", tc.in, s, tc.want)
		}
	}
}

func (e *siteEnv) rtt() string {
	e.t.Helper()
	f, err := os.Open(e.p.SiteRTTPath())
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(gr)
	return string(b)
}

func TestSiteRTTFile(t *testing.T) {
	rows := []string{
		"20.1.2,a,FR,Paris,2,1,12.5,1,1",     // sorted after 20.1.1
		"20.1.1,b,US,Chicago,2,1,9.0,1,1",    //
		"20.1.1,c,US,Chicago,2,1,7.25,1,1",   // the same city (another site): the lowest time
		"20.1.1,a,DE,Berlin,2,1,30.0,1,1",    // exactly the limit: in; DE sorts before US
		"20.1.1,a,FR,Paris,2,1,30.5,1,1",     // above 30: out
		"20.1.1,a,US,Nowhereville,2,1,4,1,1", // not placed: out
		"20.1.4,a,US,Chicago,2,1,3,1,1",      // the one layer row
		"20.1.3,a,US,Chicago,2,1,31,1,1",     // above 30: out, and the client has no row at all
	}
	want := "prefix,country,city,rtt_ms\n" +
		"20.1.1.0/24,DE,Berlin,30\n" +
		"20.1.1.0/24,US,Chicago,7.25\n" +
		"20.1.2.0/24,FR,Paris,12.5\n" +
		"20.1.4.0/24,US,Chicago,3\n"
	e := newSiteEnv(t)
	out, err := e.run(sitesHeader + strings.Join(rows, "\n") + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.rtt(); got != want {
		t.Errorf("rtt file:\n%s\nwant:\n%s", got, want)
	}
	if out.RTTRows != 4 || out.RTTBytes == 0 || out.RTTUpload != "uploaded" || out.RTTSkipped != "" {
		t.Errorf("%+v", out)
	}
	// The same rows in the opposite order give the same file.
	slices.Reverse(rows)
	e2 := newSiteEnv(t)
	if _, err := e2.run(sitesHeader + strings.Join(rows, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
	if got := e2.rtt(); got != want {
		t.Errorf("reversed:\n%s", got)
	}
}

// More rows than the cap: no RTT file, the layer is still made with every row.
func TestSiteRTTTooManyRows(t *testing.T) {
	old := rttRowLimit
	rttRowLimit = 2
	t.Cleanup(func() { rttRowLimit = old })
	e := newSiteEnv(t)
	out, err := e.run(sitesHeader + "20.1.1,a,US,Chicago,2,1,2\n20.1.2,a,US,Chicago,2,1,2\n20.1.3,a,FR,Paris,2,1,3\n")
	if err != nil {
		t.Fatal(err)
	}
	if out.Located != 3 || out.Upload != "uploaded" || out.RTTSkipped != "too many rows" || out.RTTRows != 0 || out.RTTUpload != "skipped" {
		t.Errorf("%+v", out)
	}
	if _, err := os.Stat(e.p.SiteRTTPath()); !os.IsNotExist(err) {
		t.Errorf("RTT file is there: %v", err)
	}
}

// With the layer minimum there is always an RTT row (a layer row is within 5 ms and placed), so
// "no rows" only guards the helper.
func TestBuildRTTRowsEmpty(t *testing.T) {
	if rows := buildRTTRows(nil, nil, nil, nil); len(rows) != 0 {
		t.Errorf("rows = %v", rows)
	}
}

func TestSiteRTTWriteFailureKeepsTheLayer(t *testing.T) {
	e := newSiteEnv(t)
	if err := os.MkdirAll(e.p.SiteRTTPath(), 0o750); err != nil { // a directory is in the way of the rename
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(e.p.SiteRTTPath(), "x"), []byte("x"), 0o640)
	out, err := e.run(sitesHeader + "20.1.1,a,US,Chicago,2,1,2.0\n")
	if err != nil {
		t.Fatal(err)
	}
	if out.Located != 1 || out.Upload != "uploaded" || out.RTTSkipped != "write failed" || out.RTTUpload != "skipped" || out.RTTRows != 0 {
		t.Errorf("%+v", out)
	}
	if len(e.uploads) != 1 || e.uploads[0].key != SiteLayerKey {
		t.Errorf("uploads = %+v", e.uploads)
	}
}

func TestSiteRTTUploadErrorFailsTheTask(t *testing.T) {
	e := newSiteEnv(t)
	calls := 0
	e.p.upload = func(_ context.Context, key, _ string, _ publish.PutOptions) (string, publish.PutStatus, error) {
		calls++
		if key == SiteRTTKey {
			return "", "", errors.New("boom")
		}
		return "r2://" + key, publish.PutUploaded, nil
	}
	if _, err := e.run(sitesHeader + "20.1.1,a,US,Chicago,2,1,2.0\n"); err == nil || errors.Is(err, ErrPermanent) || calls != 2 {
		t.Errorf("err %v, calls %d", err, calls)
	}
}
