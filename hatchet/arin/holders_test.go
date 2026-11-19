package arin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"openipdata.org/hatchet/publish"
)

func TestHolderClass(t *testing.T) {
	cases := []struct{ name, want string }{
		{"Example University", classEdu},
		{"Ohio State University", classEdu},
		{"University of Example, San Diego", classEdu},
		{"Example Community College", classEdu},
		{"Example School District", classEdu},
		{"Springfield School District 12", classEdu},
		{"Example Unified Schools", classEdu},
		{"Example Institute of Technology", classEdu},
		{"Example Polytechnic", classEdu},
		{"Example Academy", classEdu},
		{"Collegiate Institute", classOther}, // "collegiate" is not "college"
		{"University Federal Credit Union", classOther},
		{"Collegeville Communications", classOther},
		{"Academy Sports + Outdoors", classOther},
		{"Academy Bank", classOther},
		{"School Specialty Inc", classOther},
		{"Oklahoma State Regents for Higher Education", classOther},
		{"Board of Regents of the University System of Georgia", classOther},
		{"The Curators of the University of Missouri", classOther},
		{"Board of Education of Example", classOther},
		{"State of Iowa", classOther},
		{"Commonwealth of Virginia", classOther},
		{"Iowa Communications Network", classOther},
		{"Preschooling Inc", classOther},
		{"City of Example", classGov},
		{"Example County", classGov},
		{"County of Example", classGov},
		{"Town of Example", classGov},
		{"Village of Example", classGov},
		{"Township of Example", classGov},
		{"Borough of Example", classGov},
		{"Example County Sheriff", classGov},
		{"Office of the City of Example", classGov},
		{"Example Municipal Court", classGov},
		{"Wood County Telephone Company", classOther},
		{"Bank of Clarke County", classOther},
		{"Orange County's Credit Union", classOther},
		{"Municipal Credit Union", classOther},
		{"Example Municipal Utilities", classOther},
		{"State of the Art Networks Inc", classOther},
		{"Countywide Broadband", classOther},
		{"Statewide Fiber", classOther},
		{"Example Networks LLC", classOther},
		{"", classOther},
		{"   ", classOther},
		{"Department of Defense Network Information Center", classMil},
		{"DoD Network Information Center", classMil},
		{"Navy Federal Credit Union", classMil}, // accepted: mil is tested first
		{"US Army Example Command", classMil},
		{"Example Air Force Base", classMil},
		{"Example National Guard", classMil},
		{"Example Navy University", classMil},
		{"Admiral Networks", classOther},
		{"disaster recovery Inc", classOther}, // \bdisa\b needs the whole word
	}
	for _, c := range cases {
		if got := holderClass(c.name); got != c.want {
			t.Errorf("holderClass(%q) = %q, want %q", c.name, got, c.want)
		}
	}
	// The name is read up to 256 bytes only.
	long := strings.Repeat("x", 300) + " university"
	if got := holderClass(long); got != classOther {
		t.Errorf("a word after byte 256 counted: %q", got)
	}
}

func namedEntity(name, label string) string {
	return `{"objectClassName":"entity","roles":["registrant"],"vcardArray":["vcard",[["version",{},"text","4.0"],["fn",{},"text",` +
		fmt.Sprintf("%q", name) + `],["adr",{"label":` + label + `},"text",["","","","","","",""]]]]}`
}

