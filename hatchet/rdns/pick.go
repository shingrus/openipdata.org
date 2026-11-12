package rdns

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"

	"openipdata.org/hatchet/ipdata"
)

const (
	// maxNetworksPerRun caps the /24s traced by one run.
	maxNetworksPerRun = 1000
	batchFile         = "batch.jsonl"
)

// batchEntry is one line of the batch file pick-networks writes for trace: a /24
// and its newest visitor IP. The file is on the VM only (the IPs never go through
// Hatchet) and is rewritten by every run's pick-networks; one run of the workflow
// at a time holds the "rdns" limit, so it belongs to the run that reads it.
type batchEntry struct {
	Prefix  string `json:"prefix"`
	Visitor string `json:"visitor"`
}

// PickOutput counts the /24s that wait for a trace and those picked for this run.
type PickOutput struct {
	Eligible int `json:"eligible"` // visitor /24s with no trace in the last 60 days
	Picked   int `json:"picked"`
	Visitors int `json:"visitors"` // distinct visitor IPs in the picked /24s
}

// pickQuery lists the IPv4 /24s with a visitor seen in the last 30 days and no
// trace in the last 60, most distinct visitor IPs first, with the newest visitor IP of
// each and the number of /24s that qualify before the limit. $1 is the limit and
// $2 the non-public blocks to leave out (an address in one of them is not traced).
// There is no "already known" check: tracing is cheap, and the geofeed layer
// still wins over the rDNS one. Read only.
const pickQuery = `WITH nets AS (
		SELECT network(set_masklen(ip, 24)) AS prefix, ip, last_seen_at
		FROM public.visitor_ips
		WHERE family(ip) = 4
		  AND last_seen_at > now() - interval '30 days'
		  AND NOT (ip <<= ANY ($2::text[]::inet[]))
	)
	SELECT prefix::text, count(*), host((array_agg(ip ORDER BY last_seen_at DESC, ip))[1]), count(*) OVER ()
	FROM nets n
	WHERE NOT EXISTS (
		SELECT 1 FROM public.network_traces t
		WHERE t.prefix = n.prefix AND t.traced_at > now() - interval '60 days')
	GROUP BY prefix
	ORDER BY count(*) DESC, prefix
	LIMIT $1`

// PickNetworks chooses the /24s for this run's trace task and writes them to
// the batch file.
func (p *Pipeline) PickNetworks(ctx context.Context) (PickOutput, error) {
	var out PickOutput
	conn, err := connect(ctx, p.PGSQL)
	if err != nil {
		return out, err
	}
	defer conn.Close(ctx)

	blocks := make([]string, len(ipdata.NonPublicV4))
	for i, b := range ipdata.NonPublicV4 {
		blocks[i] = b.String()
	}
	rows, err := conn.Query(ctx, pickQuery, maxNetworksPerRun, blocks)
	if err != nil {
		return out, dbError("pick networks", err)
	}
	defer rows.Close()
	var batch []batchEntry
	for rows.Next() {
		var e batchEntry
		var visitors, eligible int
		if err := rows.Scan(&e.Prefix, &visitors, &e.Visitor, &eligible); err != nil {
			return out, dbError("pick networks", err)
		}
		batch = append(batch, e)
		out.Visitors += visitors
		out.Eligible = eligible
	}
	if err := rows.Err(); err != nil {
		return out, dbError("pick networks", err)
	}
	out.Picked = len(batch)
	if err := os.MkdirAll(p.dir(), 0o750); err != nil {
		return out, err
	}
	if err := writeBatch(filepath.Join(p.dir(), batchFile), batch); err != nil {
		return out, err
	}
	p.log("pick-networks: %d /24s wait for a trace, %d picked (%d visitor IPs)", out.Eligible, out.Picked, out.Visitors)
	return out, nil
}

// writeBatch writes the batch file through a temporary file.
func writeBatch(path string, batch []batchEntry) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, e := range batch {
		if err := enc.Encode(e); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readBatch reads the batch file. A missing file is an empty batch. A line that
// is not a /24 with a public visitor IP inside it is dropped and counted; the
// error text never shows the line.
func readBatch(path string) (entries []batchTarget, dropped int, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var e batchEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			dropped++
			continue
		}
		prefix, perr := netip.ParsePrefix(e.Prefix)
		visitor, verr := netip.ParseAddr(e.Visitor)
		if perr != nil || verr != nil || prefix.Bits() != 24 || !prefix.Addr().Is4() || prefix != prefix.Masked() ||
			!prefix.Contains(visitor) || !ipdata.PublicV4(prefix.Addr()) || !ipdata.PublicV4(visitor) {
			dropped++
			continue
		}
		entries = append(entries, batchTarget{Prefix: prefix, Visitor: visitor})
	}
	if err := sc.Err(); err != nil {
		return nil, 0, fmt.Errorf("read batch file: %w", err)
	}
	return entries, dropped, nil
}

// batchTarget is a parsed batch file line.
type batchTarget struct {
	Prefix  netip.Prefix
	Visitor netip.Addr
}
