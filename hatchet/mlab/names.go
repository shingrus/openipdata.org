package mlab

import (
	"context"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/rdns"
	"openipdata.org/hatchet/schema"
)

const (
	// freshFor is how long an ok or not_found answer is trusted; errors are asked again every run.
	freshFor = "60 days"
	// pruneAfter: a row whose IP is not in the current pairs file is deleted when older than this.
	pruneAfter = "200 days"

	// resolveBudget is how long the lookups may take (the task's own timeout is a little above).
	// A run that reaches it keeps what it stored and stops: locate works with what is there.
	resolveBudget = 3 * time.Hour
	resolveBatch  = 2000 // lookups done and stored together
)

// NamesOutput counts resolve-names (counts only: no IP, no name).
type NamesOutput struct {
	Hops     int    `json:"hops"`      // distinct hop IPs of the pairs file
	Fresh    int    `json:"fresh"`     // already answered (ok or not found) in the last 60 days
	Queried  int    `json:"queried"`   // looked up now
	OK       int    `json:"ok"`        // got a name
	NotFound int    `json:"not_found"` // no PTR record
	Errors   int    `json:"errors"`    // failed; asked again next run
	DoH      int    `json:"doh"`       // of the lookups, answered through the DoH fallback
	Left     int    `json:"left"`      // not looked up because the budget ran out
	Pruned   int    `json:"pruned"`    // old rows of IPs no longer in the pairs file, deleted
	Stopped  string `json:"stopped,omitempty"`
}

// nameStore is the table public.mlab_hop_names, as resolveNames uses it. Tests replace it.
type nameStore interface {
	// Fresh returns the IPs with an ok or not_found answer younger than freshFor.
	Fresh(ctx context.Context) (map[uint32]struct{}, error)
	// Save stores the answers (an error never replaces an older ok or not_found row).
	Save(ctx context.Context, ips []netip.Addr, answers []rdns.NameAnswer) error
	// Old returns the IPs of rows older than pruneAfter.
	Old(ctx context.Context) ([]uint32, error)
	Delete(ctx context.Context, ips []uint32) (int, error)
}

// resolveFunc looks the IPs up; answers are in the order of ips. Tests replace it.
type resolveFunc func(ctx context.Context, ips []netip.Addr, workers int) ([]rdns.NameAnswer, int)

// ResolveNames looks up the PTR names of the hop IPs of the pairs file that have no fresh answer
// and stores them 2000 at a time, so a retry or the next run goes on where this one stopped. A
// soft budget ends the lookups cleanly (stopped: time); a DNS problem never fails the task, only a
// database error or a cancellation does.
func (p *Pipeline) ResolveNames(ctx context.Context) (NamesOutput, error) {
	if err := p.CheckPGSQL(); err != nil {
		return NamesOutput{}, err
	}
	pairs, err := readPairs(p.pairsPath())
	if err != nil {
		return NamesOutput{}, err
	}
	hops := distinctHops(pairs)
	pairs = nil

	conn, err := rdns.Connect(ctx, p.PGSQL)
	if err != nil {
		return NamesOutput{}, err
	}
	defer conn.Close(ctx)
	// The one DDL of the workflow, once per run: CREATE TABLE IF NOT EXISTS, no ALTER.
	if err := schema.EnsureMLab(ctx, conn); err != nil {
		return NamesOutput{}, rdns.DBError("ensure schema", err)
	}
	resolve := p.resolve
	if resolve == nil {
		r := rdns.NewNameResolver(p.PTRResolver)
		resolve = r.ResolveAll
	}
	budget := p.budget
	if budget == 0 {
		budget = resolveBudget
	}
	batch := p.batch
	if batch == 0 {
		batch = resolveBatch
	}
	out, err := resolveNames(ctx, &pgNames{conn: conn}, resolve, hops, budget, batch)
	if err != nil {
		return out, err
	}
	p.log("resolve-names: %d hops, %d fresh; %d looked up: %d names, %d not found, %d errors, %d through DoH; %d left (%s); %d old removed",
		out.Hops, out.Fresh, out.Queried, out.OK, out.NotFound, out.Errors, out.DoH, out.Left, out.Stopped, out.Pruned)
	return out, nil
}