func TestParseClass(t *testing.T) {
	asked := mustAddr("10.1.2.3")
	edu := namedEntity("Zebra State University", goodLabel)
	gov := namedEntity("City of Zebra", goodLabel)
	mil := namedEntity("Zebra Army Network", goodLabel)

	for name, c := range map[string]struct{ entity, class string }{
		"edu": {edu, classEdu}, "gov": {gov, classGov}, "mil": {mil, classMil},
		"other": {namedEntity("Zebra Cable", goodLabel), classOther},
	} {
		rec, kind := parseAnswer(rdapJSON("10.1.0.0", "10.1.255.255", `,"entities":[`+c.entity+`]`), asked)
		if kind != kindFound || rec.Class != c.class || rec.City != "Springfield" {
			t.Errorf("found %s: %v %+v", name, kind, rec)
		}
		rec, kind = parseAnswer(rdapJSON("10.0.0.0", "10.255.255.255", `,"entities":[`+c.entity+`]`), asked)
		if kind != kindWide || rec.Class != c.class || rec.City != "Springfield" || rec.State != "IL" || rec.Country != "US" {
			t.Errorf("wide %s: %v %+v", name, kind, rec)
		}
	}
	// No registrant name: other.
	if rec, _ := parseAnswer(rdapJSON("10.1.0.0", "10.1.255.255", `,"entities":[`+entity(`"registrant"`, goodLabel)+`]`), asked); rec.Class != classOther {
		t.Errorf("fn Example Customer: %+v", rec)
	}
	noName := `{"roles":["registrant"],"vcardArray":["vcard",[["adr",{"label":` + goodLabel + `},"text",[]]]]}`
	if rec, _ := parseAnswer(rdapJSON("10.1.0.0", "10.1.255.255", `,"entities":[`+noName+`]`), asked); rec.Class != classOther || rec.City != "Springfield" {
		t.Errorf("no name: %+v", rec)
	}
	// Exactly a /8 is kept with its city; one address more has only the range.
	rec, kind := parseAnswer(rdapJSON("10.0.0.0", "10.255.255.255", `,"entities":[`+edu+`]`), asked)
	if kind != kindWide || rec.City == "" {
		t.Errorf("/8: %v %+v", kind, rec)
	}
	rec, kind = parseAnswer(rdapJSON("9.255.255.255", "10.255.255.255", `,"entities":[`+edu+`]`), asked)
	if kind != kindWide || rec.City != "" || rec.Class != "" || rec.Start == 0 {
		t.Errorf("a /8 and one address: %v %+v", kind, rec)
	}
	// A wide answer without a usable city has no city (the holders file drops it).
	foreign := namedEntity("Zebra University", `"1 Road\nBerlin\nBE\n10115\nGermany"`)
	rec, kind = parseAnswer(rdapJSON("10.0.0.0", "10.255.255.255", `,"entities":[`+foreign+`]`), asked)
	if kind != kindWide || rec.City != "" {
		t.Errorf("wide, no city: %v %+v", kind, rec)
	}
	if got := holdersWanted([]answer{{rec, kind}}, nil); len(got) != 0 {
		t.Errorf("a wide answer without a city was wanted: %+v", got)
	}
	// The name is kept in no returned value: the record has no field that could hold it.
	allowed := map[string]bool{"Start": true, "End": true, "Country": true, "State": true, "City": true, "LookedUp": true, "Class": true}
	rt := reflect.TypeOf(record{})
	for i := 0; i < rt.NumField(); i++ {
		if !allowed[rt.Field(i).Name] {
			t.Errorf("record field %s", rt.Field(i).Name)
		}
	}
	rec, _ = parseAnswer(rdapJSON("10.1.0.0", "10.1.255.255", `,"entities":[`+edu+`]`), asked)
	if strings.Contains(strings.ToLower(fmt.Sprintf("%+v", rec)), "zebra") {
		t.Errorf("the name is in the record: %+v", rec)
	}
}

