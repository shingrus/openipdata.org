package geofeeds

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func TestScanObjects(t *testing.T) {
	text := "% comment\r\n" +
		"inetnum:        192.0.2.0 - 192.0.2.255\r\n" +
		"remarks:        Geofeed\r\n" +
		"                https://example.net/geofeed.csv\r\n" +
		"+\r\n" +
		"\r\n" +
		"\r\n" +
		"# another comment\n" +
		"route6: 2001:db8::/32\n" +
		"geofeed: https://example.org/feed.csv\n"
	var got [][]attr
	err := scanObjects(strings.NewReader(text), func(obj []attr) {
		got = append(got, append([]attr(nil), obj...))
	})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]attr{
		{{"inetnum", "192.0.2.0 - 192.0.2.255"}, {"remarks", "Geofeed https://example.net/geofeed.csv"}},
		{{"route6", "2001:db8::/32"}, {"geofeed", "https://example.org/feed.csv"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("objects = %q, want %q", got, want)
	}
}

func TestFeedURLs(t *testing.T) {
	tests := []struct {
		name string
		obj  []attr
		want []string
	}{
		{"geofeed attribute", []attr{{"geofeed", "https://a.example/feed.csv"}}, []string{"https://a.example/feed.csv"}},
		{"remarks with keyword", []attr{{"remarks", "Geofeed: https://b.example/x.csv."}}, []string{"https://b.example/x.csv"}},
		{"keyword in the URL", []attr{{"remarks", "see https://c.example/geofeed.txt"}}, []string{"https://c.example/geofeed.txt"}},
		{"unrelated URL", []attr{{"remarks", "abuse reports: https://d.example/abuse"}}, nil},
		{"duplicate", []attr{{"geofeed", "https://e.example/f"}, {"remarks", "Geofeed https://e.example/f"}}, []string{"https://e.example/f"}},
		{"not http", []attr{{"geofeed", "ftp://f.example/feed.csv"}}, nil},
		{"two in one value", []attr{{"descr", "geofeeds (https://g.example/1.csv, https://g.example/2.csv)"}},
			[]string{"https://g.example/1.csv", "https://g.example/2.csv"}},
	}
	for _, tt := range tests {
		if got := feedURLs(tt.obj); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestObjectPrefixes(t *testing.T) {
	tests := []struct {
		obj     attr
		want    []string
		ok, bad bool
	}{
		{attr{"inetnum", "192.0.2.0 - 192.0.2.255"}, []string{"192.0.2.0/24"}, true, false},
		{attr{"inetnum", "10.0.0.1 - 10.0.0.6"}, []string{"10.0.0.1/32", "10.0.0.2/31", "10.0.0.4/31", "10.0.0.6/32"}, true, false},
		{attr{"inetnum", "200.3.12/22"}, []string{"200.3.12.0/22"}, true, false},
		{attr{"inet6num", "2001:db8:1::/48"}, []string{"2001:db8:1::/48"}, true, false},
		{attr{"route", "198.51.100.7/24"}, []string{"198.51.100.0/24"}, true, false},
		{attr{"inetnum", "10.0.0.9 - 10.0.0.1"}, nil, true, true},
		{attr{"inetnum", "nonsense"}, nil, true, true},
		{attr{"aut-num", "AS64496"}, nil, false, false},
	}
	for _, tt := range tests {
		prefixes, ok, err := objectPrefixes([]attr{tt.obj})
		var got []string
		for _, p := range prefixes {
			got = append(got, p.String())
		}
		if ok != tt.ok || (err != nil) != tt.bad || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%v: got %v ok=%v err=%v, want %v ok=%v bad=%v", tt.obj, got, ok, err, tt.want, tt.ok, tt.bad)
		}
	}
}

func TestRangeToPrefixes(t *testing.T) {
	tests := []struct {
		from, to string
		want     []string
	}{
		{"0.0.0.0", "255.255.255.255", []string{"0.0.0.0/0"}},
		{"255.255.255.255", "255.255.255.255", []string{"255.255.255.255/32"}},
		{"192.0.2.0", "192.0.3.127", []string{"192.0.2.0/24", "192.0.3.0/25"}},
		{"2001:db8::", "2001:db8::ffff", []string{"2001:db8::/112"}},
		{"::", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", []string{"::/0"}},
	}
	for _, tt := range tests {
		var got []string
		for _, p := range rangeToPrefixes(netip.MustParseAddr(tt.from), netip.MustParseAddr(tt.to)) {
			got = append(got, p.String())
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s - %s: got %v, want %v", tt.from, tt.to, got, tt.want)
		}
	}
}

func TestCleanURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"HTTPS://RIE.MARU.CO.JP/1E100.CSV", "https://rie.maru.co.jp/1E100.CSV"},
		{"https://Geofeed.Example.co.uk/", "https://geofeed.example.co.uk/"},
		{"https://a.example/geofeed.csv?", "https://a.example/geofeed.csv"},
		{"https://a.example/geofeed.csv#top", "https://a.example/geofeed.csv"},
		{"https://a.example/Feed%20One.csv?as=64496).", "https://a.example/Feed%20One.csv?as=64496"},
		{"http://a.example:8080/g.csv", "http://a.example:8080/g.csv"},
	}
	for _, tt := range tests {
		if got, ok := cleanURL(tt.in); !ok || got != tt.want {
			t.Errorf("cleanURL(%q) = %q %v, want %q", tt.in, got, ok, tt.want)
		}
	}
	for _, bad := range []string{"ftp://a.example/f", "https:///nohost", "not a url"} {
		if got, ok := cleanURL(bad); ok {
			t.Errorf("cleanURL(%q) = %q, want rejected", bad, got)
		}
	}
}

func TestParseFeedList(t *testing.T) {
	got := parseFeedList("# comment\n\nhttps://a.example/feed.csv \nnot a url\n")
	if !reflect.DeepEqual(got, []string{"https://a.example/feed.csv"}) {
		t.Errorf("got %q", got)
	}
	if len(parseFeedList(listedFeeds)) < 20 {
		t.Error("embedded feeds.txt looks empty")
	}
}
