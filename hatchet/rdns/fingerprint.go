package rdns

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"

	"openipdata.org/hatchet/ipdata"
)

const (
	// locateStateFile, in the rdns dir on the VM, remembers the last rebuild of
	// rdns_geo_layer: the fingerprint of its inputs and when it happened. Only locate
	// reads or writes it; losing it only costs one rebuild.
	locateStateFile = "locate-state.json"
	// locateMaxAge is how long an unchanged set of inputs may keep the table as it is.
	locateMaxAge = 24 * time.Hour
	// locateMinInterval is how long the table may stay as it is when the inputs changed but the
	// rules did not: new traces, names, ASN data, customer samples or a new MMDB wait for the next
	// rebuild, at most this long. A rule change rebuilds at once.
	locateMinInterval = 6 * time.Hour
)

// Why locate skipped its rebuild (LocateOutput.SkipReason).
const (
	skipUnchanged = "unchanged" // the same fingerprint, less than locateMaxAge old
	skipRecent    = "recent"    // other inputs changed, the rules did not, less than locateMinInterval old
)

// locateState is the content of locateStateFile.
type locateState struct {
	Fingerprint string    `json:"fingerprint"`
	RebuiltAt   time.Time `json:"rebuilt_at"`
	// Rules is the rules part of the inputs (inputsSummary, part 2) at that rebuild. A file
	// written before this field existed has none: the 6-hour skip then does not apply.
	Rules string `json:"rules,omitempty"`
}

// fingerprintTracesQuery, fingerprintNamesQuery and fingerprintASNQuery summarize
// what locate reads (the traces of its window, the usable PTR names, the current
// ASNs) as a count and the newest timestamp, computed in Postgres so no rows are
// sent. A new or pruned trace, a new or refreshed name, and a build-asn-data run
// that saw the ASNs all change the summary. Read only.
const (
	fingerprintTracesQuery = `SELECT count(*)::text || ':' || coalesce(max(traced_at)::text, '')
		FROM public.network_traces WHERE traced_at > now() - $1::text::interval`
	fingerprintNamesQuery = `SELECT count(*)::text || ':' || coalesce(max(resolved_at)::text, '')
		FROM public.ip_ptr_names WHERE status = 'ok' AND ptr IS NOT NULL`
	fingerprintASNQuery = `SELECT count(*)::text || ':' || coalesce(max(a.seen_at)::text, '')
		FROM public.asn a WHERE ` + ipdata.SeenAtFilter
	// fingerprintCustomerQuery: the UTC day of the newest sampled /24 locate reads (sample-names). Only the
	// day, so new samples cause at most one extra rebuild a day; a rule change still rebuilds at once.
	fingerprintCustomerQuery = `SELECT coalesce((date_trunc('day', max(sampled_at) AT TIME ZONE 'UTC'))::text, '')
		FROM public.customer_name_networks WHERE sampled_at > now() - $1::text::interval`
	// fingerprintRulesQuery hashes the columns of rulesQuery in its order:
	// generated_rules has no update column to look at.
	fingerprintRulesQuery = `SELECT count(*)::text || ':' || coalesce(md5(string_agg(concat_ws(E'\x1f', name, pattern, domains_json::text, country, city), E'\x1e'
			ORDER BY lower(name), name)), '')
		FROM public.generated_rules`
	fingerprintCityRulesQuery = `SELECT count(*)::text || ':' || coalesce(md5(string_agg(concat_ws(E'\x1f', domain, rank::text, pattern, dictionary, places::text, version, status), E'\x1e'
			ORDER BY lower(domain), domain, rank)), '') FROM public.rdns_city_rules`
)

