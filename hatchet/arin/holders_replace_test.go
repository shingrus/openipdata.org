package arin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openipdata.org/hatchet/publish"
)

func TestHoldersWantedKnownRange(t *testing.T) {
	r16 := func(third string) (uint32, uint32) {
		return u32(mustAddr("10." + third + ".0.0")), u32(mustAddr("10." + third + ".255.255"))
	}
	s1, e1 := r16("1")
	_, _ = r16("2")
	s3, e3 := r16("3")
	set := holderSet{{s1, e1}: {}}
	answers := []answer{
		{record{Start: s1, End: e1, Class: classOther}, kindFound},                  // known, now no city: written, switches the row off
		{hrec("10.2.0.0", "10.2.255.255", "OR", "Portland", classOther), kindFound}, // unknown, other: no
		{record{Start: s3, End: e3, Class: classEdu}, kindFound},                    // unknown, no city: no
	}
	got := holdersWanted(answers, set)
	if len(got) != 1 || got[0].Start != s1 || got[0].Class != classOther || got[0].City != "" || got[0].State != "" {
		t.Errorf("wanted %+v", got)
	}
	set.note(got)
	if len(set) != 0 {
		t.Errorf("the set still holds the range: %v", set)
	}
	set.note([]record{hrec("10.4.0.0", "10.4.255.255", "OR", "Portland", classGov)})
	if len(set) != 1 {
		t.Errorf("a gov row is not in the set: %v", set)
	}
}

