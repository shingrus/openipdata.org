package mlab

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"openipdata.org/hatchet/ipdata"
	"openipdata.org/hatchet/outputs"
	"openipdata.org/hatchet/rdns"
)

const (
	// minLayerRows: a layer with fewer rows is a failure and does not replace the old file.
	minLayerRows = 1000

	hopsInFile  = "mmdb-ips.txt" // the hop IPs and client .1 addresses to look up, one per line; removed after the run
	hopsOutFile = "mmdb-ips.tsv" // mmdbbuilder lookup's answer; removed after the run

	lookupTimeout = 15 * time.Minute

	whoLocate = "import-mlab locate"
)

// Why the MMDB part was skipped (LocateOutput.MMDBReason): fixed texts, never an error's text.
const (
	reasonNoBuilder = "mmdbbuilder path unknown"
	reasonLookup    = "mmdbbuilder lookup failed"
)

// decodeFunc turns a router name into the city a rule finds in it (rdns.Decoder.Decode).
type decodeFunc func(name string) (country, city string, ok bool)

// lookupFunc runs "mmdbbuilder lookup -with-source -countries" on the MMDB at db for the IPs in
// inFile and writes ip<TAB>country<TAB>city<TAB>city_source rows to outFile. Tests replace it.
type lookupFunc func(ctx context.Context, db, inFile, outFile string) error

// place is a country (two capital letters) and a city name.
type place struct{ Country, City string }

func (p place) valid() bool {
	return ipdata.IsCountryCode(p.Country) && p.City != "" && !strings.ContainsAny(p.City, "\t\r\n")
}

func cleanPlace(country, city string) (place, bool) {
	p := place{strings.ToUpper(strings.TrimSpace(country)), strings.Join(strings.Fields(city), " ")}
	return p, p.valid()
}

// cityKey makes the spelling of a city comparable: case and spacing only (as the geo layers do).
func cityKey(p place) place { return place{p.Country, strings.ToLower(p.City)} }

// readLookup reads mmdbbuilder lookup -with-source -countries rows,
// ip<TAB>country<TAB>city<TAB>city_source, one at a time (the output of a month of clients is big).
// It keeps the place of the IPs in need (sorted) when the record's source is one of
// mmdbStrongSources and has a valid country and city, and the country of the IPs that are the .1 of
// a client /24 in clients (sorted). A row that is not four fields with an IPv4 address is skipped.
func readLookup(r io.Reader, need, clients []uint32) (places map[uint32]place, countries map[uint32][2]byte, err error) {
	places = map[uint32]place{}
	countries = map[uint32][2]byte{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 4 {
			continue
		}
		ip, ok := parseHost(strings.TrimSpace(f[0]))
		if !ok {
			continue
		}
		country := strings.ToUpper(strings.TrimSpace(f[1]))
		if ip&255 == 1 && ipdata.IsCountryCode(country) && hasHop(clients, ip>>8) {
			countries[ip>>8] = [2]byte{country[0], country[1]}
		}
		if slices.Contains(mmdbStrongSources, strings.TrimSpace(f[3])) && hasHop(need, ip) {
			if p, ok := cleanPlace(f[1], f[2]); ok {
				places[ip] = p
			}
		}
	}
	return places, countries, sc.Err()
}

// rulePlaces decodes the name of every hop with the production rules. named counts the hops that
// have a name.
func rulePlaces(hops []uint32, names map[uint32]string, decode decodeFunc) (places map[uint32]place, named int) {
	places = map[uint32]place{}
	for _, h := range hops {
		name, ok := names[h]
		if !ok || name == "" {
			continue
		}
		named++
		if decode == nil {
			continue
		}
		if country, city, ok := decode(name); ok {
			if p, ok := cleanPlace(country, city); ok {
				places[h] = p
			}
		}
	}
	return places, named
}

// layerRow is one row of the layer: a client /24 and its city.
type layerRow struct {
	Client  uint32 // first three octets
	Country string
	City    string
}

