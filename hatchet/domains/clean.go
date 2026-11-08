// Package domains is the update-asn-domains workflow, the only writer of
// public.asn_domain. Each ASN gets one domain: the PeeringDB website when there
// is one, otherwise the best contact found in its RDAP registration record.
package domains

import (
	"net"
	"strings"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

// rejected lists registered domains that say nothing about who runs an ASN when
// they come from registration contacts: free mail providers and the registries
// themselves. PeeringDB websites are exempt, since networks list them directly.
var rejected = setOf(
	// free mail
	"gmail.com", "googlemail.com", "hotmail.com", "hotmail.co.uk", "hotmail.fr", "outlook.com",
	"live.com", "msn.com", "yahoo.com", "yahoo.co.uk", "yahoo.co.jp", "ymail.com", "aol.com",
	"icloud.com", "me.com", "mac.com", "protonmail.com", "proton.me", "pm.me", "gmx.com",
	"gmx.net", "gmx.de", "web.de", "mail.com", "mail.ru", "bk.ru", "list.ru", "inbox.ru",
	"yandex.ru", "yandex.com", "ya.ru", "rambler.ru", "ukr.net", "qq.com", "foxmail.com",
	"163.com", "126.com", "yeah.net", "sina.com", "sina.cn", "sohu.com", "aliyun.com",
	"naver.com", "hanmail.net", "daum.net", "rediffmail.com", "seznam.cz", "wp.pl", "o2.pl",
	"interia.pl", "libero.it",
	// regional and national internet registries
	"iana.org", "arin.net", "ripe.net", "apnic.net", "lacnic.net", "afrinic.net",
	"nic.or.kr", "kisa.or.kr", "nic.ad.jp", "jpnic.ad.jp", "cnnic.cn", "cnnic.net.cn",
	"twnic.net.tw", "twnic.tw", "vnnic.vn", "vnnic.net.vn", "idnic.net", "irinn.in",
	"registro.br", "nic.br", "nic.mx",
)

func setOf(values ...string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}
	return set
}

// Normalize returns the registered domain of host ("www.example.co.uk" ->
// "example.co.uk"), or "" when host is empty, an IP address, a public suffix,
// or not a valid host name (spaces, "@", underscores, bad hyphens).
func Normalize(host string) string {
	d := strings.Trim(strings.ToLower(strings.TrimSpace(host)), ".")
	if d == "" || net.ParseIP(d) != nil || !strings.Contains(d, ".") {
		return ""
	}
	d, err := idna.Lookup.ToASCII(d)
	if err != nil {
		return ""
	}
	registered, err := publicsuffix.EffectiveTLDPlusOne(d)
	if err != nil {
		return ""
	}
	return registered
}

// Rejected reports whether a registered domain is on the rejection list.
func Rejected(domain string) bool {
	return rejected[domain]
}

// Usable returns the domain to publish for a stored asn_domain row, or "".
// PeeringDB domains are kept as they are; RDAP and older rows of unknown
// origin are dropped when rejected.
func Usable(domain, source string) string {
	d := Normalize(domain)
	if d == "" || (source != SourcePDB && Rejected(d)) {
		return ""
	}
	return d
}
