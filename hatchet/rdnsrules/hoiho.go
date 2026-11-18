package rdnsrules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"openipdata.org/hatchet/download"
	"openipdata.org/hatchet/schema"
)

// HoihoOutput is the report of import-hoiho: counts and fixed texts only.
type HoihoOutput struct {
	Records           int            `json:"records"`
	RecordsSkipped    int            `json:"records_skipped"`
	Regexes           int            `json:"regexes"`
	RegexesSkipped    int            `json:"regexes_skipped"`
	SkipReasons       map[string]int `json:"skip_reasons,omitempty"`
	Rows              int            `json:"rows"`
	Keys              int            `json:"keys"`
	Hints             int            `json:"hints"`
	HintsPlacedByName int            `json:"hints_placed_by_name"`
	HintsPlacedNearby int            `json:"hints_placed_nearby"`
	HintsUnplaced     int            `json:"hints_unplaced"`
	CLLIHints         int            `json:"clli_hints"`
	CLLIGuessed       int            `json:"clli_guessed"`
	CLLIKnownNYC      int            `json:"clli_known_nyc"`
	CLLINoRegion      int            `json:"clli_no_region"`
	CLLINoCandidate   int            `json:"clli_no_candidate"`
	DomainsRejected   int            `json:"domains_rejected"` // domains whose rows stay rejected
	Written           bool           `json:"written"`
	Unchanged         bool           `json:"unchanged"`
	Skipped           string         `json:"skipped,omitempty"`
	DurationSeconds   float64        `json:"duration_seconds"`
	// Rejection and Acceptance are set by a run with the workflow inputs reject_hoiho_domains or
	// accept_hoiho_domains, which switches suffixes off or on and imports nothing.
	Rejection  *HoihoRejectOutput `json:"rejection,omitempty"`
	Acceptance *HoihoRejectOutput `json:"acceptance,omitempty"`
}

// markChecked writes the "<file>.checked" stamp that fetchSource reads (through a temporary file).
func markChecked(path string, now time.Time) error {
	tmp := path + ".checked.tmp"
	if err := os.WriteFile(tmp, []byte(now.UTC().Format(time.RFC3339Nano)), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path+".checked")
}

