package geo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// oracleRegion is one region of a test file.
type oracleRegion struct {
	name  string
	cidrs []string
}

// oracleJSON makes an Oracle region file of the given regions plus 300 filler /24s of the region
// "filler-1" (3.0.0.0 to 3.1.43.0), so that it has enough valid entries.
func oracleJSON(regions ...oracleRegion) []byte {
	var parts []string
	add := func(r oracleRegion) {
		var c []string
		for _, x := range r.cidrs {
			c = append(c, fmt.Sprintf(`{"cidr":%q,"tags":["OCI"]}`, x))
		}
		parts = append(parts, fmt.Sprintf(`{"region":%q,"cidrs":[%s]}`, r.name, strings.Join(c, ",")))
	}
	for _, r := range regions {
		add(r)
	}
	filler := oracleRegion{name: "filler-1"}
	for i := 0; i < oracleRegionsMinPrefixes; i++ {
		filler.cidrs = append(filler.cidrs, fmt.Sprintf("3.%d.%d.0/24", i/256, i%256))
	}
	add(filler)
	return []byte(`{"last_updated_timestamp":"2026-10-28T02:47:53.012304","regions":[` + strings.Join(parts, ",") + `]}`)
}

func TestOracleRegionTable(t *testing.T) {
	if len(oracleRegions) != 45 {
		t.Errorf("%d entries", len(oracleRegions))
	}
	for region, place := range oracleRegions {
		if !validRegion(region) {
			t.Errorf("region %q", region)
		}
		if len(place.country) != 2 || place.country != strings.ToUpper(place.country) || strings.Trim(place.country, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
			t.Errorf("%s: country %q", region, place.country)
		}
		if place.city == "" || place.city != strings.TrimSpace(place.city) || !utf8.ValidString(place.city) {
			t.Errorf("%s: city %q", region, place.city)
		}
	}
	if oracleRegions["uk-cardiff-1"].city != "Newport" {
		t.Error("uk-cardiff-1 is in Newport")
	}
}

func TestParseOracleRegions(t *testing.T) {
	ranges, st, err := parseOracleRegions(oracleJSON(oracleRegion{"US-Phoenix-1", []string{"129.146.0.0/21", " 134.70.8.0/21 "}}))
	if err != nil || st.Valid != 2+oracleRegionsMinPrefixes || st.Bad != 0 || st.Entries != st.Valid {
		t.Fatalf("%+v %v", st, err)
	}
	if ranges[0].region != "us-phoenix-1" || ranges[0].bits != 21 || ranges[0].addr != 129<<24|146<<16 {
		t.Errorf("first range %+v", ranges[0])
	}

	// The region key after cidrs, other keys skipped, an IPv6 range ignored and not bad.
	body := `{"a":{"b":[1,{"c":null}]},"regions":[{"cidrs":[{"cidr":"129.146.0.0/21","tags":["x"]},{"cidr":"2603:c000::/32"}],"extra":[[1]],"region":"us-phoenix-1"},` +
		`{"region":"filler-1","cidrs":[` + strings.TrimSuffix(strings.Repeat(`{"cidr":"3.0.0.0/24"},`, oracleRegionsMinPrefixes), ",") + `]}],"z":1}`
	ranges, st, err = parseOracleRegions([]byte(body))
	if err != nil || st.Entries != 2+oracleRegionsMinPrefixes || st.Valid != 1+oracleRegionsMinPrefixes || st.Bad != 0 {
		t.Fatalf("%+v %v", st, err)
	}
	if ranges[0].region != "us-phoenix-1" {
		t.Errorf("label after cidrs: %+v", ranges[0])
	}

	// Bad entries are counted.
	_, st, err = parseOracleRegions(oracleJSON(
		oracleRegion{"us-phoenix-1", []string{"129.146.0.5/21", "10.0.0.0/24", "8.0.0.0/7", "nope", "", "129.146.8.0/21"}},
		oracleRegion{"Bad Label!", []string{"129.147.0.0/21"}},
		oracleRegion{"", []string{"129.148.0.0/21"}},
		// Broader than a /12 is not used; a /12 is.
		oracleRegion{"eu-paris-1", []string{"130.0.0.0/11", "130.32.0.0/12"}},
	))
	if err != nil || st.Bad != 5+1+1+1 || st.Valid != 2+oracleRegionsMinPrefixes {
		t.Errorf("%+v %v", st, err)
	}

	// A wrong type anywhere makes the file unusable.
	good := string(oracleJSON())
	for name, body := range map[string]string{
		"not json":           "nope",
		"top is an array":    "[]",
		"regions not array":  `{"regions":{}}`,
		"region not object":  `{"regions":["x"]}`,
		"region not string":  `{"regions":[{"region":5,"cidrs":[]}]}`,
		"cidrs not array":    `{"regions":[{"region":"a","cidrs":"x"}]}`,
		"element not object": `{"regions":[{"region":"a","cidrs":["1.0.0.0/24"]}]}`,
		"cidr not string":    `{"regions":[{"region":"a","cidrs":[{"cidr":5}]}]}`,
		"cut off":            good[:len(good)-5],
	} {
		if _, _, err := parseOracleRegions([]byte(body)); err == nil {
			t.Errorf("%s: no error", name)
		} else if _, ok := err.(*cloudInputError); !ok {
			t.Errorf("%s: type %T", name, err)
		}
	}

	// Too few valid entries.
	few := `{"regions":[{"region":"a","cidrs":[` + strings.TrimSuffix(strings.Repeat(`{"cidr":"3.0.0.0/24"},`, oracleRegionsMinPrefixes-1), ",") + `]}]}`
	if _, _, err := parseOracleRegions([]byte(few)); err == nil {
		t.Error("no error for too few entries")
	} else if _, ok := err.(*cloudInputError); !ok {
		t.Errorf("type %T", err)
	}

	// More entries than the limit, counted over the whole file.
	list := func(n int) string { return strings.TrimSuffix(strings.Repeat(`{"cidr":"3.0.0.0/24"},`, n), ",") }
	many := `{"regions":[{"region":"a","cidrs":[` + list(cloudRegionsMaxEntries/2) + `]},{"region":"b","cidrs":[` +
		list(cloudRegionsMaxEntries/2+1) + `]}]}`
	_, st, err = parseOracleRegions([]byte(many))
	if _, ok := err.(*cloudInputError); !ok || st.Entries != cloudRegionsMaxEntries+1 {
		t.Errorf("%+v %v", st, err)
	}
}

