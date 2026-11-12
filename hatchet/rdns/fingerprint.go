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
)

// locateState is the content of locateStateFile.
type locateState struct {
	Fingerprint string    `json:"fingerprint"`
	RebuiltAt   time.Time `json:"rebuilt_at"`
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
	// fingerprintRulesQuery hashes the columns of rulesQuery in its order:
	// generated_rules has no update column to look at.
	fingerprintRulesQuery = `SELECT count(*)::text || ':' || coalesce(md5(string_agg(concat_ws(E'\x1f', name, pattern, domains_json::text, country, city), E'\x1e'
			ORDER BY lower(name), name)), '')
		FROM public.generated_rules`
)

// inputsFingerprint summarizes the inputs of locate; equal text means nothing
// locate reads has changed. It reads four short summaries and no rows.
func inputsFingerprint(ctx context.Context, conn *pgx.Conn) (string, error) {
	var parts [4]string
	if err := conn.QueryRow(ctx, fingerprintTracesQuery, locateWindow).Scan(&parts[0]); err != nil {
		return "", dbError("fingerprint traces", err)
	}
	if err := conn.QueryRow(ctx, fingerprintNamesQuery).Scan(&parts[1]); err != nil {
		return "", dbError("fingerprint names", err)
	}
	var table *string
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.generated_rules')::text`).Scan(&table); err != nil {
		return "", dbError("fingerprint rules", err)
	}
	parts[2] = "no table"
	if table != nil {
		if err := conn.QueryRow(ctx, fingerprintRulesQuery).Scan(&parts[2]); err != nil {
			return "", dbError("fingerprint rules", err)
		}
	}
	if err := conn.QueryRow(ctx, fingerprintASNQuery).Scan(&parts[3]); err != nil {
		return "", dbError("fingerprint asn", err)
	}
	data, _ := json.Marshal(parts)
	return string(data), nil
}

// canSkip reports whether a locate with these inputs may leave the table as it
// is: the last rebuild had the same fingerprint and is less than locateMaxAge old.
func (s locateState) canSkip(fingerprint string, now time.Time) bool {
	age := now.Sub(s.RebuiltAt)
	return s.Fingerprint != "" && s.Fingerprint == fingerprint && age >= 0 && age < locateMaxAge
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