// voteStats counts what vote found.
type voteStats struct {
	Clients   int // client /24s of the pairs
	NoCountry int // of them, no country in our MMDB (only counted when the MMDB is used): not voted
	NoCity    int // of them, no hop has a city (in the client's country)
	Located   int // rows
	ByRule    int // winners with at least one hop whose rule gave the city
	ByMMDB    int // winners whose hops all got the city from the MMDB
	Ties      int // client /24s skipped: two cities with the same largest weight
}

type cities struct {
	ids   map[place]uint32
	keys  []place
	forms []map[string]int
}

func (c *cities) id(p place) uint32 {
	k := cityKey(p)
	id, ok := c.ids[k]
	if !ok {
		c.keys = append(c.keys, k)
		c.forms = append(c.forms, map[string]int{})
		id = uint32(len(c.keys))
		c.ids[k] = id
	}
	c.forms[id-1][p.City]++
	return id
}

// spelling is the form of the city written most often (ties: the smallest string).
func (c *cities) spelling(id uint32) string {
	var best string
	n := 0
	for f, k := range c.forms[id-1] {
		if k > n || (k == n && f < best) {
			best, n = f, k
		}
	}
	return best
}

// vote is the pure core, the variant measured on RIPE Atlas ("R then M, trace-weighted"). Per
// client /24 two rounds: first the hops whose router name a rule decodes (R) vote; if that gives no
// winner, the hops with a city in a strong-source MMDB record (M) vote. In a round every hop with a
// city votes with the traces it has there (from_end 0 to 2 count the same); the city with the
// largest weight wins and a tie is no winner. An M city is not used for a hop that lies inside the
// client /24 itself (its own record holds the /24: no evidence). When countries (the client /24's
// country in our MMDB) is not nil, only a city in the client's country votes and a client with no
// country does not vote: the layer adds cities, never changes a country. pairs must be sorted by
// client /24.
func vote(pairs []pair, rule, mmdb map[uint32]place, countries map[uint32][2]byte) ([]layerRow, voteStats) {
	var st voteStats
	c := &cities{ids: map[place]uint32{}}
	ruleID := make(map[uint32]uint32, len(rule))
	for h, p := range rule {
		ruleID[h] = c.id(p)
	}
	mmdbID := make(map[uint32]uint32, len(mmdb))
	for h, p := range mmdb {
		mmdbID[h] = c.id(p)
	}
	type tally struct {
		id uint32
		w  uint64
	}
	var tallies []tally
	// round tallies the group's hops with city(hop) and returns the winner (0: none) and whether
	// two cities tied for the largest weight.
	round := func(group []pair, cc string, city func(pr pair) uint32) (win uint32, tied bool) {
		tallies = tallies[:0]
		for _, pr := range group {
			id := city(pr)
			if id == 0 || (countries != nil && c.keys[id-1].Country != cc) {
				continue
			}
			k := slices.IndexFunc(tallies, func(t tally) bool { return t.id == id })
			if k < 0 {
				tallies = append(tallies, tally{id: id})
				k = len(tallies) - 1
			}
			tallies[k].w += uint64(pr.traces)
		}
		if len(tallies) == 0 {
			return 0, false
		}
		best := 0
		for k := 1; k < len(tallies); k++ {
			switch {
			case tallies[k].w > tallies[best].w:
				best, tied = k, false
			case tallies[k].w == tallies[best].w:
				tied = true
			}
		}
		if tied {
			return 0, true
		}
		return tallies[best].id, false
	}
	var rows []layerRow
	for i := 0; i < len(pairs); {
		client := pairs[i].client()
		j := i
		for j < len(pairs) && pairs[j].client() == client {
			j++
		}
		group := pairs[i:j]
		i = j
		st.Clients++
		var cc string
		if countries != nil {
			cb, ok := countries[client]
			if !ok {
				st.NoCountry++
				continue
			}
			cc = string(cb[:])
		}
		win, tied := round(group, cc, func(pr pair) uint32 { return ruleID[pr.hop] })
		byRule := win != 0
		if win == 0 {
			var tied2 bool
			win, tied2 = round(group, cc, func(pr pair) uint32 {
				if pr.hop>>8 == client {
					return 0
				}
				return mmdbID[pr.hop]
			})
			tied = tied || tied2
		}
		switch {
		case win != 0:
			rows = append(rows, layerRow{Client: client, Country: c.keys[win-1].Country, City: c.spelling(win)})
			st.Located++
			if byRule {
				st.ByRule++
			} else {
				st.ByMMDB++
			}
		case tied:
			st.Ties++
		default:
			st.NoCity++
		}
	}
	return rows, st
}