// resolveNames is ResolveNames without the database connection and files.
func resolveNames(ctx context.Context, st nameStore, resolve resolveFunc, hops []uint32, budget time.Duration, batch int) (NamesOutput, error) {
	out := NamesOutput{Hops: len(hops)}
	fresh, err := st.Fresh(ctx)
	if err != nil {
		return out, err
	}
	var todo []uint32
	for _, h := range hops {
		if _, ok := fresh[h]; ok {
			out.Fresh++
		} else {
			todo = append(todo, h)
		}
	}
	rctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	done := 0
	for done < len(todo) {
		if rctx.Err() != nil {
			break
		}
		chunk := todo[done:min(done+batch, len(todo))]
		ips := make([]netip.Addr, len(chunk))
		for i, h := range chunk {
			ips[i] = addrOf(h)
		}
		answers, doh := resolve(rctx, ips, rdns.ResolveWorkers)
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if rctx.Err() != nil {
			break // answers cut short by the budget are not real errors: drop the chunk
		}
		if err := st.Save(ctx, ips, answers); err != nil {
			return out, err
		}
		out.Queried += len(chunk)
		out.DoH += doh
		for _, a := range answers {
			switch a.Status {
			case rdns.StatusOK:
				out.OK++
			case rdns.StatusNotFound:
				out.NotFound++
			default:
				out.Errors++
			}
		}
		done += len(chunk)
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if out.Left = len(todo) - done; out.Left > 0 {
		out.Stopped = "time"
	}

	// Old rows of IPs the current pairs file does not have.
	old, err := st.Old(ctx)
	if err != nil {
		return out, err
	}
	var gone []uint32
	for _, ip := range old {
		if !hasHop(hops, ip) {
			gone = append(gone, ip)
		}
	}
	for start := 0; start < len(gone); start += 5000 {
		n, err := st.Delete(ctx, gone[start:min(start+5000, len(gone))])
		if err != nil {
			return out, err
		}
		out.Pruned += n
	}
	return out, nil
}

// hasHop reports whether the sorted slice holds ip.
func hasHop(hops []uint32, ip uint32) bool {
	lo, hi := 0, len(hops)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if hops[m] < ip {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo < len(hops) && hops[lo] == ip
}

// pgNames is the Postgres nameStore.
type pgNames struct{ conn *pgx.Conn }

func parseHost(s string) (uint32, bool) {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Unmap().Is4() {
		return 0, false
	}
	return u32Of(a.Unmap()), true
}

func (s *pgNames) ips(ctx context.Context, op, sql string, args ...any) ([]uint32, error) {
	rows, err := s.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, rdns.DBError(op, err)
	}
	defer rows.Close()
	var out []uint32
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return nil, rdns.DBError(op, err)
		}
		if v, ok := parseHost(ip); ok {
			out = append(out, v)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, rdns.DBError(op, err)
	}
	return out, nil
}

func (s *pgNames) Fresh(ctx context.Context) (map[uint32]struct{}, error) {
	list, err := s.ips(ctx, "read hop names",
		`SELECT host(ip) FROM public.mlab_hop_names WHERE status IN ('ok', 'not_found') AND resolved_at > now() - $1::text::interval`, freshFor)
	if err != nil {
		return nil, err
	}
	set := make(map[uint32]struct{}, len(list))
	for _, v := range list {
		set[v] = struct{}{}
	}
	return set, nil
}

// saveSQL stores answers: a new answer replaces the row, except that an error never replaces a
// stored ok or not_found row (that row simply stays old, so the next run asks again).
const saveSQL = `INSERT INTO public.mlab_hop_names (ip, ptr, status, resolved_at)
	SELECT u.ip::inet, u.ptr, u.status, now() FROM unnest($1::text[], $2::text[], $3::text[]) AS u(ip, ptr, status)
	ON CONFLICT (ip) DO UPDATE SET ptr = excluded.ptr, status = excluded.status, resolved_at = excluded.resolved_at
	WHERE excluded.status <> 'error' OR public.mlab_hop_names.status = 'error'`

func (s *pgNames) Save(ctx context.Context, ips []netip.Addr, answers []rdns.NameAnswer) error {
	ip, ptr, status := make([]string, len(ips)), make([]string, len(ips)), make([]string, len(ips))
	for i, a := range ips {
		ip[i], status[i] = a.String(), answers[i].Status
		if status[i] != rdns.StatusOK && status[i] != rdns.StatusNotFound {
			status[i] = rdns.StatusError
		}
		if status[i] == rdns.StatusOK {
			ptr[i] = answers[i].PTR
		}
	}
	if _, err := s.conn.Exec(ctx, saveSQL, ip, ptr, status); err != nil {
		return rdns.DBError("store hop names", err)
	}
	return nil
}

func (s *pgNames) Old(ctx context.Context) ([]uint32, error) {
	return s.ips(ctx, "read old hop names", `SELECT host(ip) FROM public.mlab_hop_names WHERE resolved_at < now() - $1::text::interval`, pruneAfter)
}

func (s *pgNames) Delete(ctx context.Context, ips []uint32) (int, error) {
	list := make([]string, len(ips))
	for i, v := range ips {
		list[i] = addrOf(v).String()
	}
	tag, err := s.conn.Exec(ctx, `DELETE FROM public.mlab_hop_names WHERE ip = ANY($1::text[]::inet[]) AND resolved_at < now() - $2::text::interval`, list, pruneAfter)
	if err != nil {
		return 0, rdns.DBError("prune hop names", err)
	}
	return int(tag.RowsAffected()), nil
}