// ImportHoiho imports the router-name rules of CAIDA's Hoiho into public.rdns_hoiho_rules. A file
// that cannot be fetched or is unusable, or trouble with GeoNames, only skips the import (the
// table stays as it is); only a database error fails the task.
func (p *Pipeline) ImportHoiho(ctx context.Context) (HoihoOutput, error) {
	started := time.Now()
	out := HoihoOutput{}
	if err := p.CheckPGSQL(); err != nil {
		return out, err
	}
	finish := func(skipped string) (HoihoOutput, error) {
		out.Skipped = skipped
		out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
		p.log("import-hoiho: skipped: %s", skipped)
		return out, nil
	}
	dir := filepath.Join(p.DataDir, "rdnsrules", "dictionaries")
	path := filepath.Join(dir, hoihoCacheName)
	data, fresh := p.testHoiho, false
	if data == nil {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return finish("dictionary cache unavailable")
		}
		var err error
		if data, fresh, err = fetchSource(ctx, dir, hoihoCacheName, hoihoURL, download.Capped(p.client, hoihoMaxFile), p.clock().UTC()); err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			return finish("file unavailable")
		}
	}
	// The cached file, its ETag and its stamp are removed only when the FILE is the problem (too
	// large, too few records, too many keys), so the next run fetches again. fresh is also true after
	// a 304, so a good cached file is never removed for a problem that is not its own.
	dropFile := func() {
		if fresh {
			_ = os.Remove(path)
			_ = os.Remove(path + ".etag")
			_ = os.Remove(path + ".checked")
		}
	}
	if len(data) > hoihoMaxFile {
		dropFile()
		return finish("file too large")
	}
	recs := parseHoiho(data, &out)
	if len(recs) < hoihoMinRecords {
		dropFile()
		return finish("too few records")
	}
	var geo *geoIndex
	if p.testGeo != nil {
		geo = p.testGeo
	} else {
		var err error
		if geo, err = loadGeoIndex(ctx, dir, p.client, p.clock().UTC()); err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			return finish("GeoNames unavailable")
		}
	}
	rows, tooMany := convertHoiho(recs, geo, &out)
	if tooMany {
		dropFile()
		return finish("too many keys")
	}
	if len(rows) < hoihoMinRows {
		return finish("too few rows") // the file may be fine (a bad GeoNames index): it stays, with no new stamp
	}
	if fresh {
		if err := markChecked(path, p.clock()); err != nil {
			return finish("dictionary cache unavailable")
		}
	}
	conn, err := pgx.Connect(ctx, p.PGSQL)
	if err != nil {
		return out, fmt.Errorf("connect to Postgres: %w", schema.HideDSN(err))
	}
	defer conn.Close(ctx)
	if err = schema.EnsureRDNSHoihoRules(ctx, conn); err != nil {
		return out, safeDBError("ensure hoiho rules schema", err)
	}
	if err = storeHoiho(ctx, conn, rows, &out); err != nil {
		return out, err
	}
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	p.log("import-hoiho: %d records (%d skipped), %d regexes (%d skipped), %d rows, %d keys, %d hints (%d by name, %d nearby, %d unplaced), %d CLLI hints (%d guessed), written=%t unchanged=%t, %d domains rejected",
		out.Records, out.RecordsSkipped, out.Regexes, out.RegexesSkipped, out.Rows, out.Keys, out.Hints, out.HintsPlacedByName, out.HintsPlacedNearby, out.HintsUnplaced,
		out.CLLIHints, out.CLLIGuessed, out.Written, out.Unchanged, out.DomainsRejected)
	return out, nil
}

// lockHoihoRules is the first statement of every transaction that writes public.rdns_hoiho_rules:
// plain SELECTs of the readers are not blocked, imports and the reject commands are serialised.
const lockHoihoRules = `LOCK TABLE public.rdns_hoiho_rules IN SHARE ROW EXCLUSIVE MODE`

// hoihoStatus is the one status rule: rejected when the domain equals a rejected domain or ends
// with "." and a rejected domain, else candidate. (SetHoihoRejected applies the same rule in SQL.)
func hoihoStatus(domain string, rejected map[string]bool) string {
	for d := domain; ; {
		if rejected[d] {
			return "rejected"
		}
		i := strings.IndexByte(d, '.')
		if i < 0 {
			return "candidate"
		}
		d = d[i+1:]
	}
}

