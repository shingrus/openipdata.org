package ping

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openipdata.org/hatchet/publish"
)

const header = "prefix,DE/Frankfurt,GB/London,US/New York,FR/Paris\n"

func gz(t *testing.T, path, body string) {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(body))
	w.Close()
	if err := os.WriteFile(path, b.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
}

// rowsFor makes n sorted valid rows from 20.0.0.0/24 up.
func rowsFor(n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "20.%d.%d.0/24,12.5,,30,4.25\n", i>>8&255, i&255)
	}
	return sb.String()
}

type putCall struct {
	key, path string
	opts      publish.PutOptions
}

func newPipe(t *testing.T) (*Pipeline, *[]putCall) {
	t.Helper()
	var calls []putCall
	p := &Pipeline{DataDir: t.TempDir(), minRows: 10, upload: func(_ context.Context, key, path string, opts publish.PutOptions) (string, publish.PutStatus, error) {
		calls = append(calls, putCall{key, path, opts})
		return "r2://b/" + key, publish.PutUploaded, nil
	}}
	if err := os.MkdirAll(p.inbox(), 0o750); err != nil {
		t.Fatal(err)
	}
	return p, &calls
}

func TestNames(t *testing.T) {
	if RTTFile != "hosting-rtt.csv.gz" || RTTKey != "internal/ping/hosting-rtt.csv.gz" {
		t.Error("names")
	}
}