func TestOracleRegionRows(t *testing.T) {
	data := oracleJSON(
		oracleRegion{"eu-paris-1", []string{"130.0.0.0/20", "130.0.16.0/20"}},
		oracleRegion{"us-phoenix-1", []string{"129.146.0.0/21", "129.146.0.0/22"}},
		oracleRegion{"eu-madrid-1", []string{"129.146.0.0/24"}},
		oracleRegion{"xx-nowhere-1", []string{"131.0.0.0/16"}},
	)
	rows, st, err := oracleRegionRows(data)
	if err != nil {
		t.Fatal(err)
	}
	// Seen counts every label of the file (the filler too), Qualified the table regions with a row.
	if st.Seen != 5 || st.Qualified != 3 || st.Rows != len(rows) {
		t.Errorf("%+v", st)
	}
	// A table region whose addresses all belong to more specific prefixes of another region has
	// no row and is not counted.
	_, st, err = oracleRegionRows(oracleJSON(
		oracleRegion{"eu-paris-1", []string{"130.0.0.0/24"}},
		oracleRegion{"eu-madrid-1", []string{"130.0.0.0/25", "130.0.0.128/25"}},
	))
	if err != nil || st.Seen != 3 || st.Qualified != 1 || st.Rows != 2 {
		t.Errorf("%+v %v", st, err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.prefix.String()] = r.country + "|" + r.city + "|" + r.source
		if r.prefix.Bits() < 20 && !strings.HasPrefix(r.prefix.String(), "130.0") {
			t.Errorf("row %s broader than an entry", r.prefix)
		}
	}
	// The /21 and its /22 are both phoenix: the /22 is its own range, the /24 of Madrid is
	// taken out of it, and the rest of the /21 beyond the /22 stays Phoenix. Neighbours are not merged.
	for prefix, want := range map[string]string{
		"130.0.0.0/20":   "FR|Paris|cloud-regions:oracle:eu-paris-1",
		"130.0.16.0/20":  "FR|Paris|cloud-regions:oracle:eu-paris-1",
		"129.146.0.0/24": "ES|Madrid|cloud-regions:oracle:eu-madrid-1",
		"129.146.1.0/24": "US|Phoenix|cloud-regions:oracle:us-phoenix-1",
		"129.146.2.0/23": "US|Phoenix|cloud-regions:oracle:us-phoenix-1",
		"129.146.4.0/22": "US|Phoenix|cloud-regions:oracle:us-phoenix-1",
	} {
		if got[prefix] != want {
			t.Errorf("%s = %q, want %q (rows %v)", prefix, got[prefix], want, got)
		}
	}
	for prefix := range got {
		if strings.HasPrefix(prefix, "131.") || strings.HasPrefix(prefix, "3.") {
			t.Errorf("a region outside the table has a row: %s", prefix)
		}
	}
}