// An edu row is switched off by a later answer of the same range with another class.
func TestHolderTombstone(t *testing.T) {
	geo := newGeo(t, zipOf(t, testCities), 200)
	e := newEnv(t, nil, 8)
	e.p.geoURL, e.p.dlClient = geo.URL, geo.Client()
	os.MkdirAll(e.p.dir(), 0o750)
	start, end := u32(mustAddr("10.0.0.0")), u32(mustAddr("10.0.127.255"))
	appendHolders(e.p.dir(), []record{hrec("10.0.0.0", "10.0.127.255", "OR", "Portland", classEdu)}, testNow.Add(-48*time.Hour))
	out, err := e.p.Export(context.Background(), true)
	if err != nil || out.HolderRows != 1 {
		t.Fatalf("first export %+v %v", out, err)
	}
	set := loadHolderSet(e.p.dir())
	if len(set) != 1 {
		t.Fatalf("set %v", set)
	}
	later := holdersWanted([]answer{{record{Start: start, End: end, Class: classOther}, kindFound}}, set)
	if len(later) != 1 {
		t.Fatal("no row for the later answer")
	}
	if err := appendHolders(e.p.dir(), later, testNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	out, err = e.p.Export(context.Background(), true)
	if err != nil || out.Skipped != "no rows" || out.HolderRecords != 1 || out.HolderCandidates != 0 || fileExists(e.p.LayerPath()) {
		t.Errorf("after the tombstone %+v %v", out, err)
	}
	if n := len(loadHolderSet(e.p.dir())); n != 0 {
		t.Errorf("the range is still in the set (%d)", n)
	}
}

func TestBuildRowsSameRangeLaterWins(t *testing.T) {
	idx := testIndex(t)
	main := rec("10.0.0.0", "10.0.15.255", "US", "OR", "Portland")
	hold := hrec("10.0.0.0", "10.0.15.255", "MO", "Springfield", classEdu)
	for _, c := range []struct {
		mainAt, holdAt int64
		want           string
		holderRows     int
	}{{100, 200, "Springfield", 1}, {200, 100, "Portland", 0}, {100, 100, "Portland", 0}} {
		m, h := main, hold
		m.LookedUp, h.LookedUp = c.mainAt, c.holdAt
		var out ExportOutput
		rows := buildRows([]record{m}, []record{h}, idx, &out)
		if len(rows) != 1 || rows[0].city != c.want || out.HolderRows != c.holderRows {
			t.Errorf("%+v: %+v %+v", c, rows, out)
		}
	}
}

func TestRestoreFailedThenBatch(t *testing.T) {
	text := "net_start,net_end,country,state,city,class,looked_up\n10.0.1.0,10.0.1.255,US,OR,Portland,gov,5\n"
	r2 := &fakeR2{objects: map[string][]byte{HoldersKey: gz(t, text)}, statErr: errors.New("down")}
	srv := newRDAP(t, func(w http.ResponseWriter, octet, n int) {
		serve(w, rdapJSON("192.0.0.0", "192.255.255.255", `,"entities":[`+namedEntity("Example College", goodLabel)+`]`))
	})
	e := newEnv(t, srv, 8)
	e.p.objects = r2
	path := filepath.Join(e.p.dir(), holdersFile)
	out, err := e.p.Lookup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.HoldersRestore != holdersFailed || out.HoldersStored != 0 || out.HoldersSkipped != 3 || out.Wide != 8 || out.Batches != 3 || fileExists(path) {
		t.Errorf("failed restore: %+v", out)
	}
	if e.db.cursor != 256 {
		t.Errorf("the rest of the run did not go on: %d", e.db.cursor)
	}
	// Part files of a killed download are removed before the next restore.
	part := filepath.Join(e.p.dir(), holdersGzFile+".restore.abc.part.minio")
	os.WriteFile(part, []byte("x"), 0o640)
	// The next run restores, then appends.
	r2.statErr = nil
	e.db.hasRow, e.db.cursor = false, 0
	out, err = e.p.Lookup(context.Background())
	if err != nil || out.HoldersRestore != holdersRestored || out.HoldersStored != 3 || out.HoldersSkipped != 0 {
		t.Fatalf("second run: %+v %v", out, err)
	}
	if fileExists(part) {
		t.Error("a part file stays")
	}
	data := readFile(t, path)
	if !strings.Contains(data, "Portland") || strings.Count(data, "\n") != 5 { // header, restored row, 3 new
		t.Errorf("holders file %q", data)
	}
	var st backupState
	raw := readFile(t, filepath.Join(e.p.dir(), holdersBackupFile))
	if err := json.Unmarshal([]byte(raw), &st); err != nil || st.Records != 1 {
		t.Errorf("state after the restore: %q", raw)
	}
}

func TestHoldersBackupRefusesSmallFile(t *testing.T) {
	calls := 0
	now := testNow
	p := &Pipeline{DataDir: t.TempDir(), now: func() time.Time { return now }, upload: func(context.Context, string, string, publish.PutOptions) (string, publish.PutStatus, error) {
		calls++
		return "", publish.PutUploaded, nil
	}}
	rows := func(n int) {
		os.Remove(filepath.Join(p.dir(), holdersFile))
		var recs []record
		for i := 0; i < n; i++ {
			recs = append(recs, hrec(fmt.Sprintf("10.0.%d.0", i), fmt.Sprintf("10.0.%d.255", i), "OR", "Portland", classGov))
		}
		if err := appendHolders(p.dir(), recs, testNow); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(p.dir(), holdersBackupFile)
	rows(10)
	if out, _ := p.Upload(context.Background(), false); out.HoldersBackup != backupUploaded {
		t.Fatalf("first: %+v", out)
	}
	before := readFile(t, state)
	rows(4) // less than half of 10
	now = testNow.Add(48 * time.Hour)
	n := calls
	for _, force := range []bool{false, true} {
		out, err := p.Upload(context.Background(), force)
		if err != nil || out.HoldersBackup != backupRefused || calls != n {
			t.Errorf("force %v: %+v %v", force, out, err)
		}
	}
	if readFile(t, state) != before {
		t.Error("the state file changed on a refusal")
	}
	rows(5) // exactly half is accepted
	if out, _ := p.Upload(context.Background(), false); out.HoldersBackup != backupUploaded || calls != n+1 {
		t.Errorf("half: %+v", out)
	}
	rows(1)
	now = testNow.Add(96 * time.Hour)
	if out, _ := p.Upload(context.Background(), true); out.HoldersBackup != backupRefused {
		t.Errorf("1 of 5: %+v", out)
	}
	os.Remove(state) // on purpose
	if out, _ := p.Upload(context.Background(), false); out.HoldersBackup != backupUploaded {
		t.Errorf("after deleting the state: %+v", out)
	}
}