func TestFindInput(t *testing.T) {
	dir := t.TempDir()
	if _, found, err := findInput(filepath.Join(dir, "nope"), ""); found || err != nil {
		t.Errorf("no inbox: %v %v", found, err)
	}
	if _, found, err := findInput(dir, ""); found || err != nil {
		t.Errorf("empty inbox: %v %v", found, err)
	}
	old, new := filepath.Join(dir, "a.csv.gz"), filepath.Join(dir, "b.csv.gz")
	os.WriteFile(old, []byte("x"), 0o640)
	os.WriteFile(new, []byte("x"), 0o640)
	os.WriteFile(filepath.Join(dir, "c.csv"), []byte("x"), 0o640)
	os.WriteFile(filepath.Join(dir, ".d.csv.gz"), []byte("x"), 0o640)
	os.Chtimes(old, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	got, found, err := findInput(dir, "")
	if err != nil || !found || got != new {
		t.Errorf("newest: %q %v %v", got, found, err)
	}
	if got, found, err = findInput(dir, "a.csv.gz"); err != nil || !found || got != old {
		t.Errorf("named: %q %v %v", got, found, err)
	}
	for _, name := range []string{"missing.csv.gz", "../a.csv.gz", "sub/a.csv.gz", "c.csv", "a.csv.gz\x00", "..", "a.gz"} {
		if _, _, err := findInput(dir, name); !errors.Is(err, ErrPermanent) {
			t.Errorf("%q: %v", name, err)
		}
	}
	os.Symlink(old, filepath.Join(dir, "link.csv.gz"))
	if _, _, err := findInput(dir, "link.csv.gz"); !errors.Is(err, ErrPermanent) {
		t.Errorf("symlink: %v", err)
	}
}

func TestUploadNoFile(t *testing.T) {
	p, calls := newPipe(t)
	out, err := p.Upload(context.Background(), "", Options{})
	if err != nil || out.Skipped != "no file" || len(*calls) != 0 {
		t.Errorf("%+v %v", out, err)
	}
	if _, err := p.Upload(context.Background(), "x.csv.gz", Options{}); !errors.Is(err, ErrPermanent) {
		t.Errorf("named missing: %v", err)
	}
}

func TestUpload(t *testing.T) {
	p, calls := newPipe(t)
	path := filepath.Join(p.inbox(), "hosting-rtt.csv.gz")
	gz(t, path, header+rowsFor(50))
	out, err := p.Upload(context.Background(), "", Options{})
	if err != nil || out.Rows != 50 || out.Valid != 50 || out.Sites != 4 || out.Upload != "uploaded" || out.Bad() != 0 || out.File != "hosting-rtt.csv.gz" || out.Bytes == 0 {
		t.Fatalf("%+v %v", out, err)
	}
	c := (*calls)[0]
	if len(*calls) != 1 || c.key != RTTKey || c.path != path || c.opts.Refresh || c.opts.Force {
		t.Errorf("call %+v", *calls)
	}
	// Named file, force.
	if _, err := p.Upload(context.Background(), "hosting-rtt.csv.gz", Options{ForceUpload: true}); err != nil || !(*calls)[1].opts.Force || (*calls)[1].opts.Refresh {
		t.Errorf("force: %v %+v", err, *calls)
	}
	// Skip.
	out, err = p.Upload(context.Background(), "", Options{SkipUpload: true})
	if err != nil || out.Upload != "skipped" || len(*calls) != 2 {
		t.Errorf("skip: %+v %v", out, err)
	}
	// Local path wins over the inbox.
	other := filepath.Join(t.TempDir(), "other.csv.gz")
	gz(t, other, header+rowsFor(20))
	out, err = p.Upload(context.Background(), "", Options{Path: other})
	if err != nil || out.File != "other.csv.gz" || (*calls)[2].path != other {
		t.Errorf("path: %+v %v", out, err)
	}
	// An upload error fails the task.
	p.upload = func(context.Context, string, string, publish.PutOptions) (string, publish.PutStatus, error) {
		return "", "", errors.New("boom")
	}
	if _, err := p.Upload(context.Background(), "", Options{}); err == nil || errors.Is(err, ErrPermanent) {
		t.Errorf("upload error: %v", err)
	}
}

func TestCheckUpload(t *testing.T) {
	if err := (&Pipeline{}).CheckUpload(); err == nil {
		t.Error("an empty R2 setting is accepted")
	}
}

// Every reason has a counter; a row that is bad for two reasons is counted once, by the first.
func TestBadRows(t *testing.T) {
	p, _ := newPipe(t)
	p.minRows = 1
	path := filepath.Join(p.inbox(), "x.csv.gz")
	body := header + rowsFor(2000) + // 20.0.0.0 .. 20.7.207.0
		"30.0.0.0/24,1,2\n" + // bad_columns
		"30.0.0.0/24,1,2,3,4,5\n" + // bad_columns
		strings.Repeat("a", 5000) + "\n" + // bad_columns (too long)
		"30.0.0.1/24,1,2,3,4\n" + // bad_prefix (host bits)
		"30.0.0.0/23,1,2,3,4\n" + // bad_prefix (length)
		"30.0.0.0,1,2,3,4\n" + // bad_prefix (no length)
		"030.0.0.0/24,1,2,3,4\n" + // bad_prefix (leading zero)
		"2001:db8::/24,1,2,3,4\n" + // bad_prefix (IPv6)
		"10.0.0.0/24,1,2,3,4\n" + // not_public
		"192.168.1.0/24,1,2,3,4\n" + // not_public
		"19.0.0.0/24,1,2,3,4\n" + // not_sorted (before 20.0.0.0)
		"21.0.0.0/24,1,2,3,4\n" + // ok
		"21.0.0.0/24,1,2,3,4\n" + // not_sorted (duplicate)
		"22.0.0.0/24,0,2,3,4\n" + // bad_rtt (zero)
		"22.0.1.0/24,-1,2,3,4\n" + // bad_rtt
		"22.0.2.0/24,2000.5,2,3,4\n" + // bad_rtt (over limit)
		"22.0.3.0/24,abc,2,3,4\n" + // bad_rtt
		"22.0.4.0/24,NaN,2,3,4\n" + // bad_rtt
		"22.0.5.0/24,1e2,2,3,4\n" + // bad_rtt
		"22.0.6.0/24,1.2.3,2,3,4\n" + // bad_rtt
		"22.0.7.0/24, 5,2,3,4\n" + // bad_rtt (space)
		"22.0.8.0/24,2000,2,3,4\n" + // ok (the limit)
		"22.0.9.0/24,,,3,4\n" + // few_sites
		"22.0.10.0/24,,,,\n" + // few_sites
		"22.0.11.0/24,1,2,,4\n" + // few_sites? no: 3 answers is ok
		"\n" // an empty line is nothing
	gz(t, path, body)
	var out Output
	if err := p.check(context.Background(), mustOpen(t, path), &out); err != nil {
		t.Fatal(err)
	}
	if out.BadColumns != 3 || out.BadPrefix != 5 || out.NotPublic != 2 || out.NotSorted != 2 || out.BadRTT != 8 || out.FewSites != 2 {
		t.Errorf("%+v", out)
	}
	if out.Valid != 2000+1+1+1 { // 21.0.0.0, 22.0.8.0 and 22.0.11.0
		t.Errorf("valid %d", out.Valid)
	}
	if out.Rows != out.Valid+out.Bad() {
		t.Errorf("rows %d != valid %d + bad %d", out.Rows, out.Valid, out.Bad())
	}
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestRefusals(t *testing.T) {
	p, calls := newPipe(t)
	path := filepath.Join(p.inbox(), "x.csv.gz")
	ctx := context.Background()
	// Fewer valid rows than the minimum.
	gz(t, path, header+rowsFor(9))
	if _, err := p.Upload(ctx, "", Options{}); !errors.Is(err, ErrPermanent) {
		t.Errorf("minimum: %v", err)
	}
	// Exactly 1% bad is accepted, more is not.
	gz(t, path, header+rowsFor(99)+"10.0.0.0/24,1,2,3,4\n")
	if out, err := p.Upload(ctx, "", Options{}); err != nil || out.Bad() != 1 {
		t.Errorf("1%%: %+v %v", out, err)
	}
	gz(t, path, header+rowsFor(98)+"10.0.0.0/24,1,2,3,4\n10.0.0.1/24,1,2,3,4\n")
	out, err := p.Upload(ctx, "", Options{})
	if !errors.Is(err, ErrPermanent) || out.Valid != 98 || out.Bad() != 2 {
		t.Errorf("over 1%%: %+v %v", out, err)
	}
	if len(*calls) != 1 {
		t.Errorf("uploads: %d, want only the accepted file", len(*calls))
	}
}

func TestHeaderErrors(t *testing.T) {
	p, calls := newPipe(t)
	path := filepath.Join(p.inbox(), "x.csv.gz")
	rows := rowsFor(50)
	sites := func(n int) string {
		s := "prefix"
		for i := 0; i < n; i++ {
			s += fmt.Sprintf(",DE/City%d", i)
		}
		return s + "\n"
	}
	bad := map[string]string{
		"no prefix":      "net,DE/A,GB/B,US/C\n",
		"two sites":      "prefix,DE/A,GB/B\n",
		"65 sites":       sites(65),
		"lower case":     "prefix,de/A,GB/B,US/C\n",
		"no slash":       "prefix,DE-A,GB/B,US/C\n",
		"three letters":  "prefix,DEU/A,GB/B,US/C\n",
		"empty city":     "prefix,DE/,GB/B,US/C\n",
		"long city":      "prefix,DE/" + strings.Repeat("a", 65) + ",GB/B,US/C\n",
		"angle bracket":  "prefix,DE/<A>,GB/B,US/C\n",
		"control":        "prefix,DE/A\x01,GB/B,US/C\n",
		"bad utf8":       "prefix,DE/\xff,GB/B,US/C\n",
		"duplicate":      "prefix,DE/A,GB/B,DE/A\n",
		"empty file":     "",
		"prefix quoted":  "\"prefix\",DE/A,GB/B,US/C\n",
		"trailing comma": "prefix,DE/A,GB/B,US/C,\n",
	}
	for name, h := range bad {
		gz(t, path, h+rows)
		_, err := p.Upload(context.Background(), "", Options{})
		if !errors.Is(err, ErrPermanent) {
			t.Errorf("%s: %v", name, err)
		} else if strings.Contains(err.Error(), "DE/A") || strings.Contains(err.Error(), "GB/B") || strings.Contains(err.Error(), "net") {
			t.Errorf("%s: the error echoes the header: %v", name, err)
		}
	}
	// 3 and 64 sites are fine; a 64-byte city and a non-ASCII city too.
	gz(t, path, "prefix,DE/A,GB/B,US/C\n"+strings.Repeat("20.0.0.0/24,1,2,3\n", 0)+rowsForN(50, 3))
	if _, err := p.Upload(context.Background(), "", Options{}); err != nil {
		t.Errorf("3 sites: %v", err)
	}
	h64 := sites(64)
	h64 = strings.Replace(h64, "DE/City0", "DE/"+strings.Repeat("é", 32), 1)
	gz(t, path, h64+rowsForN(50, 64))
	if out, err := p.Upload(context.Background(), "", Options{}); err != nil || out.Sites != 64 {
		t.Errorf("64 sites: %+v %v", out, err)
	}
	if len(*calls) != 2 {
		t.Errorf("uploads %d", len(*calls))
	}
}

// rowsForN makes n rows with sites cells, all answered.
func rowsForN(n, sites int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "20.0.%d.0/24%s\n", i, strings.Repeat(",9.5", sites))
	}
	return sb.String()
}

