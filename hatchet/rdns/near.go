package rdns

import (
	"context"
	"encoding/binary"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/ipdata"
)

// The near lane of sample-names: only 2.7% of sampled /24s end up with a decoded city (a row of scope
// named), but those rows cluster. /16 blocks that already hold a named row caught 79% of the later
// rows and gave 15 times more rows per lookup (measured 2026-11-27). So each run first samples the
// /16 blocks around the named rows, within the same total of sampleNetworks /24s a run: the named
// /24s themselves again (a /24 that got a city leaves the gap list, so the walk never sees it
// again and its customer_name_networks row would be pruned at 200 days), then the gap /24s of those
// blocks, both in walk order.
//
// The done file (sample-done.bits in the rdns dir) has one bit per /24 index: sampled or passed by the
// walk in the current cycle. The lane and the walk skip those, so nothing is looked up twice in
// a cycle. Losing the file costs only some repeated lookups.
const (
	nearNetworks = 1500 // at most this many of the /24s of a run come from the lane
	nearBits     = 16   // a block is a /16
	doneFile     = "sample-done.bits"

	walkMulInv = 0xb382c9 // the inverse of walkMul modulo 2^24 (walkPos(walkAt(i)) == i, tested)

	doneHeader = 8
	doneBytes  = walkSize / 8
)

// walkPos is the position of the /24 index idx in the walk: the inverse of walkAt.
func walkPos(idx uint32) uint32 { return ((idx - walkAdd) * walkMulInv) & (walkSize - 1) }

// doneSet has one bit per /24 index.
type doneSet struct{ bits []byte }

func newDoneSet() doneSet { return doneSet{bits: make([]byte, doneBytes)} }

func (d doneSet) has(idx uint32) bool { return d.bits[idx>>3]&(1<<(idx&7)) != 0 }
func (d doneSet) set(idx uint32)      { d.bits[idx>>3] |= 1 << (idx & 7) }

func (d doneSet) clone() doneSet { return doneSet{bits: slices.Clone(d.bits)} }

func (p *Pipeline) donePath() string { return filepath.Join(p.dir(), doneFile) }

// readDone reads the done file of the cycle in state. A missing file, one of another length or one of
// another cycle gives a new set with every /24 the walk already passed (the positions below the cursor)
// marked. It never fails: losing the file only repeats some lookups.
func readDone(path string, state sampleState) doneSet {
	data, err := os.ReadFile(path)
	if err == nil && len(data) == doneHeader+doneBytes &&
		int64(binary.BigEndian.Uint64(data)) == state.CycleStarted.UnixNano() {
		return doneSet{bits: data[doneHeader:]}
	}
	d := newDoneSet()
	for i := 0; i < min(state.Cursor, walkSize); i++ {
		d.set(walkAt(uint32(i)))
	}
	return d
}

// write saves the set for the cycle that began at cycleStarted, through a temporary file.
func (d doneSet) write(path string, cycleStarted time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	data := make([]byte, doneHeader+doneBytes)
	binary.BigEndian.PutUint64(data, uint64(cycleStarted.UnixNano()))
	copy(data[doneHeader:], d.bits)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// nearPick picks up to want /24s from the /16 blocks that hold a key of named (a /24 index to the
// country of its named row): first the named /24s themselves (a refresh), then the gap /24s of those
// blocks, each group in walk order, never one that is done or whose .1 is not public. candidates is the
// number before the cut and blocks the number of distinct blocks. The hosts are drawn with intn, only
// for the picked /24s and in their final order.
func nearPick(g gapList, named map[uint32]string, done doneSet, want int, intn func(int) int) (picked []sampled, candidates, blocks int) {
	const shift = 24 - nearBits
	public := func(idx uint32) bool {
		return ipdata.PublicV4(netip.AddrFrom4([4]byte{byte(idx >> 16), byte(idx >> 8), byte(idx), 1}))
	}
	// Walk positions, not indexes: sorting them gives the walk order and walkAt gives the index back.
	var refresh, gap []uint32
	blockList := make([]uint32, 0, len(named))
	for idx := range named {
		blockList = append(blockList, idx>>shift)
		if !done.has(idx) && public(idx) {
			refresh = append(refresh, walkPos(idx))
		}
	}
	slices.Sort(blockList)
	blockList = slices.Compact(blockList)
	blocks = len(blockList)
	for _, b := range blockList {
		lo, hi := b<<shift, (b+1)<<shift // the block is [lo, hi)
		r := sort.Search(len(g.runs), func(k int) bool { return g.runs[k].first+g.runs[k].count > lo })
		for ; r < len(g.runs) && g.runs[r].first < hi; r++ {
			from := max(g.runs[r].first, lo)
			to := min(g.runs[r].first+g.runs[r].count, hi)
			for idx := from; idx < to; idx++ {
				if _, isNamed := named[idx]; isNamed || done.has(idx) || !public(idx) {
					continue
				}
				gap = append(gap, walkPos(idx))
			}
		}
	}
	slices.Sort(refresh)
	slices.Sort(gap)
	candidates = len(refresh) + len(gap)
	for _, group := range [][]uint32{refresh, gap} {
		for _, pos := range group {
			if len(picked) >= want {
				return picked, candidates, blocks
			}
			idx := walkAt(pos)
			country, ok := named[idx]
			if !ok {
				country, _ = g.contains(idx)
			}
			a := netip.AddrFrom4([4]byte{byte(idx >> 16), byte(idx >> 8), byte(idx), 0})
			p := netip.PrefixFrom(a, 24)
			picked = append(picked, sampled{Prefix: p, Country: country, Hosts: sampleHosts(p, intn), Near: true})
		}
	}
	return picked, candidates, blocks
}

// loadNamed reads the /24s that have a row of scope named in public.rdns_geo_layer, as /24 index to
// country. Read only, no DDL; a missing table is no rows. Errors carry no value (dbError).
func loadNamed(ctx context.Context, conn *pgx.Conn) (map[uint32]string, error) {
	var table *string
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.rdns_geo_layer')::text`).Scan(&table); err != nil {
		return nil, dbError("check named rows", err)
	}
	named := map[uint32]string{}
	if table == nil {
		return named, nil
	}
	rows, err := conn.Query(ctx, `SELECT host(prefix), country FROM public.rdns_geo_layer
		WHERE scope = 'named' AND family(prefix) = 4 AND masklen(prefix) = 24`)
	if err != nil {
		return nil, dbError("read named rows", err)
	}
	defer rows.Close()
	for rows.Next() {
		var addr string
		var country *string
		if err := rows.Scan(&addr, &country); err != nil {
			return nil, dbError("read named rows", err)
		}
		a, err := netip.ParseAddr(addr)
		if err != nil || !a.Is4() || country == nil || len(*country) != 2 {
			continue
		}
		b := a.As4()
		named[uint32(b[0])<<16|uint32(b[1])<<8|uint32(b[2])] = strings.ToUpper(*country)
	}
	if err := rows.Err(); err != nil {
		return nil, dbError("read named rows", err)
	}
	return named, nil
}
