package schema

import (
	"context"
	"fmt"
)

// rdnsHoihoRulesStatement creates the table of router-name rules imported by the task
// import-hoiho of learn-rdns-rules (its only writer). A row is one regular expression of one DNS
// suffix; seq is its 1-based position in the source record, so the order of a domain's
// expressions is kept even when an earlier one was left out. plan holds the types of the capture
// groups, comma-joined (for example "iata,cc"). places maps the lower-case captures joined with a
// comma to [country, city], like rdns_city_rules.places. version hashes pattern, plan, class and
// places.
const rdnsHoihoRulesStatement = `CREATE TABLE IF NOT EXISTS public.rdns_hoiho_rules (
	domain TEXT NOT NULL,
	seq SMALLINT NOT NULL,
	pattern TEXT NOT NULL,
	plan TEXT NOT NULL,
	places JSONB NOT NULL,
	class TEXT NOT NULL,
	version TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'candidate',
	imported_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (domain, seq)
)`

// rdnsHoihoRejectedStatement creates the list of suffixes whose Hoiho rules are switched off (written
// by learn-rdns-rules only, through run-rdns-rules -reject-hoiho and -accept-hoiho). A row switches
// off the Hoiho rules of the domain and of every name under it; import-hoiho reads it for every
// import, so a rejection outlives the rows it applied to.
const rdnsHoihoRejectedStatement = `CREATE TABLE IF NOT EXISTS public.rdns_hoiho_rejected_domains (
	domain TEXT PRIMARY KEY,
	rejected_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// EnsureRDNSHoihoRules creates public.rdns_hoiho_rules and public.rdns_hoiho_rejected_domains only
// (written by learn-rdns-rules). Like EnsureRDNSCityRules it is not part of Ensure.
func EnsureRDNSHoihoRules(ctx context.Context, db Execer) error {
	for _, stmt := range []string{rdnsHoihoRulesStatement, rdnsHoihoRejectedStatement} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("ensure schema: %w", err)
		}
	}
	return nil
}