func TestDamagedAndNotGzip(t *testing.T) {
	p, _ := newPipe(t)
	path := filepath.Join(p.inbox(), "x.csv.gz")
	os.WriteFile(path, []byte("prefix,DE/A,GB/B,US/C\n"), 0o640)
	if _, err := p.Upload(context.Background(), "", Options{}); !errors.Is(err, ErrPermanent) {
		t.Errorf("not gzip: %v", err)
	}
	// Cut short.
	gz(t, path, header+rowsFor(5000))
	data, _ := os.ReadFile(path)
	os.WriteFile(path, data[:len(data)-30], 0o640)
	if _, err := p.Upload(context.Background(), "", Options{}); !errors.Is(err, ErrPermanent) {
		t.Errorf("cut short: %v", err)
	}
}

func TestTooLarge(t *testing.T) {
	p, calls := newPipe(t)
	path := filepath.Join(p.inbox(), "x.csv.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxFileBytes + 1); err != nil { // sparse
		t.Skip("no sparse files")
	}
	f.Close()
	if _, err := p.Upload(context.Background(), "", Options{}); !errors.Is(err, ErrPermanent) || len(*calls) != 0 {
		t.Errorf("too large: %v", err)
	}
}

func TestCancelled(t *testing.T) {
	p, _ := newPipe(t)
	path := filepath.Join(p.inbox(), "x.csv.gz")
	gz(t, path, header+rowsFor(70000))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Upload(ctx, "", Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("%v", err)
	}
}

func TestProgressLogsCounts(t *testing.T) {
	p, _ := newPipe(t)
	var logs []string
	p.Progress = func(s string) { logs = append(logs, s) }
	path := filepath.Join(p.inbox(), "x.csv.gz")
	gz(t, path, header+rowsFor(50))
	if _, err := p.Upload(context.Background(), "", Options{}); err != nil {
		t.Fatal(err)
	}
	if len(logs) == 0 || strings.Contains(strings.Join(logs, "\n"), "20.0.") {
		t.Errorf("logs %v", logs)
	}
}
