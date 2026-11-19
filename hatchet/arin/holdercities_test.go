package arin

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"openipdata.org/hatchet/publish"
)

func rowTexts(rows []layerRow) string {
	var got []string
	for _, r := range rows {
		got = append(got, r.prefix+","+r.country+","+r.state+","+r.city)
	}
	return strings.Join(got, " ")
}

func TestBuildHolderCityRows(t *testing.T) {
	idx := testIndex(t)
	recs := []record{
		rec("10.0.0.0", "10.0.63.255", "US", "OR", "Portland"),    // a /18: taken, whatever its class
		rec("10.1.0.0", "10.1.3.255", "US", "OR", "Portland"),     // a /22: layer only
		rec("10.2.0.0", "10.2.15.255", "US", "OR", "Portland"),    // a /20: layer only
		rec("10.3.0.0", "10.3.31.255", "US", "ZZ", "Nowhere"),     // unplaced
		rec("10.4.0.0", "10.4.31.255", "US", "IL", "Springfield"), // another state
		rec("10.5.0.0", "10.5.31.255", "US", "OH", "Delta"),       // ambiguous
		rec("10.6.0.0", "10.6.31.255", "", "", ""),                // no city
	}
	holders := []record{
		hrec("10.16.0.0", "10.31.255.255", "OR", "Portland", classOther),  // a /12
		hrec("10.40.0.0", "10.41.255.255", "MO", "Springfield", classEdu), // 10.40/15, not a /16 ... wider than a /16
		hrec("10.50.0.0", "10.50.255.255", "OR", "Portland", classEdu),    // a /16: the main records hold it
		hrec("10.60.0.0", "10.62.255.255", "OR", "Portland", classGov),    // not one block: /15 and /16
	}
	rows, cands, dropped := buildHolderCityRows(recs, holders, idx)
	want := "10.0.0.0/18,US,OR,Portland 10.16.0.0/12,US,OR,Portland 10.40.0.0/15,US,MO,Springfield 10.60.0.0/15,US,OR,Portland 10.62.0.0/16,US,OR,Portland"
	if got := rowTexts(rows); got != want {
		t.Errorf("rows\n%s\nwant\n%s", got, want)
	}
	if cands != 3+3+1 || dropped != 3 {
		t.Errorf("candidates %d dropped %d", cands, dropped)
	}
	// The same prefix from two records: the smaller record wins (10.2.0.0/16 is in both).
	rows, _, _ = buildHolderCityRows([]record{
		rec("10.2.0.0", "10.2.255.255", "US", "TX", "Foo Town"),
	}, []record{
		hrec("10.0.0.0", "10.2.255.255", "OR", "Portland", classOther),
	}, idx)
	if got := rowTexts(rows); got != "10.0.0.0/15,US,OR,Portland 10.2.0.0/16,US,TX,Foo Town" {
		t.Errorf("overlap: %s", got)
	}
	rows, _, _ = buildHolderCityRows([]record{
		rec("10.0.0.0", "10.0.63.255", "US", "TX", "Foo Town"),
		rec("10.0.0.0", "10.0.63.255", "US", "OR", "Portland"),
	}, nil, idx)
	if len(rows) != 1 {
		t.Errorf("duplicate prefix: %v", rows)
	}
}