func hrec(start, end, state, city, class string) record {
	r := rec(start, end, "US", state, city)
	r.Class = class
	return r
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestHoldersWanted(t *testing.T) {
	answers := []answer{
		{hrec("10.0.0.0", "10.255.255.255", "IL", "Springfield", classOther), kindWide}, // wide: always
		{hrec("10.1.0.0", "10.1.255.255", "OR", "Portland", classEdu), kindFound},
		{hrec("10.2.0.0", "10.2.255.255", "OR", "Portland", classOther), kindFound}, // not edu or gov
		{hrec("10.3.0.0", "10.3.255.255", "OR", "Portland", classMil), kindFound},   // mil is only in the main records
		{hrec("10.4.0.0", "10.4.0.255", "OR", "Portland", classGov), kindFound},
		{hrec("10.5.0.0", "10.5.255.255", "OR", "", classEdu), kindFound},       // no city
		{hrec("10.6.0.0", "10.7.255.255", "IL", "", classEdu), kindWide},        // wide without a city
		{hrec("0.0.0.0", "10.0.0.0", "IL", "Springfield", classEdu), kindWide},  // wider than a /8
		{hrec("10.4.0.0", "10.4.0.255", "OR", "Portland", classEdu), kindFound}, // the range again: the later answer wins
		{record{}, kindNotFound},
	}
	got := holdersWanted(answers, nil)
	var text []string
	for _, r := range got {
		text = append(text, strings.Join(holderRowOf(r), ","))
	}
	want := "10.0.0.0,10.255.255.255,US,IL,Springfield,other,0 10.1.0.0,10.1.255.255,US,OR,Portland,edu,0 10.4.0.0,10.4.0.255,US,OR,Portland,edu,0"
	if strings.Join(text, " ") != want {
		t.Errorf("wanted\n%s\nwant\n%s", strings.Join(text, " "), want)
	}
}

func TestAppendAndCompactHolders(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "arin")
	path := filepath.Join(dir, holdersFile)
	// A missing file is an empty set and stays missing.
	if rows, pruned, err := compactHolders(dir, testNow); err != nil || len(rows) != 0 || pruned != 0 {
		t.Fatalf("missing: %v %d %v", rows, pruned, err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("compaction made a file")
	}
	day1, day2 := testNow.Add(-48*time.Hour), testNow.Add(-24*time.Hour)
	old := testNow.Add(-pruneAfter - time.Hour)
	if err := appendHolders(dir, nil, day1); err != nil || fileExists(path) {
		t.Fatalf("no rows must write nothing: %v", err)
	}
	if err := appendHolders(dir, []record{
		hrec("10.0.2.0", "10.0.2.255", "OR", "Portland", classEdu),
		hrec("10.0.1.0", "10.0.1.255", "OR", "Portland", classGov),
		hrec("10.0.9.0", "10.0.9.255", "TX", "Foo Town", classOther),
	}, day1); err != nil {
		t.Fatal(err)
	}
	if err := appendHolders(dir, []record{hrec("10.0.1.0", "10.0.1.255", "MO", "Springfield", classEdu)}, day2); err != nil {
		t.Fatal(err)
	}
	if err := appendHolders(dir, []record{hrec("10.0.5.0", "10.0.5.255", "MO", "Springfield", classEdu)}, old); err != nil {
		t.Fatal(err)
	}
	text := readFile(t, path)
	if !strings.HasPrefix(text, "net_start,net_end,country,state,city,class,looked_up\n") || strings.Count(text, "net_start") != 1 {
		t.Errorf("header: %q", text)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", fi.Mode())
	}
	// A torn last row (inside a quoted city), an unparsable row and a row with a bad class.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o640)
	f.WriteString("10.0.3.0,10.0.3.255,US,OR,not an address row\n")
	f.WriteString("x,y,US,OR,Portland,edu,1\n")
	f.WriteString("10.0.4.0,10.0.4.255,US,OR,Portland,weird,1\n")
	f.WriteString("10.0.3.0,10.0.3.255,US,\"OR,Port")
	f.Close()
	// The next append cuts the torn row off.
	if err := appendHolders(dir, []record{hrec("10.0.6.0", "10.0.6.255", "OR", "Portland", classGov)}, day2); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readFile(t, path), "Port\"") || strings.Contains(readFile(t, path), "\"OR,Port") {
		t.Error("the torn row stayed")
	}
	rows, pruned, err := compactHolders(dir, testNow)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%s %s %s %d", addrOf(r.Start), r.State, r.Class, r.LookedUp))
	}
	want := fmt.Sprintf("10.0.1.0 MO edu %d|10.0.2.0 OR edu %d|10.0.6.0 OR gov %d|10.0.9.0 TX other %d", day2.Unix(), day1.Unix(), day2.Unix(), day1.Unix())
	if strings.Join(got, "|") != want || pruned != 1 {
		t.Errorf("compacted\n%s\nwant\n%s\npruned %d", strings.Join(got, "|"), want, pruned)
	}
	// The file was written again: the same rows, no .tmp left, and compacting twice changes nothing.
	again, pruned2, err := compactHolders(dir, testNow)
	if err != nil || !reflect.DeepEqual(again, rows) || pruned2 != 0 {
		t.Errorf("second compaction: %v %d %v", again, pruned2, err)
	}
	if fileExists(path + ".tmp") {
		t.Error("a .tmp file is left")
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestHoldersAppendAfterTornFirstRow(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, holdersFile), []byte("net_start,net_end,country,state,city,cl"), 0o640)
	if err := appendHolders(dir, []record{hrec("10.0.1.0", "10.0.1.255", "OR", "Portland", classGov)}, testNow); err != nil {
		t.Fatal(err)
	}
	rows, _, err := compactHolders(dir, testNow)
	if err != nil || len(rows) != 1 {
		t.Errorf("%v %v", rows, err)
	}
}

