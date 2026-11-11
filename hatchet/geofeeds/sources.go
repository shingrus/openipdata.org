package geofeeds

import (
	"bufio"
	"context"
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// dump is one bulk whois or IRR file. Source is the label written to the
// outputs: the registry for whois objects, "irr" for route objects, which
// show who announces a prefix, not who holds it.
type dump struct {
	Name   string // cache file name: <name>.gz
	Source string
	URL    string
}

var defaultDumps = []dump{
	{"ripe-inetnum", "ripe", "https://ftp.ripe.net/ripe/dbase/split/ripe.db.inetnum.gz"},
	{"ripe-inet6num", "ripe", "https://ftp.ripe.net/ripe/dbase/split/ripe.db.inet6num.gz"},
	{"apnic-inetnum", "apnic", "https://ftp.apnic.net/apnic/whois/apnic.db.inetnum.gz"},
	{"apnic-inet6num", "apnic", "https://ftp.apnic.net/apnic/whois/apnic.db.inet6num.gz"},
	{"afrinic", "afrinic", "https://ftp.afrinic.net/dbase/afrinic.db.gz"},
	{"lacnic", "lacnic", "https://ftp.lacnic.net/lacnic/dbase/lacnic.db.gz"},
	{"ripe-route", "irr", "https://ftp.ripe.net/ripe/dbase/split/ripe.db.route.gz"},
	{"ripe-route6", "irr", "https://ftp.ripe.net/ripe/dbase/split/ripe.db.route6.gz"},
	{"apnic-route", "irr", "https://ftp.apnic.net/apnic/whois/apnic.db.route.gz"},
	{"apnic-route6", "irr", "https://ftp.apnic.net/apnic/whois/apnic.db.route6.gz"},
	{"arin-irr", "irr", "https://ftp.arin.net/pub/rr/arin.db.gz"},
	{"lacnic-irr", "irr", "https://ftp.lacnic.net/lacnic/irr/lacnic.db.gz"},
	// RADB serves its own IRR and mirrors of others, over FTP only. Level3's
	// own server did not answer, and RADB carries no copy of it.
	{"radb", "irr", "ftp://ftp.radb.net/radb/dbase/radb.db.gz"},
	{"altdb", "irr", "ftp://ftp.radb.net/radb/dbase/altdb.db.gz"},
	{"nttcom", "irr", "ftp://ftp.radb.net/radb/dbase/nttcom.db.gz"},
	{"jpirr", "irr", "ftp://ftp.radb.net/radb/dbase/jpirr.db.gz"},
	{"tc", "irr", "ftp://ftp.radb.net/radb/dbase/tc.db.gz"},
	{"reach", "irr", "ftp://ftp.radb.net/radb/dbase/reach.db.gz"},
	{"bell", "irr", "ftp://ftp.radb.net/radb/dbase/bell.db.gz"},
	{"bboi", "irr", "ftp://ftp.radb.net/radb/dbase/bboi.db.gz"},
	{"canarie", "irr", "ftp://ftp.radb.net/radb/dbase/canarie.db.gz"},
	{"panix", "irr", "ftp://ftp.radb.net/radb/dbase/panix.db.gz"},
	{"nestegg", "irr", "ftp://ftp.radb.net/radb/dbase/nestegg.db.gz"},
}

// listedFeeds is the hand-kept list of operator feeds that whois does not
// point to (source "list").
//
//go:embed feeds.txt
var listedFeeds string

// parseFeedList splits feeds.txt into feed URLs and the pages that link to feeds.
func parseFeedList(text string) (feeds, pages []string) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		page := strings.HasPrefix(line, "page ")
		if u, ok := cleanURL(strings.TrimPrefix(line, "page ")); ok {
			if page {
				pages = append(pages, u)
			} else {
				feeds = append(feeds, u)
			}
		}
	}
	return feeds, pages
}

// maxPage caps how much of a page is read.
const maxPage = 5 << 20

// csvLink matches an https link to a .csv file.
var csvLink = regexp.MustCompile(`(?i)https://[^\s"'<>\\]+\.csv`)

// pageFeeds returns the distinct .csv links in a page. Links inside JSON
// have escaped slashes (https:\/\/host\/file.csv).
func pageFeeds(ctx context.Context, client *http.Client, page string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPage))
	if err != nil {
		return nil, err
	}
	var urls []string
	seen := map[string]bool{}
	for _, link := range csvLink.FindAllString(strings.ReplaceAll(string(body), `\/`, "/"), -1) {
		if u, ok := cleanURL(link); ok && !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	return urls, nil
}

// CAIDA publishes the geofeed URLs it finds in whois dumps, including ARIN's
// and the NIRs', which have no public dumps. Use is governed by the CAIDA AUA
// for public datasets: publications must cite "The CAIDA UCSD Geofeed WHOIS
// dataset" (https://catalog.caida.org/dataset/geofeed_whois).
const (
	caidaBase = "https://publicdata.caida.org/datasets/geofeed-whois"
	caidaDays = 7 // a day's lists are sometimes partial, so take the union of a week
)

var caidaFiles = []string{"standard_geofeeds.txt", "non_standard_geofeeds.txt"}

// caidaURLs reads the lists of the last caidaDays days. Lines look like
// <registry>_<attribute>_<index>_<url>. It fails only if no list was read.
func caidaURLs(ctx context.Context, client *http.Client, base string, now time.Time) (urls []string, files int, err error) {
	seen := map[string]bool{}
	var lastErr error
	for day := 0; day < caidaDays; day++ {
		date := now.UTC().AddDate(0, 0, -day)
		for _, name := range caidaFiles {
			endpoint := fmt.Sprintf("%s/%s/%s", base, date.Format("2006/01/02"), name)
			found, err := fetchList(ctx, client, endpoint)
			if err != nil {
				lastErr = err
				continue
			}
			files++
			for _, line := range found {
				parts := strings.SplitN(line, "_", 4)
				if len(parts) != 4 {
					continue
				}
				if u, ok := cleanURL(parts[3]); ok && !seen[u] {
					seen[u] = true
					urls = append(urls, u)
				}
			}
		}
	}
	if files == 0 {
		return nil, 0, fmt.Errorf("no CAIDA list in the last %d days: %v", caidaDays, lastErr)
	}
	return urls, files, nil
}

// fetchList returns the non-empty lines of a small text file.
func fetchList(ctx context.Context, client *http.Client, endpoint string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", endpoint, resp.StatusCode)
	}
	var lines []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, sc.Err()
}