// writeLayer writes the layer file (gzipped CSV in the geofeed layout) through a temporary file
// and a rename. Rows are in client order, which is address order.
func writeLayer(path string, rows []layerRow) (int64, error) {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp)
	gz := gzip.NewWriter(f)
	w := csv.NewWriter(gz)
	w.Write([]string{"prefix", "country", "region", "city", "source"})
	var prefix []byte
	for _, r := range rows {
		prefix = appendClient(prefix[:0], r.Client)
		prefix = append(prefix, ".0/24"...)
		w.Write([]string{string(prefix), r.Country, "", r.City, layerSource})
	}
	w.Flush()
	if err := errors.Join(w.Error(), gz.Close(), f.Close()); err != nil {
		return 0, err
	}
	fi, err := os.Stat(tmp)
	if err != nil {
		return 0, err
	}
	return fi.Size(), os.Rename(tmp, path)
}

// LocateOutput counts a locate run (counts only).
type LocateOutput struct {
	Clients    int    `json:"clients"`      // client /24s of the pairs file
	Hops       int    `json:"hops"`         // distinct hop IPs
	Named      int    `json:"named"`        // hops with an ok PTR name
	RuleHops   int    `json:"rule_hops"`    // hops whose name a rule decodes to a city
	MMDBHops   int    `json:"mmdb_hops"`    // hops with a city in a strong-source MMDB record
	Located    int    `json:"located"`      // client /24s given a city: rows of the layer
	ByRule     int    `json:"by_rule"`      // of them, won in round R (rule cities)
	ByMMDB     int    `json:"by_mmdb"`      // of them, won in round M (MMDB cities)
	Ties       int    `json:"ties"`         // client /24s with no city: two cities with the same largest weight in the last round that had votes
	NoCity     int    `json:"no_city"`      // client /24s with no city: no hop has one (in the client's country)
	NoCountry  int    `json:"no_country"`   // client /24s not voted: no country in our MMDB
	Legacy     int    `json:"legacy_rules"` // legacy rules read
	Domains    int    `json:"rule_domains"` // DNS suffixes with candidate rules read (learned, LLM and Hoiho rules together)
	MMDB       string `json:"mmdb"`         // "used", "saved copy" or "skipped"
	MMDBReason string `json:"mmdb_reason,omitempty"`
	Bytes      int64  `json:"bytes"`
}