func TestCompactHoldersUnreadable(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, holdersFile), 0o750) // a directory where the file should be
	if _, _, err := compactHolders(dir, testNow); err == nil {
		t.Error("an unreadable file is no error")
	}
}

// The holders go to disk before the journal and the position: a batch whose save fails has its
// holders in the file and the position unmoved; a rerun appends them again and compaction merges.
func TestLookupHoldersBeforePosition(t *testing.T) {
	srv := newRDAP(t, func(w http.ResponseWriter, octet, n int) {
		serve(w, rdapJSON("192.0.0.0", "192.255.255.255", `,"entities":[`+namedEntity("Zebra University", goodLabel)+`]`))
	})
	e := newEnv(t, srv, 8)
	e.db.saveErr = errors.New("disk full")
	if _, err := e.p.Lookup(context.Background()); err == nil {
		t.Fatal("the save error was lost")
	}
	if e.db.cursor != 0 || e.db.hasRow {
		t.Errorf("the position moved: %d", e.db.cursor)
	}
	text := readFile(t, filepath.Join(e.p.dir(), holdersFile))
	if strings.Count(text, ",Springfield,edu,") != 1 {
		t.Errorf("holders after a failed save: %q", text)
	}
	if strings.Contains(strings.ToLower(text), "zebra") {
		t.Errorf("the name is in the file: %q", text)
	}
	// The rerun appends the rows again; compaction keeps one per range.
	e.db.saveErr = nil
	if _, err := e.p.Lookup(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, _, err := compactHolders(e.p.dir(), testNow)
	if err != nil || len(rows) != 1 || rows[0].Class != classEdu {
		t.Errorf("rows %+v %v", rows, err)
	}
}

// A stored wide record skips no /24 of the walk: every gap /24 is still asked.
func TestLookupNoSkipInsideWide(t *testing.T) {
	srv := newRDAP(t, func(w http.ResponseWriter, octet, n int) {
		serve(w, rdapJSON("192.0.0.0", "192.255.255.255", `,"entities":[`+namedEntity("Example College", goodLabel)+`]`))
	})
	e := newEnv(t, srv, 8)
	out, err := e.p.Lookup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Targets != 8 || len(srv.paths) != 8 || out.Wide != 8 || out.Found != 0 || !out.CycleDone {
		t.Errorf("%+v asked %d", out, len(srv.paths))
	}
	// One holder row per batch (3 batches), all class edu; the main records get none.
	if out.HoldersStored != 3 || out.HoldersEdu != 3 || out.HoldersGov+out.HoldersMil+out.HoldersOther != 0 || out.Stored != 0 {
		t.Errorf("holders %+v", out)
	}
	if n := strings.Count(readFile(t, filepath.Join(e.p.dir(), holdersFile)), "\n"); n != 4 { // header + 3
		t.Errorf("holders file has %d lines", n)
	}
	e.noAddress(t, out)
}

func TestLookupHolderClasses(t *testing.T) {
	names := map[int]string{0: "Zebra University", 1: "City of Zebra", 2: "Zebra Army", 3: "Zebra Cable"}
	srv := newRDAP(t, func(w http.ResponseWriter, octet, n int) {
		serve(w, rdapJSON(fmt.Sprintf("192.0.%d.0", octet), fmt.Sprintf("192.0.%d.255", octet), `,"entities":[`+namedEntity(names[octet], goodLabel)+`]`))
	})
	e := newEnv(t, srv, 4)
	out, err := e.p.Lookup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Found != 4 || out.Stored != 4 || out.HoldersStored != 2 || out.HoldersEdu != 1 || out.HoldersGov != 1 || out.HoldersMil != 0 || out.HoldersOther != 0 {
		t.Errorf("%+v", out)
	}
}

func TestBuildRowsWithHolders(t *testing.T) {
	idx := testIndex(t)
	var out ExportOutput
	holders := []record{
		hrec("10.0.0.0", "10.0.127.255", "OR", "Portland", classEdu),    // an edu /17: rows
		hrec("10.1.0.0", "10.1.127.255", "OR", "Portland", classOther),  // other: none
		hrec("10.2.0.0", "10.2.127.255", "OR", "Portland", classMil),    // mil: none
		hrec("10.4.0.0", "10.5.255.255", "OR", "Portland", classEdu),    // wider than a /16: none
		hrec("10.6.0.0", "10.6.127.255", "IL", "Springfield", classGov), // the name is Springfield, MO: another state
		hrec("10.7.0.0", "10.7.127.255", "MO", "Springfield", classGov), // fine
		hrec("10.8.0.0", "10.8.127.255", "ZZ", "Nowhere", classEdu),     // unplaced
		hrec("10.9.0.0", "10.9.127.255", "OR", "", classEdu),            // no city
		hrec("10.10.0.0", "10.10.127.255", "OH", "Delta", classEdu),     // ambiguous
		hrec("10.11.0.0", "10.11.0.255", "OR", "Portland", classGov),    // a /24 from holders; the main record is the same
	}
	recs := []record{rec("10.11.0.0", "10.11.0.255", "US", "OR", "Portland")}
	rows := buildRows(recs, holders, idx, &out)
	var got []string
	for _, r := range rows {
		got = append(got, strings.Join([]string{r.prefix, r.state, r.city}, ","))
	}
	want := []string{
		"10.0.0.0/17,OR,Portland",
		"10.7.0.0/17,MO,Springfield",
		"10.11.0.0/24,OR,Portland",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rows\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Candidates: edu or gov, at most a /16, with a city: 10.0, 10.6, 10.7, 10.8, 10.10, 10.11.
	if out.Records != 1 || out.HolderCandidates != 6 || out.HolderRows != 2 || out.Unplaced != 1 || out.OtherState != 1 || out.Ambiguous != 1 {
		t.Errorf("counts %+v", out)
	}
}

func TestExportWithHolders(t *testing.T) {
	geo := newGeo(t, zipOf(t, testCities), 200)
	e := newEnv(t, nil, 8)
	e.p.geoURL, e.p.dlClient = geo.URL, geo.Client()
	os.MkdirAll(e.p.dir(), 0o750)
	day := testNow.Add(-24 * time.Hour)
	appendHolders(e.p.dir(), []record{
		hrec("10.0.0.0", "10.0.127.255", "OR", "Portland", classEdu),
		hrec("10.1.0.0", "10.1.127.255", "OR", "Portland", classOther),
		hrec("10.4.0.0", "10.255.255.255", "OR", "Portland", classEdu),
	}, day)
	appendHolders(e.p.dir(), []record{hrec("10.2.0.0", "10.2.255.255", "OR", "Portland", classGov)}, testNow.Add(-pruneAfter-time.Hour))
	out, err := e.p.Export(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Skipped != "" || out.Rows != 1 || out.HolderRows != 1 || out.HolderRecords != 3 || out.HolderCandidates != 1 || out.HoldersPruned != 1 || out.Records != 0 || out.HoldersUnreadable {
		t.Errorf("%+v", out)
	}
	if got := readLayer(t, e.p.LayerPath()); got != "prefix,country,region,city,source\n10.0.0.0/17,US,OR,Portland,whois-city:arin\n" {
		t.Errorf("layer %q", got)
	}
	// Holders alone are enough for a layer, but class other gives none: the layer file goes.
	os.Remove(filepath.Join(e.p.dir(), holdersFile))
	appendHolders(e.p.dir(), []record{hrec("10.1.0.0", "10.1.127.255", "OR", "Portland", classOther)}, day)
	if out, err = e.p.Export(context.Background(), true); err != nil || out.Skipped != "no rows" || fileExists(e.p.LayerPath()) {
		t.Errorf("no rows: %+v %v", out, err)
	}
	// An unreadable holders file leaves the layer as it is without holders.
	os.Remove(filepath.Join(e.p.dir(), holdersFile))
	os.Mkdir(filepath.Join(e.p.dir(), holdersFile), 0o750)
	e.db.nets = []record{rec("10.0.1.0", "10.0.1.255", "US", "OR", "Portland")}
	if out, err = e.p.Export(context.Background(), true); err != nil || !out.HoldersUnreadable || out.Rows != 1 || out.HolderRecords != 0 {
		t.Errorf("unreadable: %+v %v", out, err)
	}
	e.noAddress(t, out)
}

type putCall struct {
	key  string
	data []byte
	opts publish.PutOptions
}

func TestUploadHolders(t *testing.T) {
	var calls []putCall
	status := publish.PutUploaded
	var putErr error
	now := testNow
	p := &Pipeline{DataDir: t.TempDir(), now: func() time.Time { return now }, upload: func(_ context.Context, key, path string, opts publish.PutOptions) (string, publish.PutStatus, error) {
		data, _ := os.ReadFile(path)
		calls = append(calls, putCall{key, data, opts})
		return "", status, putErr
	}}
	// No holders file: nothing is sent for it.
	out, err := p.Upload(context.Background(), false)
	if err != nil || out.HoldersBackup != "" || len(calls) != 0 {
		t.Fatalf("no file: %+v %v", out, err)
	}
	if err := appendHolders(p.dir(), []record{hrec("10.0.1.0", "10.0.1.255", "OR", "Portland", classGov)}, testNow); err != nil {
		t.Fatal(err)
	}
	out, err = p.Upload(context.Background(), false)
	if err != nil || out.HoldersBackup != backupUploaded || len(calls) != 1 || calls[0].key != "internal/arin/holders.csv.gz" {
		t.Fatalf("first: %+v %v %v", out, err, calls)
	}
	if fileExists(filepath.Join(p.dir(), holdersGzFile)) {
		t.Error("the gz copy stays")
	}
	first := calls[0].data
	// Less than a day later: not again, unless forced. After a day it is sent again with the same bytes.
	now = testNow.Add(time.Hour)
	if out, _ = p.Upload(context.Background(), false); out.HoldersBackup != backupRecent || len(calls) != 1 {
		t.Errorf("recent: %+v", out)
	}
	now = testNow.Add(25 * time.Hour)
	status = publish.PutUnchanged
	if out, _ = p.Upload(context.Background(), false); out.HoldersBackup != backupUnchanged || len(calls) != 2 || string(calls[1].data) != string(first) {
		t.Errorf("a day later: %+v", out)
	}
	// An R2 problem is only counted.
	now = testNow.Add(60 * time.Hour)
	putErr = errors.New("boom")
	if out, err = p.Upload(context.Background(), false); err != nil || out.HoldersBackup != holdersFailed {
		t.Errorf("R2 failure: %+v %v", out, err)
	}
}

func TestRestoreHolders(t *testing.T) {
	text := "net_start,net_end,country,state,city,class,looked_up\n10.0.1.0,10.0.1.255,US,OR,Portland,gov,5\nbroken row\n"
	r2 := &fakeR2{objects: map[string][]byte{HoldersKey: gz(t, text)}}
	e := newEnv(t, nil, 8)
	e.p.objects = r2
	os.MkdirAll(e.p.dir(), 0o750)
	if got := e.p.restoreHolders(context.Background()); got != holdersRestored {
		t.Fatalf("restore: %q", got)
	}
	data := readFile(t, filepath.Join(e.p.dir(), holdersFile))
	if !strings.Contains(data, "Portland") || strings.Contains(data, "broken") {
		t.Errorf("restored file %q", data)
	}
	// The file is there: R2 is not asked.
	r2.stats = 0
	if got := e.p.restoreHolders(context.Background()); got != "" || r2.stats != 0 {
		t.Errorf("file present: %q stats %d", got, r2.stats)
	}
	// No copy in R2: nothing, no warning counted.
	os.Remove(filepath.Join(e.p.dir(), holdersFile))
	delete(r2.objects, HoldersKey)
	if got := e.p.restoreHolders(context.Background()); got != "" {
		t.Errorf("no copy: %q", got)
	}
	// R2 down, a damaged copy and a failed download: failed, no file, the task goes on.
	r2.statErr = errors.New("down")
	if got := e.p.restoreHolders(context.Background()); got != holdersFailed || fileExists(filepath.Join(e.p.dir(), holdersFile)) {
		t.Errorf("stat error: %q", got)
	}
	r2.statErr = nil
	r2.objects[HoldersKey] = []byte("not gzip")
	if got := e.p.restoreHolders(context.Background()); got != holdersFailed || fileExists(filepath.Join(e.p.dir(), holdersFile)) {
		t.Errorf("damaged: %q", got)
	}
	r2.objects[HoldersKey] = gz(t, text)
	r2.downloadErr = errors.New("cut")
	if got := e.p.restoreHolders(context.Background()); got != holdersFailed {
		t.Errorf("download error: %q", got)
	}
	// A lookup restores it too and reports it.
	r2.downloadErr = nil
	srv := newRDAP(t, statusHandler)
	e.p.base = srv.URL
	e.p.client = srv.Client()
	out, err := e.p.Lookup(context.Background())
	if err != nil || out.HoldersRestore != holdersRestored {
		t.Errorf("lookup: %+v %v", out, err)
	}
	for _, l := range e.logs {
		if strings.Contains(l, "Portland") {
			t.Errorf("a city in a log line: %q", l)
		}
	}
}
