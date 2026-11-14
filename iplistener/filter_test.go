package main

import "testing"

func TestFilter(t *testing.T) {
	f, err := NewFilter(`"(GET|POST) /api/\w+`, `cc=T1|bot|crawl|spider`)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		line string
		want Result
		ip   string
	}{
		{`8.8.8.8 - - [x] "POST /api/beacon HTTP/1.1" 200 cc=US`, Kept, "8.8.8.8"},
		{"8.8.4.4\t- \"GET /api/x HTTP/1.1\"", Kept, "8.8.4.4"},
		{`::ffff:8.8.8.8 - "GET /api/x HTTP/1.1"`, Kept, "8.8.8.8"},
		{`8.8.8.8 - "GET /index.html HTTP/1.1"`, NoMatch, ""},
		{`8.8.8.8 - "GET /api/x HTTP/1.1" cc=T1`, Excluded, ""},
		{`8.8.8.8 - "GET /api/x HTTP/1.1" Googlebot`, Excluded, ""},
		{`10.1.2.3 - "GET /api/x"`, BadIP, ""},
		{`192.168.1.1 - "GET /api/x"`, BadIP, ""},
		{`172.16.0.1 - "GET /api/x"`, BadIP, ""},
		{`100.64.0.1 - "GET /api/x"`, BadIP, ""},
		{`100.127.255.254 - "GET /api/x"`, BadIP, ""},
		{`100.128.0.1 - "GET /api/x"`, Kept, "100.128.0.1"},
		{`127.0.0.1 - "GET /api/x"`, BadIP, ""},
		{`169.254.1.1 - "GET /api/x"`, BadIP, ""},
		{`224.0.0.1 - "GET /api/x"`, BadIP, ""},
		{`0.0.0.0 - "GET /api/x"`, BadIP, ""},
		{`2001:4860:4860::8888 - "GET /api/x"`, BadIP, ""},
		{`::1 - "GET /api/x"`, BadIP, ""},
		{`unix: - "GET /api/x"`, BadIP, ""},
		{`"GET /api/x"`, BadIP, ""},
	}
	for _, c := range cases {
		ip, got := f.Check(c.line)
		if got != c.want || (got == Kept && ip.String() != c.ip) {
			t.Errorf("%q: got %v %v, want %v %q", c.line, got, ip, c.want, c.ip)
		}
	}
}

func TestFilterInvalid(t *testing.T) {
	if _, err := NewFilter(`(`, ""); err == nil {
		t.Error("invalid FILTER accepted")
	}
	if _, err := NewFilter(`x`, `(`); err == nil {
		t.Error("invalid EXCLUDE accepted")
	}
}