// Locate is the locate task: it writes the layer file from the pairs, the stored names, the rules
// and our published MMDB (hop cities, and the country of each client /24). An MMDB problem only skips the MMDB part; fewer than minLayerRows rows
// is a failure that leaves the old layer file as it was.
func (p *Pipeline) Locate(ctx context.Context) (LocateOutput, error) {
	var out LocateOutput
	pairs, err := readPairs(p.pairsPath())
	if err != nil {
		return out, err
	}
	hops := distinctHops(pairs)
	out.Hops = len(hops)

	names := p.readNames
	if names == nil {
		names = p.readOKNames
	}
	named, err := names(ctx, hops)
	if err != nil {
		return out, err
	}
	load := p.loadDecoder
	if load == nil {
		load = func(ctx context.Context) (decodeFunc, error) {
			d, err := rdns.LoadDecoder(ctx, p.PGSQL)
			if err != nil {
				return nil, err
			}
			out.Legacy, out.Domains = d.Counts()
			return d.Decode, nil
		}
	}
	decode, err := load(ctx)
	if err != nil {
		return out, err
	}
	rule, namedHops := rulePlaces(hops, named, decode)
	out.Named, out.RuleHops = namedHops, len(rule)

	// The MMDB city of every hop (the second round of the vote) and the country of every client /24.
	mmdb, countries, res := p.mmdbPlaces(ctx, hops, distinctClients(pairs))
	out.MMDB, out.MMDBReason, out.MMDBHops = res.Status, res.Reason, len(mmdb)
	// A cancelled run must not write a layer without the MMDB part.
	if err := ctx.Err(); err != nil {
		return out, err
	}

	rows, st := vote(pairs, rule, mmdb, countries)
	out.Clients, out.Located, out.ByRule, out.ByMMDB, out.Ties, out.NoCity, out.NoCountry = st.Clients, st.Located, st.ByRule, st.ByMMDB, st.Ties, st.NoCity, st.NoCountry
	minRows := p.minLayerRows
	if minRows == 0 {
		minRows = minLayerRows
	}
	if len(rows) < minRows {
		return out, permanent("only %d client /24s got a city, fewer than %d; the old %s stays", len(rows), minRows, LayerFile)
	}
	if err := os.MkdirAll(p.dir(), 0o750); err != nil {
		return out, err
	}
	if out.Bytes, err = writeLayer(p.LayerPath(), rows); err != nil {
		return out, fmt.Errorf("write %s: %w", LayerFile, err)
	}
	p.log("locate: %d client /24s, %d hops, %d named, %d with a rule city, %d with an MMDB city (mmdb %s); %d /24s located (%d by rule, %d by MMDB), %d ties, %d without a city",
		out.Clients, out.Hops, out.Named, out.RuleHops, out.MMDBHops, out.MMDB, out.Located, out.ByRule, out.ByMMDB, out.Ties, out.NoCity)
	return out, nil
}

// mmdbResult is what mmdbPlaces found about the MMDB.
type mmdbResult struct{ Status, Reason string }

func (p *Pipeline) mmdbCopy() *rdns.MMDBCopy {
	return &rdns.MMDBCopy{Dir: p.dir(), R2: p.R2, Progress: p.Progress, Store: p.store, Now: p.now}
}

// distinctClients returns the client /24s of pairs (sorted: pairs are).
func distinctClients(pairs []pair) []uint32 {
	var out []uint32
	for i, pr := range pairs {
		if i == 0 || pr.client() != pairs[i-1].client() {
			out = append(out, pr.client())
		}
	}
	return out
}

// mmdbPlaces returns the strong-source MMDB cities of the hops and the country of each client /24
// (the country of its .1 address). Any problem skips the MMDB part (a fixed reason, logged; the
// result is then nil maps): the rule part still works, without the country check.
func (p *Pipeline) mmdbPlaces(ctx context.Context, hops, clients []uint32) (map[uint32]place, map[uint32][2]byte, mmdbResult) {
	skip := func(reason string) (map[uint32]place, map[uint32][2]byte, mmdbResult) {
		p.log("locate: WARNING MMDB skipped (%s)", reason)
		return nil, nil, mmdbResult{"skipped", reason}
	}
	if p.lookup == nil && p.MMDBBuilder == "" {
		return skip(reasonNoBuilder)
	}
	copy := p.mmdbCopy()
	state := copy.Prepare(ctx, whoLocate)
	if !state.Usable() {
		return skip(state.Reason)
	}
	places, countries, err := p.lookupMMDB(ctx, state.Path, hops, clients)
	if err != nil {
		if ctx.Err() != nil {
			return skip(reasonLookup)
		}
		p.log("locate: mmdbbuilder lookup: %v", rdns.ScrubIPs(err.Error()))
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			copy.Drop() // the copy may be what is wrong: fetch it again next time
		}
		return skip(reasonLookup)
	}
	return places, countries, mmdbResult{state.Status, state.Reason}
}

