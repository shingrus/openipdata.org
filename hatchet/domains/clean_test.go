package domains

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"":                        "",
		" . ":                     "",
		"com":                     "",
		"co.uk":                   "",
		"localhost":               "",
		"192.0.2.1":               "",
		"2001:db8::1":             "",
		"example.com":             "example.com",
		"WWW.Example.COM.":        "example.com",
		"wholesale.telekom.com":   "telekom.com",
		"noc.ops.example.co.uk":   "example.co.uk",
		"about.google":            "about.google",
		"www.cloudflare.com":      "cloudflare.com",
		"cernet.edu.cn":           "cernet.edu.cn",
		"mail.nic.or.kr":          "nic.or.kr",
		"bücher.example.de":       "example.de",
		"shop.bücher.de":          "xn--bcher-kva.de",
		"someuser.github.io":      "someuser.github.io",
		"one.one.one.one":         "one.one",
		"sub.domain.net":          "domain.net",
		"bad domain.com":          "",
		"foo@example.com":         "",
		"under_score.example.com": "",
		"-bad.example.com":        "",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUsableTopDomains runs the most common asn_domain values from the first
// production export through the rules. Older rows have no source.
func TestUsableTopDomains(t *testing.T) {
	cases := map[string]string{
		"cernet.edu.cn":   "cernet.edu.cn", // kept for now; registrant-first lookups decide
		" ":               "",              // old "looked up, nothing found" marker
		"nic.or.kr":       "",
		"nic.ad.jp":       "",
		"gmail.com":       "",
		"apnic.net":       "",
		"war.gov":         "war.gov",
		"chinatelecom.cn": "chinatelecom.cn",
		"vnnic.vn":        "",
		"verisign.com":    "verisign.com",
		"registro.br":     "",
		"irinn.in":        "",
		"yandex.ru":       "",
		"hotmail.com":     "",
		"orange.com":      "orange.com",
		"qq.com":          "",
		"outlook.com":     "",
		"mail.ru":         "",
		"att.com":         "att.com",
	}
	for in, want := range cases {
		if got := Usable(in, ""); got != want {
			t.Errorf("Usable(%q, legacy) = %q, want %q", in, got, want)
		}
	}
}

func TestUsableBySource(t *testing.T) {
	// Networks list their own websites in PeeringDB: never rejected.
	if got := Usable("www.yandex.ru", SourcePDB); got != "yandex.ru" {
		t.Errorf("pdb yandex = %q", got)
	}
	if got := Usable("yandex.ru", SourceRDAP); got != "" {
		t.Errorf("rdap yandex = %q, want rejected", got)
	}
	if got := Usable("noc.example.net", SourceRDAP); got != "example.net" {
		t.Errorf("rdap example = %q", got)
	}
}
