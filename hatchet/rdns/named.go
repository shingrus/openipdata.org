package rdns

import (
	"context"
	"net/netip"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// scopeNamed rows of rdns_geo_layer: a gap /24 whose customer PTR names (see sample.go) decode to
// one city, layer 67-rdns-names. They are volatile: recomputed every rebuild from the current
// rules, never sticky (not carried, not in the shrink guard), never marking traced_networks
// located, and they cast no wide vote.
const scopeNamed = "named"

// customerName is the PTR name of one sampled host.
type customerName struct {
	IP  netip.Addr
	PTR string
}

// customerNet is a /24 of customer_name_networks with the ok names of its hosts.
type customerNet struct {
	Prefix  netip.Prefix
	Country string
	Names   []customerName
}

// namedStats counts what locateNamed found.
type namedStats struct {
	Networks int // /24s read (with at least one name)
	Names    int // names read
	Matched  int // names that gave a city
	Rows     int // /24s given a row
	Ties     int // /24s skipped: two cities with the most names
	Foreign  int // /24s skipped: the city's country is not the row's
}

// locateNamed gives each /24 the city most of its names decode to. Per name the rules are tried as
// target names are in locate: the legacy rules first, then the learned candidate rules (a
// domain's ranks in order); a name gives at most one city. The /24 gets the city with the most
// names (a tie gives none) when its country is the row's country. The row's router_ip and
// router_ptr are the first host (by address) that gave the winning city, a legacy match
// preferred; rule_name and method are that name's, as for traced rows. same_asn is NULL.
func locateNamed(nets []customerNet, rules []rule, cityRules cityRuleIndex) ([]geoRow, namedStats) {
	var st namedStats
	var rows []geoRow
	type found struct {
		city   cityKey
		ip     netip.Addr
		ptr    string
		rule   string
		method string
	}
	for _, n := range nets {
		if len(n.Names) == 0 {
			continue
		}
		st.Networks++
		var hits []found
		for _, nm := range n.Names {
			st.Names++
			if r := firstMatch(rules, nm.PTR); r != nil {
				hits = append(hits, found{cityKey{r.country, r.city}, nm.IP, nm.PTR, r.name, methodRule})
			} else if c, r, ok := firstCityRuleMatchRule(cityRules, nm.PTR); ok {
				hits = append(hits, found{c, nm.IP, nm.PTR, candidateRuleName(r), methodCandidate})
			} else {
				continue
			}
			st.Matched++
		}
		count := map[cityKey]int{}
		for _, h := range hits {
			count[h.city]++
		}
		var top cityKey
		topN, tied := 0, false
		for c, k := range count {
			switch {
			case k > topN:
				top, topN, tied = c, k, false
			case k == topN:
				tied = true
			}
		}
		if topN == 0 {
			continue
		}
		if tied {
			st.Ties++
			continue
		}
		if !strings.EqualFold(top.Country, n.Country) {
			st.Foreign++
			continue
		}
		var best *found
		for i := range hits {
			h := &hits[i]
			if h.city != top {
				continue
			}
			if best == nil || (best.method != methodRule && h.method == methodRule) ||
				(best.method == h.method && h.ip.Less(best.ip)) {
				best = h
			}
		}
		rows = append(rows, geoRow{Prefix: n.Prefix, Scope: scopeNamed, Country: top.Country, City: top.City,
			RouterIP: best.ip, PTR: best.ptr, Rule: best.rule, Method: best.method})
		st.Rows++
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Prefix.Addr().Less(rows[j].Prefix.Addr()) })
	return rows, st
}

// customerNetsQuery reads the sampled /24s of the last $1 (an interval, as text) with the hosts that
// have a name and the names. Read only.
const customerNetsQuery = `SELECT c.prefix::text, c.country, host(h.ip), h.ptr
	FROM public.customer_name_networks c
	CROSS JOIN LATERAL unnest(c.hosts, c.names) AS h(ip, ptr)
	WHERE c.sampled_at > now() - $1::text::interval AND h.ptr <> ''
	ORDER BY c.prefix, h.ip`

// readCustomerNets reads the sampled /24s of the last window with their ok names. A missing
// table (sample-names has not run) means none.
func readCustomerNets(ctx context.Context, conn *pgx.Conn, window string) ([]customerNet, error) {
	var table *string
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.customer_name_networks')::text`).Scan(&table); err != nil {
		return nil, dbError("read customer names", err)
	}
	if table == nil {
		return nil, nil
	}
	rows, err := conn.Query(ctx, customerNetsQuery, window)
	if err != nil {
		return nil, dbError("read customer names", err)
	}
	defer rows.Close()
	var nets []customerNet
	for rows.Next() {
		var prefix, country, ip, ptr string
		if err := rows.Scan(&prefix, &country, &ip, &ptr); err != nil {
			return nil, dbError("read customer names", err)
		}
		p, perr := netip.ParsePrefix(prefix)
		a, aerr := netip.ParseAddr(ip)
		if perr != nil || aerr != nil {
			continue
		}
		if len(nets) == 0 || nets[len(nets)-1].Prefix != p {
			nets = append(nets, customerNet{Prefix: p, Country: strings.ToUpper(strings.TrimSpace(country))})
		}
		last := &nets[len(nets)-1]
		last.Names = append(last.Names, customerName{IP: a.Unmap(), PTR: ptr})
	}
	if err := rows.Err(); err != nil {
		return nil, dbError("read customer names", err)
	}
	return nets, nil
}