// lookupMMDB asks the MMDB at db about every hop IP and the .1 of every client /24: the IPs go to a
// run file in the mlab dir, mmdbbuilder lookup writes its answer to another, and both are removed.
func (p *Pipeline) lookupMMDB(ctx context.Context, db string, hops, clients []uint32) (map[uint32]place, map[uint32][2]byte, error) {
	inFile, outFile := filepath.Join(p.dir(), hopsInFile), filepath.Join(p.dir(), hopsOutFile)
	for _, f := range []string{inFile, inFile + ".tmp", outFile, outFile + ".tmp"} {
		defer os.Remove(f)
		os.Remove(f) // left by a run that was killed
	}
	if err := os.MkdirAll(p.dir(), 0o750); err != nil {
		return nil, nil, err
	}
	if err := writeIPList(inFile, hops, clients); err != nil {
		return nil, nil, fmt.Errorf("write %s: %w", hopsInFile, err)
	}
	run := p.lookup
	if run == nil {
		run = p.runLookup
	}
	if err := run(ctx, db, inFile, outFile); err != nil {
		return nil, nil, err
	}
	f, err := os.Open(outFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", hopsOutFile, err)
	}
	defer f.Close()
	return readLookup(f, hops, clients)
}

// writeIPList writes the hop IPs and the .1 of each client /24, one per line, through a temporary file.
func writeIPList(path string, hops, clients []uint32) error {
	f, err := os.Create(path + ".tmp")
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	var line []byte
	for _, h := range hops {
		line = append(appendHop(line[:0], h), '\n')
		w.Write(line)
	}
	for _, c := range clients {
		line = append(appendHop(line[:0], c<<8|1), '\n')
		w.Write(line)
	}
	if err := errors.Join(w.Flush(), f.Close()); err != nil {
		os.Remove(path + ".tmp")
		return err
	}
	return os.Rename(path+".tmp", path)
}

// runLookup is the default lookupFunc: the mmdbbuilder next to the worker. Its stderr only counts
// skipped lines, never the lines themselves; the text still goes through ScrubIPs.
func (p *Pipeline) runLookup(ctx context.Context, db, inFile, outFile string) error {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, p.MMDBBuilder, "lookup", "-db", db, "-in", inFile, "-out", outFile, "-with-source", "-countries")
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mmdbbuilder lookup: %w: %s", err, rdns.ScrubIPs(outputs.LastLines(stderr.String(), 3)))
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		p.log("locate: mmdbbuilder lookup: %s", rdns.ScrubIPs(outputs.LastLines(msg, 3)))
	}
	return nil
}

// readOKNames reads the ok names of the hops from public.mlab_hop_names (no DDL; a missing table
// means no names). Only names of IPs in the sorted slice hops are kept.
func (p *Pipeline) readOKNames(ctx context.Context, hops []uint32) (map[uint32]string, error) {
	if err := p.CheckPGSQL(); err != nil {
		return nil, err
	}
	conn, err := rdns.Connect(ctx, p.PGSQL)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	var table *string
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.mlab_hop_names')::text`).Scan(&table); err != nil {
		return nil, rdns.DBError("read hop names", err)
	}
	names := map[uint32]string{}
	if table == nil {
		return names, nil
	}
	rows, err := conn.Query(ctx, `SELECT host(ip), ptr FROM public.mlab_hop_names WHERE status = 'ok' AND ptr <> ''`)
	if err != nil {
		return nil, rdns.DBError("read hop names", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ip, ptr string
		if err := rows.Scan(&ip, &ptr); err != nil {
			return nil, rdns.DBError("read hop names", err)
		}
		if v, ok := parseHost(ip); ok && hasHop(hops, v) {
			names[v] = ptr
		}
	}
	if err := rows.Err(); err != nil {
		return nil, rdns.DBError("read hop names", err)
	}
	return names, nil
}