// storeHoiho replaces the table in one transaction. It reads the rejected domains and gives every
// new row its status by hoihoStatus. The same set of (domain, seq, version, status) leaves the table
// alone; anything else deletes every row and inserts the new ones.
func storeHoiho(ctx context.Context, conn *pgx.Conn, rows []HoihoRow, out *HoihoOutput) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return safeDBError("begin hoiho import", err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, lockHoihoRules); err != nil {
		return safeDBError("lock hoiho rules", err)
	}
	rejected := map[string]bool{}
	cur, err := tx.Query(ctx, `SELECT domain FROM public.rdns_hoiho_rejected_domains`)
	if err != nil {
		return safeDBError("read rejected hoiho domains", err)
	}
	for cur.Next() {
		var d string
		if err = cur.Scan(&d); err != nil {
			cur.Close()
			return safeDBError("read rejected hoiho domains", err)
		}
		rejected[d] = true
	}
	cur.Close()
	if err = cur.Err(); err != nil {
		return safeDBError("read rejected hoiho domains", err)
	}
	type id struct {
		domain string
		seq    int16
	}
	type state struct{ version, status string }
	old := map[id]state{}
	cur, err = tx.Query(ctx, `SELECT domain, seq, version, status FROM public.rdns_hoiho_rules`)
	if err != nil {
		return safeDBError("read hoiho rules", err)
	}
	for cur.Next() {
		var domain, version, status string
		var seq int16
		if err = cur.Scan(&domain, &seq, &version, &status); err != nil {
			cur.Close()
			return safeDBError("read hoiho rules", err)
		}
		old[id{domain, seq}] = state{version, status}
	}
	cur.Close()
	if err = cur.Err(); err != nil {
		return safeDBError("read hoiho rules", err)
	}
	statuses := make([]string, len(rows))
	same := len(old) == len(rows)
	distinct := map[string]bool{}
	for i, r := range rows {
		statuses[i] = hoihoStatus(r.Domain, rejected)
		if statuses[i] == "rejected" {
			distinct[r.Domain] = true
		}
		same = same && old[id{r.Domain, int16(r.Seq)}] == state{r.Version, statuses[i]}
	}
	out.DomainsRejected = len(distinct)
	if same {
		out.Unchanged = true
		return nil
	}
	if _, err = tx.Exec(ctx, `DELETE FROM public.rdns_hoiho_rules`); err != nil {
		return safeDBError("clear hoiho rules", err)
	}
	batch := &pgx.Batch{}
	for i, r := range rows {
		places, merr := json.Marshal(r.Places)
		if merr != nil {
			return errors.New("encode hoiho rules failed")
		}
		batch.Queue(`INSERT INTO public.rdns_hoiho_rules (domain, seq, pattern, plan, places, class, version, status)
			VALUES ($1, $2, $3, $4, $5::text::jsonb, $6, $7, $8)`, r.Domain, int16(r.Seq), r.Pattern, r.Plan, string(places), r.Class, r.Version, statuses[i])
	}
	res := tx.SendBatch(ctx, batch)
	for range rows {
		if _, err = res.Exec(); err != nil {
			res.Close()
			return safeDBError("insert hoiho rules", err)
		}
	}
	if err = res.Close(); err != nil {
		return safeDBError("insert hoiho rules", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return safeDBError("commit hoiho import", err)
	}
	out.Written = true
	return nil
}

// HoihoRejectOutput is the report of SetHoihoRejected: counts only.
type HoihoRejectOutput struct {
	Domains         int     `json:"domains"`          // given (distinct, after cleaning)
	DomainsChanged  int     `json:"domains_changed"`  // added to or removed from the rejected list
	RowsChanged     int     `json:"rows_changed"`     // rule rows whose status changed
	Rejected        int     `json:"rejected_domains"` // domains on the rejected list now
	DurationSeconds float64 `json:"duration_seconds"`
}

// ErrHoihoDomain is the fixed error for a domain that is not a plain host name (it never echoes it).
var ErrHoihoDomain = errors.New("invalid domain: want a plain host name such as example.net")

// SwitchHoiho is what the workflow inputs reject_hoiho_domains and accept_hoiho_domains do: it runs
// SetHoihoRejected for the reject list and then for the accept list (a domain in both ends up
// accepted). A list with no domain is left out and its report is nil. Both lists are checked before
// anything is changed, so a bad domain in one never leaves the other applied.
func (p *Pipeline) SwitchHoiho(ctx context.Context, reject, accept []string) (rejected, accepted *HoihoRejectOutput, err error) {
	for _, list := range [][]string{reject, accept} {
		if len(list) > 0 {
			if _, err := cleanHoihoDomains(list); err != nil {
				return nil, nil, err
			}
		}
	}
	if len(reject) > 0 {
		out, err := p.SetHoihoRejected(ctx, reject, true)
		if err != nil {
			return nil, nil, err
		}
		rejected = &out
	}
	if len(accept) > 0 {
		out, err := p.SetHoihoRejected(ctx, accept, false)
		if err != nil {
			return rejected, nil, err
		}
		accepted = &out
	}
	return rejected, accepted, nil
}

