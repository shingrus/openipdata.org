package ping

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/ipdata"
	"openipdata.org/hatchet/publish"
	"openipdata.org/hatchet/schema"
)

const (
	minTargets    = 100_000
	maxTargets    = 6_000_000
	minPrefixBits = 8 // a broader announced prefix is skipped
	shuffleSeed   = 1
)

// excludeText is the opt-out list: networks that asked not to be pinged, one IPv4 prefix per line.
//
//go:embed exclude.txt
var excludeText string

// span is a range of /24 indexes (address >> 8), both ends included.
type span struct{ first, last uint32 }

// excludeSpans is the parsed opt-out list. A bad line stops the start.
var excludeSpans = mustParseExclude(excludeText)

func mustParseExclude(text string) []span {
	spans, err := parseExclude(text)
	if err != nil {
		panic("ping: exclude.txt: " + err.Error())
	}
	return spans
}

// parseExclude reads the list (# comments and blank lines are ignored) into sorted, merged spans.
func parseExclude(text string) ([]span, error) {
	var spans []span
	for n, line := range strings.Split(text, "\n") {
		line, _, _ = strings.Cut(line, "#")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pfx, err := netip.ParsePrefix(line)
		if err != nil || !pfx.Addr().Is4() {
			return nil, fmt.Errorf("line %d is not an IPv4 prefix", n+1)
		}
		first, last := blockRange(pfx)
		spans = append(spans, span{first, last})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].first < spans[j].first })
	var merged []span
	for _, s := range spans {
		if k := len(merged) - 1; k >= 0 && s.first <= merged[k].last+1 {
			if s.last > merged[k].last {
				merged[k].last = s.last
			}
			continue
		}
		merged = append(merged, s)
	}
	return merged, nil
}

// blockRange is the first and last /24 index a prefix covers (a prefix longer than /24 is its /24).
func blockRange(pfx netip.Prefix) (first, last uint32) {
	a := pfx.Masked().Addr().As4()
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	first = v >> 8
	if pfx.Bits() >= 24 {
		return first, first
	}
	return first, first + 1<<(24-pfx.Bits()) - 1
}

func inSpans(spans []span, idx uint32) bool {
	i := sort.Search(len(spans), func(i int) bool { return spans[i].last >= idx })
	return i < len(spans) && spans[i].first <= idx
}

// hostingSource calls visit with the announced IPv4 prefixes (as written) of every current
// hosting network. reset is called first whenever the read starts again from the beginning.
type hostingSource func(ctx context.Context, reset func(), visit func(prefixes []string)) error

// reducedHostingQuery reads only the IPv4 prefix list of the networks whose category is hosting.
// public.asn.json is TEXT; the CASEs keep the cast to jsonb away from a value that is not an
// object, as ipdata.PrefixesJSON does. Read only.
const reducedHostingQuery = `SELECT ARRAY(SELECT t.e FROM jsonb_array_elements_text(l.list) AS t(e) WHERE t.e IS NOT NULL)
	FROM public.asn a
	CROSS JOIN LATERAL (SELECT CASE WHEN a.json LIKE '{%' THEN a.json::jsonb END AS doc) d
	CROSS JOIN LATERAL (SELECT CASE WHEN jsonb_typeof(d.doc #> '{prefixes,ipv4}') = 'array' THEN d.doc #> '{prefixes,ipv4}' END AS list) l
	WHERE l.list IS NOT NULL AND d.doc #>> '{metadata,category}' = 'hosting' AND ` + ipdata.SeenAtFilter + ` ORDER BY a.asn`

// wholeHostingQuery reads every current ASN's json, for the case that Postgres cannot cast one of
// them to jsonb. Read only.
const wholeHostingQuery = `SELECT a.json FROM public.asn a WHERE ` + ipdata.SeenAtFilter + ` ORDER BY a.asn`

// pgHosting reads the hosting networks from public.asn.
func pgHosting(dsn string) hostingSource {
	return func(ctx context.Context, reset func(), visit func([]string)) error {
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return fmt.Errorf("connect to Postgres: %w", schema.HideDSN(err))
		}
		defer conn.Close(ctx)
		whole := false
		return ipdata.WithJSONFallback(reducedHostingQuery, wholeHostingQuery, func(query string) error {
			reset()
			whole = query == wholeHostingQuery
			rows, err := conn.Query(ctx, query)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				if whole {
					var raw *string
					if err := rows.Scan(&raw); err != nil {
						return err
					}
					if raw != nil {
						if list, ok := hostingList(*raw); ok {
							visit(list)
						}
					}
					continue
				}
				var list []string
				if err := rows.Scan(&list); err != nil {
					return err
				}
				visit(list)
			}
			return rows.Err()
		})
	}
}