func TestDownloadOracleRegions(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 18, 7, 0, 0, 0, time.UTC)
	client := newHTTPClient()
	aws := newRegionServer(regionJSON([2]string{"52.0.0.0/16", "eu-west-1"}))
	defer aws.Close()
	ora := newRegionServer(oracleJSON(oracleRegion{"eu-paris-1", []string{"20.0.0.0/21"}}))
	defer ora.Close()
	const awsRow = "52.0.0.0/16,IE,,Dublin,cloud-regions:aws:eu-west-1\n"
	const oraRow = "20.0.0.0/21,FR,,Paris,cloud-regions:oracle:eu-paris-1\n"

	run := func(p *Pipeline, layers, downloads string, at time.Time) *Source {
		t.Helper()
		src := newCloudSource()
		if err := p.downloadCloudRegions(ctx, client, downloads, layers, at, src); err != nil {
			t.Fatal(err)
		}
		return src
	}

	// Both operators: one file, sorted by prefix, Oracle's row first here.
	p, layers, downloads := cloudPipeline(t, aws.URL, "52.0.0.0/20,IE,Dublin")
	p.oracleRegionsURL = ora.URL
	src := run(p, layers, downloads, now)
	if src.skipped(CloudRegionsLayer) || len(src.CloudRegionsLeftOut) != 0 || src.Rows[CloudRegionsLayer] != 2 || src.CloudRegions != 2 || src.CloudRegionsSeen != 4 {
		t.Fatalf("source %+v", src)
	}
	if got := readGzip(t, filepath.Join(layers, CloudRegionsLayer)); got != feedHeader+oraRow+awsRow {
		t.Errorf("layer:\n%s", got)
	}

	// Oracle's fetch fails with no saved copy: Amazon's rows are written.
	ora.fail = true
	p2, layers2, downloads2 := cloudPipeline(t, aws.URL, "52.0.0.0/20,IE,Dublin")
	p2.oracleRegionsURL = ora.URL
	src = run(p2, layers2, downloads2, now)
	if src.skipped(CloudRegionsLayer) || !reflect.DeepEqual(src.CloudRegionsLeftOut, []string{"oracle"}) || src.Rows[CloudRegionsLayer] != 1 {
		t.Fatalf("source %+v", src)
	}
	if got := readGzip(t, filepath.Join(layers2, CloudRegionsLayer)); got != feedHeader+awsRow {
		t.Errorf("layer:\n%s", got)
	}

	// Oracle's fetch fails with a recent saved copy (the first run's): the copy is used.
	src = run(p, layers, downloads, now.Add(5*24*time.Hour))
	if src.skipped(CloudRegionsLayer) || len(src.CloudRegionsLeftOut) != 0 || src.Rows[CloudRegionsLayer] != 2 {
		t.Errorf("saved copy not used: %+v", src)
	}
	ora.fail = false

	// Amazon's fetch fails: Oracle's rows are written.
	aws.fail = true
	p3, layers3, downloads3 := cloudPipeline(t, aws.URL, "52.0.0.0/20,IE,Dublin")
	p3.oracleRegionsURL = ora.URL
	src = run(p3, layers3, downloads3, now)
	if src.skipped(CloudRegionsLayer) || !reflect.DeepEqual(src.CloudRegionsLeftOut, []string{"aws"}) || src.CloudRegions != 1 || src.CloudRegionsSeen != 2 {
		t.Fatalf("source %+v", src)
	}
	if got := readGzip(t, filepath.Join(layers3, CloudRegionsLayer)); got != feedHeader+oraRow {
		t.Errorf("layer:\n%s", got)
	}
	aws.fail = false

	// No geofeed layer: Oracle only.
	p4, layers4, downloads4 := cloudPipeline(t, aws.URL)
	p4.oracleRegionsURL = ora.URL
	src = run(p4, layers4, downloads4, now)
	if src.skipped(CloudRegionsLayer) || !reflect.DeepEqual(src.CloudRegionsLeftOut, []string{"aws"}) {
		t.Fatalf("source %+v", src)
	}
	if got := readGzip(t, filepath.Join(layers4, CloudRegionsLayer)); got != feedHeader+oraRow {
		t.Errorf("layer:\n%s", got)
	}

	// An unusable Oracle file is deleted with its .etag and .checked, Amazon's rows stay.
	bad := newRegionServer([]byte(`{"regions":[]}`))
	defer bad.Close()
	p5, layers5, downloads5 := cloudPipeline(t, aws.URL, "52.0.0.0/20,IE,Dublin")
	p5.oracleRegionsURL = bad.URL
	src = run(p5, layers5, downloads5, now)
	if !reflect.DeepEqual(src.CloudRegionsLeftOut, []string{"oracle"}) || src.Rows[CloudRegionsLayer] != 1 {
		t.Errorf("source %+v", src)
	}
	saved := filepath.Join(downloads5, oracleRegionsFile)
	for _, f := range []string{saved, saved + ".etag", saved + ".checked"} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s was kept", filepath.Base(f))
		}
	}

	// A good Oracle file with no region of the table: Oracle is left out, its labels are counted.
	none := newRegionServer(oracleJSON(oracleRegion{"xx-nowhere-1", []string{"20.0.0.0/21"}}))
	defer none.Close()
	p7, layers7, downloads7 := cloudPipeline(t, aws.URL, "52.0.0.0/20,IE,Dublin")
	p7.oracleRegionsURL = none.URL
	src = run(p7, layers7, downloads7, now)
	if !reflect.DeepEqual(src.CloudRegionsLeftOut, []string{"oracle"}) || src.CloudRegions != 1 || src.CloudRegionsSeen != 2+2 || src.Rows[CloudRegionsLayer] != 1 {
		t.Errorf("source %+v", src)
	}
	if _, err := os.Stat(filepath.Join(downloads7, oracleRegionsFile)); err != nil {
		t.Error("a good file with no table region was not kept")
	}

	// Neither has rows: the layer is skipped, nothing is stale and no file is left.
	aws.fail = true
	ora.fail = true
	p6, layers6, downloads6 := cloudPipeline(t, aws.URL, "52.0.0.0/20,IE,Dublin")
	p6.oracleRegionsURL = ora.URL
	src = run(p6, layers6, downloads6, now)
	if !src.skipped(CloudRegionsLayer) || len(src.StaleLayers) != 0 || !reflect.DeepEqual(src.CloudRegionsLeftOut, []string{"aws", "oracle"}) || src.Rows[CloudRegionsLayer] != 0 {
		t.Errorf("source %+v", src)
	}
	if _, err := os.Stat(filepath.Join(layers6, CloudRegionsLayer)); err == nil {
		t.Error("a layer file for a skipped layer")
	}
	aws.fail, ora.fail = false, false

	// A cancelled context fails the task.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := p.downloadCloudRegions(cctx, client, downloads, layers, now, newCloudSource()); err == nil {
		t.Error("no error for a cancelled context")
	}
}

