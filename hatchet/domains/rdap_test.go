package domains

import (
	"encoding/json"
	"testing"
)

func vcardEntity(roles string, props ...string) string {
	list := `[["version", {}, "text", "4.0"]`
	for _, p := range props {
		list += ", " + p
	}
	return `{"roles": ` + roles + `, "vcardArray": ["vcard", ` + list + `]]}`
}

func emailProp(addr string) string { return `["email", {}, "text", "` + addr + `"]` }
func urlProp(u string) string      { return `["url", {}, "uri", "` + u + `"]` }

func TestDomainFromRDAP(t *testing.T) {
	cases := []struct {
		name         string
		rdap         string
		want         string
		wantRejected int
	}{
		{
			name: "registrant wins over an abuse contact listed first",
			rdap: `{"entities": [` +
				vcardEntity(`["abuse"]`, emailProp("abuse@upstream-isp.net")) + `, ` +
				vcardEntity(`["registrant"]`, emailProp("hostmaster@example.org")) + `]}`,
			want: "example.org",
		},
		{
			name: "url before email within one contact",
			rdap: `{"entities": [` + vcardEntity(`["registrant"]`, emailProp("noc@mailhost.net"), urlProp("https://www.example.com/")) + `]}`,
			want: "example.com",
		},
		{
			name: "free mail is skipped for the next candidate",
			rdap: `{"entities": [` +
				vcardEntity(`["registrant"]`, emailProp("someone@gmail.com")) + `, ` +
				vcardEntity(`["technical"]`, emailProp("noc@ops.example.net")) + `]}`,
			want: "example.net", wantRejected: 1,
		},
		{
			name: "registry contacts are skipped",
			rdap: `{"entities": [` +
				vcardEntity(`["registrant"]`, emailProp("ip@nic.or.kr")) + `, ` +
				vcardEntity(`["administrative"]`, emailProp("ip-adm@example.kr")) + `]}`,
			want: "example.kr", wantRejected: 1,
		},
		{
			name: "nested abuse contact of the registrant ranks after admin",
			rdap: `{"entities": [{"roles": ["registrant"], "entities": [` +
				vcardEntity(`["abuse"]`, emailProp("abuse@abuse-desk.example")) + `]}, ` +
				vcardEntity(`["administrative"]`, emailProp("admin@example.co.uk")) + `]}`,
			want: "example.co.uk",
		},
		{
			name: "contacts with other roles come last",
			rdap: `{"entities": [` + vcardEntity(`["noc"]`, emailProp("Ops <ops@example.io>")) + `]}`,
			want: "example.io",
		},
		{
			name: "email values may be lists and mailto URIs",
			rdap: `{"entities": [{"roles": ["registrant"], "vcardArray": ["vcard", [["email", {}, "text", ["x", "mailto:noc@example.info"]]]]}]}`,
			want: "example.info",
		},
		{
			name: "only rejected candidates means nothing found",
			rdap: `{"entities": [` + vcardEntity(`["registrant"]`, emailProp("a@gmail.com"), emailProp("b@hotmail.com")) + `]}`,
			want: "", wantRejected: 2,
		},
		{
			name: "no entities",
			rdap: `{"handle": "AS64500"}`,
			want: "",
		},
	}
	for _, tc := range cases {
		var record any
		if err := json.Unmarshal([]byte(tc.rdap), &record); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got, rejectedCount := domainFromRDAP(record)
		if got != tc.want || rejectedCount != tc.wantRejected {
			t.Errorf("%s: got %q (rejected %d), want %q (rejected %d)", tc.name, got, rejectedCount, tc.want, tc.wantRejected)
		}
	}
}