// hostingList is the IPv4 prefix list of a whole json document of a hosting network (the Go twin
// of reducedHostingQuery, for a document Postgres cannot cast).
func hostingList(raw string) ([]string, bool) {
	var doc struct {
		Metadata struct {
			Category string `json:"category"`
		} `json:"metadata"`
		Prefixes struct {
			IPv4 []any `json:"ipv4"`
		} `json:"prefixes"`
	}
	if json.Unmarshal([]byte(raw), &doc) != nil || doc.Metadata.Category != "hosting" || doc.Prefixes.IPv4 == nil {
		return nil, false
	}
	var out []string
	for _, v := range doc.Prefixes.IPv4 {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out, true
}

// TargetsOutput counts the targets task (counts only).
type TargetsOutput struct {
	Skipped         string  `json:"skipped,omitempty"`
	OldDeleted      int     `json:"old_deleted"`
	HostingNetworks int     `json:"hosting_networks"`
	Prefixes        int     `json:"prefixes"`
	Blocks          int     `json:"blocks"` // public /24s before the opt-out list
	Excluded        int     `json:"excluded"`
	Targets         int     `json:"targets"`
	Upload          string  `json:"upload,omitempty"`
	Seconds         float64 `json:"duration_seconds"`
}

// blockSet is a set of /24 indexes (2^24 bits).
type blockSet []uint64

func newBlockSet() blockSet            { return make(blockSet, 1<<24/64) }
func (b blockSet) add(idx uint32)      { b[idx>>6] |= 1 << (idx & 63) }
func (b blockSet) has(idx uint32) bool { return b[idx>>6]&(1<<(idx&63)) != 0 }

// buildTargets applies the target rules to what the source reads: every /24 covered by an
// announced IPv4 prefix of a hosting network (a prefix longer than /24 gives its /24; a prefix
// broader than /8 or one that does not parse is skipped), public only (the .1 decides), minus the
// /24s that overlap the opt-out spans. It returns the target /24 indexes, sorted.
func buildTargets(ctx context.Context, src hostingSource, exclude []span) (out TargetsOutput, blocks []uint32, err error) {
	set := newBlockSet()
	var networks, prefixes int
	reset := func() { set, networks, prefixes = newBlockSet(), 0, 0 }
	visit := func(list []string) {
		networks++
		for _, text := range list {
			pfx, err := netip.ParsePrefix(strings.TrimSpace(text))
			if err != nil || !pfx.Addr().Is4() || pfx.Bits() < minPrefixBits {
				continue
			}
			prefixes++
			first, last := blockRange(pfx)
			for i := first; i <= last; i++ {
				set.add(i)
			}
		}
	}
	if err := src(ctx, reset, visit); err != nil {
		return out, nil, err
	}
	out.HostingNetworks, out.Prefixes = networks, prefixes
	for i := uint32(0); i < 1<<24; i++ {
		if !set.has(i) || !ipdata.PublicV4(netip.AddrFrom4([4]byte{byte(i >> 16), byte(i >> 8), byte(i), 1})) {
			continue
		}
		out.Blocks++
		if inSpans(exclude, i) {
			out.Excluded++
			continue
		}
		blocks = append(blocks, i)
	}
	out.Targets = len(blocks)
	return out, blocks, nil
}

// writeTargets writes the .1 of each /24, shuffled with a fixed seed, through a .tmp file.
func writeTargets(path string, blocks []uint32) error {
	order := append([]uint32(nil), blocks...)
	rand.New(rand.NewSource(shuffleSeed)).Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	for _, i := range order {
		fmt.Fprintf(w, "%d.%d.%d.1\n", byte(i>>16), byte(i>>8), byte(i))
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// Targets is the targets task: it empties the objects of earlier runs in R2, lists the /24s of
// the hosting networks, writes the shuffled target list on the VM and uploads it to R2, where the
// ping VMs read it through a presigned URL.
func (p *Pipeline) Targets(ctx context.Context) (TargetsOutput, error) {
	started := time.Now()
	var out TargetsOutput
	if !p.Enabled() {
		out.Skipped = "no GCP key"
		p.log("targets: skipped (no GCP key)")
		return out, nil
	}
	store, err := p.objects()
	if err != nil {
		return out, err
	}
	// A VM of a crashed earlier run still holds upload links: delete it before the objects, so it
	// cannot write an old result into this run.
	c, err := p.computeAPI()
	if err != nil {
		return out, err
	}
	if _, err := deleteLeftovers(ctx, c); err != nil {
		return out, err
	}
	// Results of an earlier month must not count: the ping task only sees this run's objects.
	if out.OldDeleted, err = store.DeletePrefix(ctx, RunPrefix); err != nil {
		return out, fmt.Errorf("delete the objects of an earlier run: %w", err)
	}
	src := p.hosting
	if src == nil {
		src = pgHosting(p.PGSQL)
	}
	counts, blocks, err := buildTargets(ctx, src, excludeSpans)
	counts.OldDeleted = out.OldDeleted
	out = counts
	if err != nil {
		return out, err
	}
	min, max := p.minTargets, p.maxTargets
	if min == 0 {
		min = minTargets
	}
	if max == 0 {
		max = maxTargets
	}
	p.log("targets: %d hosting networks, %d prefixes, %d /24s, %d excluded, %d targets",
		out.HostingNetworks, out.Prefixes, out.Blocks, out.Excluded, out.Targets)
	if out.Targets < min || out.Targets > max {
		return out, permanent("%d targets is outside %d to %d", out.Targets, min, max)
	}
	if err := writeTargets(p.targetsPath(), blocks); err != nil {
		return out, fmt.Errorf("write the target list: %w", err)
	}
	_, status, err := store.Put(ctx, TargetsKey, p.targetsPath(), publish.PutOptions{Force: true})
	if err != nil {
		return out, err
	}
	out.Upload = string(status)
	out.Seconds = time.Since(started).Round(time.Second).Seconds()
	return out, nil
}