func TestPrepareHoldoutOracleRegions(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := filepath.Join(dir, "mmdbbuilder")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > "+argsFile+"\n"), 0o755)
	p := &Pipeline{DataDir: dir, MMDBBuilder: script, heldOutURLs: heldOutSet(testHeldOut)}
	layers := p.layersDir()
	downloads := filepath.Join(p.dir(), "downloads")
	os.MkdirAll(layers, 0o750)
	os.MkdirAll(downloads, 0o750)
	writeGzip(t, filepath.Join(layers, RIRLayer), feedHeader)
	// Feed1 is held out: Amazon's af-south-1 learns nothing from the kept feeds.
	writeGzip(t, filepath.Join(layers, FeedsLayer), feedHeader+
		"52.0.0.0/20,IE,,Dublin,https://feed0.example/geo.csv\n"+
		"13.0.0.0/20,ZA,,Cape Town,https://feed1.example/geo.csv\n")
	awsFile := filepath.Join(downloads, cloudRegionsFile)
	oraFile := filepath.Join(downloads, oracleRegionsFile)
	os.WriteFile(awsFile, regionJSON([2]string{"52.0.0.0/16", "eu-west-1"}, [2]string{"13.0.0.0/16", "af-south-1"}), 0o640)
	os.WriteFile(oraFile, oracleJSON(oracleRegion{"eu-paris-1", []string{"20.0.0.0/21"}}), 0o640)
	// This run's layer has rows of both operators, so both take part in the holdout.
	const bothRows = "20.0.0.0/21,FR,,Paris,cloud-regions:oracle:eu-paris-1\n52.0.0.0/16,IE,,Dublin,cloud-regions:aws:eu-west-1\n"
	writeGzip(t, filepath.Join(layers, CloudRegionsLayer), feedHeader+bothRows)

	eval := p.evalDir()
	os.MkdirAll(eval, 0o750)
	own := filepath.Join(eval, "holdout", CloudRegionsLayer)
	prepare := func() (string, string) {
		t.Helper()
		os.Remove(argsFile)
		mmdb, _, status, err := p.prepareHoldout(context.Background(), eval)
		if err != nil || mmdb == "" {
			t.Fatalf("%q %q %v", mmdb, status, err)
		}
		args, _ := os.ReadFile(argsFile)
		return status, string(args)
	}

	// Both operators, sorted by prefix.
	status, args := prepare()
	if got := readGzip(t, own); got != feedHeader+bothRows {
		t.Errorf("holdout layer = %q", got)
	}
	if !strings.Contains(args, " "+own+" ") || strings.Contains(status, "left out") || strings.Contains(status, CloudRegionsLayer+" without") {
		t.Errorf("args = %s status = %q", args, status)
	}

	// No saved Oracle file: Amazon's rows only, and the note says so.
	os.Remove(oraFile)
	status, args = prepare()
	if got := readGzip(t, own); got != feedHeader+"52.0.0.0/16,IE,,Dublin,cloud-regions:aws:eu-west-1\n" {
		t.Errorf("holdout layer = %q", got)
	}
	if !strings.Contains(status, "; "+CloudRegionsLayer+" without oracle: no saved region file") || !strings.Contains(args, " "+own+" ") {
		t.Errorf("args = %s status = %q", args, status)
	}

	// No saved Amazon file: Oracle's rows only.
	os.WriteFile(oraFile, oracleJSON(oracleRegion{"eu-paris-1", []string{"20.0.0.0/21"}}), 0o640)
	os.Remove(awsFile)
	status, _ = prepare()
	if got := readGzip(t, own); got != feedHeader+"20.0.0.0/21,FR,,Paris,cloud-regions:oracle:eu-paris-1\n" {
		t.Errorf("holdout layer = %q", got)
	}
	if !strings.Contains(status, "without aws: no saved region file") {
		t.Errorf("status = %q", status)
	}

	// A saved Oracle file with no region of the table: Oracle is left out with its reason.
	os.WriteFile(awsFile, regionJSON([2]string{"52.0.0.0/16", "eu-west-1"}, [2]string{"13.0.0.0/16", "af-south-1"}), 0o640)
	os.WriteFile(oraFile, oracleJSON(oracleRegion{"xx-nowhere-1", []string{"20.0.0.0/21"}}), 0o640)
	status, _ = prepare()
	if !strings.Contains(status, "without oracle: no region of the file is in the table") {
		t.Errorf("status = %q", status)
	}

	// Neither: the layer is left out of the build, and no file of an earlier holdout stays.
	os.Remove(awsFile)
	os.Remove(oraFile)
	status, args = prepare()
	if strings.Contains(args, "68-cloud-regions") || !strings.Contains(status, CloudRegionsLayer+" left out: aws: no saved region file, oracle: no saved region file") {
		t.Errorf("args = %s status = %q", args, status)
	}
	if _, err := os.Stat(own); err == nil {
		t.Error("the holdout layer file of an earlier run was kept")
	}

	// An operator that has no row in this run's layer (Download left it out) takes no part,
	// whatever copy is saved.
	os.WriteFile(awsFile, regionJSON([2]string{"52.0.0.0/16", "eu-west-1"}, [2]string{"13.0.0.0/16", "af-south-1"}), 0o640)
	os.WriteFile(oraFile, oracleJSON(oracleRegion{"eu-paris-1", []string{"20.0.0.0/21"}}), 0o640)
	writeGzip(t, filepath.Join(layers, CloudRegionsLayer), feedHeader+"52.0.0.0/16,IE,,Dublin,cloud-regions:aws:eu-west-1\n")
	status, _ = prepare()
	if got := readGzip(t, own); got != feedHeader+"52.0.0.0/16,IE,,Dublin,cloud-regions:aws:eu-west-1\n" {
		t.Errorf("holdout layer = %q", got)
	}
	if strings.Contains(status, "oracle") {
		t.Errorf("status = %q", status)
	}
}
