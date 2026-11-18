package rdns

import (
	"context"
	"strings"
	"testing"

	"openipdata.org/hatchet/schema"
)

// The imported rules reach the decoder and the fingerprint of locate (needs a Postgres).
func TestIntegrationHoihoRulesDecoderAndFingerprint(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := schema.EnsureRDNS(ctx, w.conn); err != nil {
		t.Fatal(err)
	}
	before, err := inputsSummary(ctx, w.conn)
	if err != nil || !strings.Contains(before[2], "|hoiho:no table") {
		t.Fatalf("without the table: %q %v", before[2], err)
	}
	if err := schema.EnsureRDNSHoihoRules(ctx, w.conn); err != nil {
		t.Fatal(err)
	}
	w.exec(`INSERT INTO public.rdns_hoiho_rules (domain, seq, pattern, plan, places, class, version) VALUES
		('example.net', 1, '^[a-z0-9]+\.([a-z]{3})\d+\.([a-z]{2})\.example\.net$', 'iata,cc', '{"ams,nl":["NL","Amsterdam"]}', 'good', 'v1')`)
	withRow, err := inputsSummary(ctx, w.conn)
	if err != nil || !strings.Contains(withRow[2], "|hoiho:1:") || withRow[2] == before[2] {
		t.Fatalf("with a row: %q %v", withRow[2], err)
	}
	d, err := LoadDecoder(ctx, w.dsn)
	if err != nil {
		t.Fatal(err)
	}
	if country, city, ok := d.Decode("ae1.ams01.nl.example.net"); !ok || country != "NL" || city != "Amsterdam" {
		t.Errorf("decode = %q %q %v", country, city, ok)
	}
	// A new version, and a rejected row, change the fingerprint; a rejected row no longer decodes.
	w.exec(`UPDATE public.rdns_hoiho_rules SET version = 'v2'`)
	changed, err := inputsSummary(ctx, w.conn)
	if err != nil || changed[2] == withRow[2] {
		t.Fatalf("version change: %q %v", changed[2], err)
	}
	w.exec(`UPDATE public.rdns_hoiho_rules SET status = 'rejected'`)
	rejected, err := inputsSummary(ctx, w.conn)
	if err != nil || rejected[2] == changed[2] {
		t.Fatalf("status change: %q %v", rejected[2], err)
	}
	if d, err = LoadDecoder(ctx, w.dsn); err != nil {
		t.Fatal(err)
	} else if _, _, ok := d.Decode("ae1.ams01.nl.example.net"); ok {
		t.Error("a rejected row decoded")
	}
	// locate reports the Hoiho rules it loaded.
	w.exec(`UPDATE public.rdns_hoiho_rules SET status = 'candidate'`)
	out, err := w.p.Locate(ctx, true)
	if err != nil || out.HoihoRules != 1 || out.CandidateRules != 1 {
		t.Fatalf("locate: %+v %v", out, err)
	}
}