// cleanHoihoDomains lower-cases the domains, trims dots and spaces, drops repeats and sorts them.
// Every one must be a plain host name (an empty item among several is a trailing comma and is
// skipped); a bad one, or none at all, is ErrHoihoDomain.
func cleanHoihoDomains(domains []string) ([]string, error) {
	var clean []string
	seen := map[string]bool{}
	for _, d := range domains {
		d = strings.Trim(strings.ToLower(strings.TrimSpace(d)), ".")
		if d == "" && len(domains) > 1 {
			continue // a trailing comma
		}
		if !plainHostName(d) {
			return nil, ErrHoihoDomain
		}
		if !seen[d] {
			seen[d] = true
			clean = append(clean, d)
		}
	}
	if len(clean) == 0 {
		return nil, ErrHoihoDomain
	}
	sort.Strings(clean)
	return clean, nil
}

// SetHoihoRejected adds the domains to (rejected = true) or removes them from the list of suffixes
// whose Hoiho rules are switched off (public.rdns_hoiho_rejected_domains), then brings the status of
// every row of public.rdns_hoiho_rules in line with the rule of hoihoStatus. Any suffix may be
// given, not only a registered domain. Nothing else is touched.
func (p *Pipeline) SetHoihoRejected(ctx context.Context, domains []string, rejected bool) (HoihoRejectOutput, error) {
	started := time.Now()
	out := HoihoRejectOutput{}
	if err := p.CheckPGSQL(); err != nil {
		return out, err
	}
	clean, err := cleanHoihoDomains(domains)
	if err != nil {
		return out, err
	}
	out.Domains = len(clean)
	conn, err := pgx.Connect(ctx, p.PGSQL)
	if err != nil {
		return out, fmt.Errorf("connect to Postgres: %w", schema.HideDSN(err))
	}
	defer conn.Close(ctx)
	if err = schema.EnsureRDNSHoihoRules(ctx, conn); err != nil {
		return out, safeDBError("ensure hoiho rules schema", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return out, safeDBError("begin hoiho rejection", err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, lockHoihoRules); err != nil {
		return out, safeDBError("lock hoiho rules", err)
	}
	for _, d := range clean {
		var tag pgconn.CommandTag
		if rejected {
			tag, err = tx.Exec(ctx, `INSERT INTO public.rdns_hoiho_rejected_domains (domain) VALUES ($1) ON CONFLICT DO NOTHING`, d)
		} else {
			tag, err = tx.Exec(ctx, `DELETE FROM public.rdns_hoiho_rejected_domains WHERE domain = $1`, d)
		}
		if err != nil {
			return out, safeDBError("update rejected hoiho domains", err)
		}
		out.DomainsChanged += int(tag.RowsAffected())
	}
	tag, err := tx.Exec(ctx, `UPDATE public.rdns_hoiho_rules r SET status = s.want
		FROM (SELECT x.domain, x.seq, CASE WHEN EXISTS (
				SELECT 1 FROM public.rdns_hoiho_rejected_domains d
				WHERE x.domain = d.domain OR right(x.domain, length(d.domain) + 1) = '.' || d.domain
			) THEN 'rejected' ELSE 'candidate' END AS want
			FROM public.rdns_hoiho_rules x) s
		WHERE r.domain = s.domain AND r.seq = s.seq AND r.status <> s.want`)
	if err != nil {
		return out, safeDBError("update hoiho rule status", err)
	}
	out.RowsChanged = int(tag.RowsAffected())
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM public.rdns_hoiho_rejected_domains`).Scan(&out.Rejected); err != nil {
		return out, safeDBError("count rejected hoiho domains", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return out, safeDBError("commit hoiho rejection", err)
	}
	out.DurationSeconds = time.Since(started).Round(time.Second).Seconds()
	word := "rejected"
	if !rejected {
		word = "accepted"
	}
	p.log("hoiho rules %s: %d domains given, %d added or removed, %d rule rows changed, %d rejected domains in all", word, out.Domains, out.DomainsChanged, out.RowsChanged, out.Rejected)
	return out, nil
}
