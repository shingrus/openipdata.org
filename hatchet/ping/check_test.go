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

func checkPipe(t *testing.T) (*Pipeline, string) {
	t.Helper()
	return &Pipeline{DataDir: t.TempDir()}, filepath.Join(t.TempDir(), "x.csv.gz")
}

func TestNames(t *testing.T) {
	if RTTFile != "hosting-rtt.csv.gz" || RTTKey != "internal/ping/hosting-rtt.csv.gz" {
		t.Error("names")
	}
}

func TestCheckFile(t *testing.T) {
	p, path := checkPipe(t)
	gz(t, path, header+rowsFor(50))
	out, err := p.checkFile(context.Background(), path, 10)
	if err != nil || out.Rows != 50 || out.Valid != 50 || out.Sites != 4 || out.Bad() != 0 || out.Bytes == 0 {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := p.checkFile(context.Background(), filepath.Join(t.TempDir(), "none"), 10); !errors.Is(err, ErrPermanent) {
		t.Errorf("missing file: %v", err)
	}
}

// Every reason has a counter; a row that is bad for two reasons is counted once, by the first.
func TestBadRows(t *testing.T) {
	p, path := checkPipe(t)
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
		"22.0.11.0/24,1,2,,4\n" + // ok: 3 answers
		"\n" // an empty line is nothing
	gz(t, path, body)
	var out FileCheck
	if err := p.check(context.Background(), mustOpen(t, path), &out); err != nil {
		t.Fatal(err)
	}
	if out.BadColumns != 3 || out.BadPrefix != 5 || out.NotPublic != 2 || out.NotSorted != 2 || out.BadRTT != 8 || out.FewSites != 2 {
		t.Errorf("%+v", out)
	}
	if out.Valid != 2000+1+1+1 {
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
	p, path := checkPipe(t)
	ctx := context.Background()
	gz(t, path, header+rowsFor(9))
	if _, err := p.checkFile(ctx, path, 10); !errors.Is(err, ErrPermanent) {
		t.Errorf("minimum: %v", err)
	}
	// Exactly 1% bad is accepted, more is not.
	gz(t, path, header+rowsFor(99)+"10.0.0.0/24,1,2,3,4\n")
	if out, err := p.checkFile(ctx, path, 10); err != nil || out.Bad() != 1 {
		t.Errorf("1%%: %+v %v", out, err)
	}
	gz(t, path, header+rowsFor(98)+"10.0.0.0/24,1,2,3,4\n10.0.0.1/24,1,2,3,4\n")
	out, err := p.checkFile(ctx, path, 10)
	if !errors.Is(err, ErrPermanent) || out.Valid != 98 || out.Bad() != 2 {
		t.Errorf("over 1%%: %+v %v", out, err)
	}
	// The default minimum is 100,000 valid rows.
	gz(t, path, header+rowsFor(50))
	if _, err := p.checkFile(ctx, path, 0); !errors.Is(err, ErrPermanent) {
		t.Errorf("default minimum: %v", err)
	}
}

func TestHeaderErrors(t *testing.T) {
	p, path := checkPipe(t)
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
		_, err := p.checkFile(context.Background(), path, 10)
		if !errors.Is(err, ErrPermanent) {
			t.Errorf("%s: %v", name, err)
		} else if strings.Contains(err.Error(), "DE/A") || strings.Contains(err.Error(), "GB/B") || strings.Contains(err.Error(), "net") {
			t.Errorf("%s: the error echoes the header: %v", name, err)
		}
	}
	gz(t, path, "prefix,DE/A,GB/B,US/C\n"+rowsForN(50, 3))
	if _, err := p.checkFile(context.Background(), path, 10); err != nil {
		t.Errorf("3 sites: %v", err)
	}
	h64 := strings.Replace(sites(64), "DE/City0", "DE/"+strings.Repeat("é", 32), 1)
	gz(t, path, h64+rowsForN(50, 64))
	if out, err := p.checkFile(context.Background(), path, 10); err != nil || out.Sites != 64 {
		t.Errorf("64 sites: %+v %v", out, err)
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
	p, path := checkPipe(t)
	os.WriteFile(path, []byte("prefix,DE/A,GB/B,US/C\n"), 0o640)
	if _, err := p.checkFile(context.Background(), path, 10); !errors.Is(err, ErrPermanent) {
		t.Errorf("not gzip: %v", err)
	}
	gz(t, path, header+rowsFor(5000))
	data, _ := os.ReadFile(path)
	os.WriteFile(path, data[:len(data)-30], 0o640)
	if _, err := p.checkFile(context.Background(), path, 10); !errors.Is(err, ErrPermanent) {
		t.Errorf("cut short: %v", err)
	}
}

func TestCheckCancelled(t *testing.T) {
	p, path := checkPipe(t)
	gz(t, path, header+rowsFor(70000))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.checkFile(ctx, path, 10); !errors.Is(err, context.Canceled) {
		t.Errorf("%v", err)
	}
}

func TestRegionSitesAreValid(t *testing.T) {
	if len(regions) != 27 {
		t.Errorf("%d regions", len(regions))
	}
	seen := map[string]bool{}
	for _, r := range regions {
		if !validSite([]byte(r.Site)) || seen[r.Site] || seen[r.Name] || !strings.HasPrefix(r.Name, strings.SplitN(r.Name, "-", 2)[0]) {
			t.Errorf("region %+v", r)
		}
		seen[r.Site], seen[r.Name] = true, true
	}
	if _, ok := parseHeader([]byte(allSitesHeader())); !ok {
		t.Error("the header of the merged file is not valid")
	}
}

func allSitesHeader() string {
	h := "prefix"
	for _, r := range regions {
		h += "," + r.Site
	}
	return h
}
