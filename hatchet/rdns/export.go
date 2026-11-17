package rdns

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
)

// Thin exports for other workflows (import-mlab, sync-arin-whois). They add no behaviour: they hand out the
// PTR resolver, the production rule decoder, the MMDB copy helper, the gap list and the walk of
// resolve-ips, so the code is not copied.

// PTR lookup statuses (the same texts ip_ptr_names uses).
const (
	StatusOK       = statusOK
	StatusNotFound = statusNotFound
	StatusError    = statusError

	// ResolveWorkers is how many lookups resolve-names runs at a time.
	ResolveWorkers = resolveWorkers
)

// ScrubIPs hides IPv4 addresses in text going to a log or an error.
func ScrubIPs(text string) string { return scrubIPs(text) }

// NameAnswer is the result of one PTR lookup: PTR is lower case with no trailing dot and
// set only for StatusOK; ErrKind says why an error happened (only counted).
type NameAnswer struct {
	PTR     string
	Status  string
	ErrKind string
}

// NameResolver is the resolver of resolve-names: the system resolver first, Cloudflare's
// DNS-over-HTTPS when it fails.
type NameResolver struct{ r *ptrResolver }

// NewNameResolver makes the production resolver; addr is PTR_RESOLVER (empty: the system
// resolver), checked by CheckPTRResolver.
func NewNameResolver(addr string) *NameResolver { return &NameResolver{r: newPTRResolver(addr)} }

// ResolveAll looks up every IP with at most workers lookups at a time. The answers are in the
// order of ips; doh counts the lookups that needed the fallback.
func (n *NameResolver) ResolveAll(ctx context.Context, ips []netip.Addr, workers int) (answers []NameAnswer, doh int) {
	raw, doh := resolveAll(ctx, n.r, ips, workers)
	answers = make([]NameAnswer, len(raw))
	for i, a := range raw {
		answers[i] = NameAnswer{PTR: a.PTR, Status: a.Status, ErrKind: a.ErrKind}
	}
	return answers, doh
}

// Decoder decodes a router name into a city exactly as locate does: the legacy rules of
// public.generated_rules first, then the candidate rules of public.rdns_city_rules (a
// domain's ranks in order).
type Decoder struct {
	rules     []rule
	cityRules cityRuleIndex
}

// LoadDecoder reads the rules with its own connection and no DDL; a missing table means no
// rules. Errors never quote the DSN or a value.
func LoadDecoder(ctx context.Context, dsn string) (*Decoder, error) {
	conn, err := connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	rules, _, err := loadRules(ctx, conn)
	if err != nil {
		return nil, dbError("read rules", err)
	}
	cityRules, _, err := loadCityRules(ctx, conn)
	if err != nil {
		return nil, dbError("read candidate rules", err)
	}
	return &Decoder{rules: rules, cityRules: indexCityRules(cityRules)}, nil
}

// Decode returns the country and city a name stands for; ok is false when no rule gives one.
func (d *Decoder) Decode(name string) (country, city string, ok bool) {
	if d == nil {
		return "", "", false
	}
	if r := firstMatch(d.rules, name); r != nil {
		return r.country, r.city, true
	}
	if c, _, ok := firstCityRuleMatchRule(d.cityRules, name); ok {
		return c.Country, c.City, true
	}
	return "", "", false
}

// Counts returns how many legacy rules and learned domains the decoder holds.
func (d *Decoder) Counts() (legacy, domains int) {
	if d == nil {
		return 0, 0
	}
	return len(d.rules), len(d.cityRules)
}

// MMDBCopy keeps a copy of our published ip2geo MMDB from the private R2 bucket in Dir,
// fetched the way locate and sample-names do: the dated key of today (UTC), else yesterday's,
// the ETag saved beside the copy, a download only when it differs, and the saved copy when R2
// cannot be used.
type MMDBCopy struct {
	Dir      string // the workflow's own directory; the copy is Dir/ip2geo.mmdb
	R2       string // the R2 setting (the secret key is in it: never logged)
	Progress func(string)

	// Tests only.
	Store ObjectStore
	Now   func() time.Time
}

// MMDBState is what Prepare found.
type MMDBState struct {
	Path       string // the MMDB to read; empty when Status is "skipped"
	Status     string // "used", "saved copy" or "skipped"
	Reason     string // a fixed text: why R2 was not used or the MMDB skipped; empty for "used"
	ETag       string
	Downloaded bool
}

// Usable reports whether Path can be read.
func (s MMDBState) Usable() bool { return s.Status != statusSkipped && s.Path != "" }

func (c *MMDBCopy) pipeline() *Pipeline {
	return &Pipeline{Progress: c.Progress, R2: c.R2, store: c.Store, now: c.Now, dirOverride: c.Dir}
}

// Prepare makes the MMDB ready to read. It never fails: problems are logged (as the task who,
// with no IP) and returned as fixed reasons.
func (c *MMDBCopy) Prepare(ctx context.Context, who string) MMDBState {
	p := c.pipeline()
	ref := p.statMMDBFor(ctx, who)
	if !ref.usable() {
		res := skipped(ref.Reason)
		return MMDBState{Status: res.Status, Reason: res.Reason, ETag: res.ETag}
	}
	path, res := p.useMMDB(ctx, ref, who)
	if res.Status == statusSkipped {
		return MMDBState{Status: res.Status, Reason: res.Reason, ETag: res.ETag}
	}
	return MMDBState{Path: path, Status: res.Status, Reason: res.Reason, ETag: res.ETag, Downloaded: res.Downloaded}
}

// Drop removes the saved copy and its ETag (a copy mmdbbuilder could not read), so the next run
// fetches it again.
func (c *MMDBCopy) Drop() {
	path := filepath.Join(c.Dir, mmdbFile)
	os.Remove(path)
	os.Remove(path + mmdbETagSuffix)
}

// Connect opens a Postgres connection with no DDL; its errors never echo the DSN.
func Connect(ctx context.Context, dsn string) (*pgx.Conn, error) { return connect(ctx, dsn) }

// DBError replaces a Postgres error by its code: the message of a failed statement can quote
// the values it was given.
func DBError(op string, err error) error { return dbError(op, err) }

// WalkSize is the number of /24 indexes of the walk of sample-names (2^24).
const WalkSize = walkSize

// WalkAt is the /24 index (address >> 8) at position i of the shuffled walk over all /24s.
func WalkAt(i uint32) uint32 { return walkAt(i) }

// GapList is the parsed output of mmdbbuilder gaps: the runs of /24s that our MMDB gives a
// country but no city.
type GapList struct{ g gapList }

// ParseGaps reads the lines of mmdbbuilder gaps; a line that is not one is skipped.
func ParseGaps(data []byte) GapList { return GapList{parseGaps(data)} }

// Contains returns the country of the gap run that holds the /24 index idx (address >> 8).
func (l GapList) Contains(idx uint32) (country string, ok bool) { return l.g.contains(idx) }

// Total is the number of /24s in the list.
func (l GapList) Total() int { return l.g.total }