// The layer and its counters are what they were; the holder blocks file comes beside it.
func TestExportHolderCities(t *testing.T) {
	geo := newGeo(t, zipOf(t, testCities), 200)
	e := newEnv(t, nil, 8)
	e.p.geoURL, e.p.dlClient = geo.URL, geo.Client()
	os.MkdirAll(e.p.dir(), 0o750)
	e.db.nets = []record{
		rec("10.0.0.0", "10.0.63.255", "US", "OR", "Portland"), // a /18
		rec("10.1.0.0", "10.1.3.255", "US", "OR", "Portland"),  // a /22: layer only
		rec("10.3.0.0", "10.3.31.255", "US", "ZZ", "Nowhere"),  // unplaced
	}
	appendHolders(e.p.dir(), []record{
		hrec("10.16.0.0", "10.31.255.255", "OR", "Portland", classOther),
		hrec("10.50.0.0", "10.50.255.255", "OR", "Portland", classEdu), // a /16: layer only
	}, testNow.Add(-time.Hour))
	out, err := e.p.Export(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Records != 1 || out.Rows != 2 || out.HolderRows != 1 || out.Unplaced != 0 || out.HolderCityRecords != 3 || out.HolderCityDropped != 1 || out.HolderCityRows != 2 {
		t.Errorf("output %+v", out)
	}
	want := "prefix,country,region,city,source\n" +
		"10.0.0.0/18,US,OR,Portland,holder-city:arin\n" +
		"10.16.0.0/12,US,OR,Portland,holder-city:arin\n"
	if got := readLayer(t, e.p.HolderCitiesPath()); got != want {
		t.Errorf("holder blocks\n%s\nwant\n%s", got, want)
	}
	wantLayer := "prefix,country,region,city,source\n" +
		"10.1.0.0/22,US,OR,Portland,whois-city:arin\n" +
		"10.50.0.0/16,US,OR,Portland,whois-city:arin\n"
	if got := readLayer(t, e.p.LayerPath()); got != wantLayer {
		t.Errorf("layer\n%s\nwant\n%s", got, wantLayer)
	}
	e.noAddress(t, out)

	if out.HolderCityFile != holderCitiesWritten {
		t.Errorf("file status %q", out.HolderCityFile)
	}

	// The file cannot be written: the task succeeds, the layer is written, the old file stays.
	if err := os.Mkdir(e.p.HolderCitiesPath()+".tmp", 0o750); err != nil {
		t.Fatal(err)
	}
	os.Remove(e.p.LayerPath())
	if out, err = e.p.Export(context.Background(), true); err != nil || out.HolderCityFile != holdersFailed || out.Rows != 2 {
		t.Fatalf("write failure: %+v %v", out, err)
	}
	if got := readLayer(t, e.p.LayerPath()); got != wantLayer {
		t.Errorf("layer after a holder blocks failure\n%s", got)
	}
	if got := readLayer(t, e.p.HolderCitiesPath()); got != want {
		t.Errorf("holder blocks after a failure\n%s", got)
	}
	e.noAddress(t, out)
	os.Remove(e.p.HolderCitiesPath() + ".tmp")

	// holders.csv unreadable: the file is left as it is (main records alone would lose the wide blocks).
	holdersPath := e.p.dir() + "/" + holdersFile
	saved, err := os.ReadFile(holdersPath)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(holdersPath)
	os.Mkdir(holdersPath, 0o750)
	if out, err = e.p.Export(context.Background(), true); err != nil || !out.HoldersUnreadable || out.HolderCityFile != holderCitiesKept || out.HolderCityRows != 0 {
		t.Fatalf("unreadable: %+v %v", out, err)
	}
	if got := readLayer(t, e.p.HolderCitiesPath()); got != want {
		t.Errorf("kept file: %q", got)
	}
	os.Remove(holdersPath)
	os.WriteFile(holdersPath, saved, 0o640)

	// No candidate left: the file is removed.
	e.db.nets = []record{rec("10.1.0.0", "10.1.3.255", "US", "OR", "Portland")}
	os.Remove(holdersPath)
	if out, err = e.p.Export(context.Background(), true); err != nil || out.HolderCityRows != 0 || out.HolderCityFile != holderCitiesRemoved || fileExists(e.p.HolderCitiesPath()) {
		t.Errorf("no candidate: %+v %v", out, err)
	}
	e.noAddress(t, out)
}

// With no row for the layer the holder blocks file is still written.
func TestExportHolderCitiesWithoutLayerRows(t *testing.T) {
	geo := newGeo(t, zipOf(t, testCities), 200)
	e := newEnv(t, nil, 8)
	e.p.geoURL, e.p.dlClient = geo.URL, geo.Client()
	e.db.nets = []record{rec("10.0.0.0", "10.0.63.255", "US", "OR", "Portland")}
	out, err := e.p.Export(context.Background(), false)
	if err != nil || out.Skipped != "no rows" || out.HolderCityRows != 1 || !fileExists(e.p.HolderCitiesPath()) {
		t.Errorf("%+v %v", out, err)
	}
}

func TestUploadHolderCities(t *testing.T) {
	var keys []string
	var opts []publish.PutOptions
	status := publish.PutRefreshed
	failKey := ""
	p := &Pipeline{DataDir: t.TempDir(), now: func() time.Time { return testNow }, upload: func(_ context.Context, key, _ string, o publish.PutOptions) (string, publish.PutStatus, error) {
		keys, opts = append(keys, key), append(opts, o)
		if key == failKey {
			return "", "", errors.New("boom")
		}
		return "", status, nil
	}}
	// No file: no put for that key.
	os.MkdirAll(p.dir(), 0o750)
	os.WriteFile(p.LayerPath(), []byte("12345"), 0o640)
	out, err := p.Upload(context.Background(), false)
	if err != nil || out.HolderCities != "" {
		t.Fatalf("no file: %+v %v", out, err)
	}
	for _, k := range keys {
		if k == HolderCitiesKey {
			t.Error("a put for a missing file")
		}
	}
	// With the file: put at the fixed key with Refresh.
	os.WriteFile(p.HolderCitiesPath(), []byte("x"), 0o640)
	keys, opts = nil, nil
	out, err = p.Upload(context.Background(), true)
	if err != nil || out.HolderCities != "refreshed" {
		t.Fatalf("%+v %v", out, err)
	}
	found := false
	for i, k := range keys {
		if k == "internal/arin/holder-cities.csv.gz" && opts[i].Refresh && opts[i].Force {
			found = true
		}
	}
	if !found {
		t.Errorf("puts %v %+v", keys, opts)
	}
	// A failing put never fails the task or stops the backups.
	appendHolders(p.dir(), []record{hrec("10.0.1.0", "10.0.1.255", "OR", "Portland", classGov)}, testNow)
	os.WriteFile(p.dir()+"/"+recordsFile, []byte("not read"), 0o640) // a damaged base: backup is refused, not failed
	failKey = HolderCitiesKey
	keys = nil
	out, err = p.Upload(context.Background(), false)
	if err != nil || out.HolderCities != "failed" || out.HoldersBackup != backupUploaded || out.Backup == "" {
		t.Errorf("failing put: %+v %v", out, err)
	}
	// A cancelled context is returned. There is no layer file, so the first put is the holder blocks file's.
	os.Remove(p.LayerPath())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	keys = nil
	p.upload = func(ctx context.Context, key, _ string, _ publish.PutOptions) (string, publish.PutStatus, error) {
		keys = append(keys, key)
		return "", "", ctx.Err()
	}
	if _, err = p.Upload(ctx, true); !errors.Is(err, context.Canceled) || len(keys) != 1 || keys[0] != HolderCitiesKey {
		t.Errorf("a cancelled context: %v, puts %v", err, keys)
	}
}