// inputsSummary summarizes the database inputs of locate: five short texts, no rows.
func inputsSummary(ctx context.Context, conn *pgx.Conn) ([5]string, error) {
	var parts [5]string
	if err := conn.QueryRow(ctx, fingerprintTracesQuery, locateWindow).Scan(&parts[0]); err != nil {
		return parts, dbError("fingerprint traces", err)
	}
	if err := conn.QueryRow(ctx, fingerprintNamesQuery).Scan(&parts[1]); err != nil {
		return parts, dbError("fingerprint names", err)
	}
	var table *string
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.generated_rules')::text`).Scan(&table); err != nil {
		return parts, dbError("fingerprint rules", err)
	}
	parts[2] = "no table"
	if table != nil {
		if err := conn.QueryRow(ctx, fingerprintRulesQuery).Scan(&parts[2]); err != nil {
			return parts, dbError("fingerprint rules", err)
		}
	}
	var cityTable *string
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.rdns_city_rules')::text`).Scan(&cityTable); err != nil {
		return parts, dbError("fingerprint candidate rules", err)
	}
	cityFingerprint := "no table"
	if cityTable != nil {
		if err := conn.QueryRow(ctx, fingerprintCityRulesQuery).Scan(&cityFingerprint); err != nil {
			return parts, dbError("fingerprint candidate rules", err)
		}
	}
	parts[2] += "|candidate:" + cityFingerprint
	if err := conn.QueryRow(ctx, fingerprintASNQuery).Scan(&parts[3]); err != nil {
		return parts, dbError("fingerprint asn", err)
	}
	var customerTable *string
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.customer_name_networks')::text`).Scan(&customerTable); err != nil {
		return parts, dbError("fingerprint customer names", err)
	}
	parts[4] = "no table"
	if customerTable != nil {
		if err := conn.QueryRow(ctx, fingerprintCustomerQuery, customerWindow).Scan(&parts[4]); err != nil {
			return parts, dbError("fingerprint customer names", err)
		}
	}
	return parts, nil
}

// fingerprintOf is the fingerprint of the inputs of locate: the summary of the
// database inputs and the ETag of the MMDB the hop fallback reads (noMMDB when
// the fallback has none). Equal text means nothing locate reads has changed.
func fingerprintOf(summary [5]string, mmdbETag string) string {
	if mmdbETag == "" {
		mmdbETag = noMMDB
	}
	data, _ := json.Marshal([6]string{summary[0], summary[1], summary[2], summary[3], summary[4], mmdbETag})
	return string(data)
}

// canSkip reports whether a locate with these inputs may leave the table as it is, and why
// (skipUnchanged, skipRecent; empty when it must rebuild). Unchanged: the last rebuild had the
// same fingerprint and is less than locateMaxAge old. Recent: the fingerprint differs, but the
// rules are the ones of the last rebuild (a state without them never matches) and that rebuild is
// less than locateMinInterval old. A rebuild time in the future never skips.
func (s locateState) canSkip(fingerprint, rules string, now time.Time) (bool, string) {
	age := now.Sub(s.RebuiltAt)
	if s.Fingerprint == "" || age < 0 {
		return false, ""
	}
	if s.Fingerprint == fingerprint && age < locateMaxAge {
		return true, skipUnchanged
	}
	if s.Rules != "" && s.Rules == rules && age < locateMinInterval {
		return true, skipRecent
	}
	return false, ""
}

// rebuiltState is the state saved after a rebuild. mmdbFromR2 says whether the rebuild read the
// MMDB that R2 offered. When it did not (a saved copy during an outage, another copy after a
// failed download, or none), the rules part is left out, so the 6-hour skip does not apply and
// the next run rebuilds and tries R2 again, as it always did.
func rebuiltState(fingerprint, rules string, now time.Time, mmdbFromR2 bool) locateState {
	s := locateState{Fingerprint: fingerprint, RebuiltAt: now}
	if mmdbFromR2 {
		s.Rules = rules
	}
	return s
}

// readLocateState reads the state file; a missing or damaged one is an empty
// state (no skip).
func readLocateState(path string) locateState {
	var s locateState
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &s) != nil {
		return locateState{}
	}
	return s
}

// writeLocateState writes the state file through a temporary file.
func writeLocateState(path string, s locateState) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
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
