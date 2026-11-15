package main

import (
	"regexp"
	"strings"
	"testing"
)

func TestParseUA(t *testing.T) {
	const brave = `"Chromium";v="156", "Brave";v="156", "Not.A/Brand";v="24"`
	chromeMac := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36"
	for _, c := range []struct {
		name, ua, ch string
		want         browserInfo
	}{
		{"chrome macOS", chromeMac, "", browserInfo{"Chrome", "156", "macOS"}},
		{"brave", chromeMac, brave, browserInfo{"Brave", "156", "macOS"}},
		{"chromium brand only", chromeMac, `"Chromium";v="156"`, browserInfo{"Chrome", "156", "macOS"}},
		{"safari iOS", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1", "", browserInfo{"Safari", "18", "iOS"}},
		{"safari macOS", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/19.0 Safari/605.1.15", "", browserInfo{"Safari", "19", "macOS"}},
		{"firefox windows", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:146.0) Gecko/20100101 Firefox/146.0", "", browserInfo{"Firefox", "146", "Windows"}},
		{"firefox iOS", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/146.0 Mobile/15E148 Safari/605.1.15", "", browserInfo{"Firefox", "146", "iOS"}},
		{"edge windows", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36 Edg/156.0.2800.12", "", browserInfo{"Edge", "156", "Windows"}},
		{"opera", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36 OPR/121.0.0.0", "", browserInfo{"Opera", "121", "Windows"}},
		{"samsung android", "Mozilla/5.0 (Linux; Android 15; SM-S928B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/29.0 Chrome/136.0.0.0 Mobile Safari/537.36", "", browserInfo{"Samsung Internet", "29", "Android"}},
		{"vivaldi linux", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36 Vivaldi/7.5.3735.54", "", browserInfo{"Vivaldi", "7", "Linux"}},
		{"chrome android", "Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Mobile Safari/537.36", "", browserInfo{"Chrome", "156", "Android"}},
		{"chrome iOS", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/156.0.0.0 Mobile/15E148 Safari/604.1", "", browserInfo{"Chrome", "156", "iOS"}},
		{"chromeos", "Mozilla/5.0 (X11; CrOS x86_64 16093.57.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36", "", browserInfo{"Chrome", "156", "ChromeOS"}},
		{"ubuntu firefox", "Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:146.0) Gecko/20100101 Firefox/146.0", "", browserInfo{"Firefox", "146", "Ubuntu"}},
		{"curl", "curl/8.7.1", "", browserInfo{}},
		{"empty", "", "", browserInfo{}},
		{"garbage", "\x00\xff ???", "", browserInfo{}},
		{"safari without version", "Mozilla/5.0 (Macintosh) Safari/605", "", browserInfo{"", "", "macOS"}},
		{"version digits only, at most 4", "Chrome/123456.7", "", browserInfo{"Chrome", "1234", ""}},
	} {
		if got := parseUA(c.ua, c.ch); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
	// Only the first 512 bytes are read.
	if got := parseUA(strings.Repeat("a", 600)+"Firefox/1", ""); got.Browser != "" {
		t.Errorf("read past 512 bytes: %+v", got)
	}
}

var (
	idRE  = regexp.MustCompile(`\sid="([^"]*)"`)
	refRE = regexp.MustCompile(`url\(#([^)]*)\)|href="#([^"]*)"`)
)

func TestIconsPrepared(t *testing.T) {
	for name, h := range icons.browser {
		s := string(h)
		ids := map[string]bool{}
		for _, m := range idRE.FindAllStringSubmatch(s, -1) {
			if !strings.HasPrefix(m[1], "b-") {
				t.Errorf("%s: id %q has no b- prefix", name, m[1])
			}
			ids[m[1]] = true
		}
		for _, m := range refRE.FindAllStringSubmatch(s, -1) {
			ref := m[1] + m[2]
			if !ids[ref] {
				t.Errorf("%s: reference #%s has no id in the icon", name, ref)
			}
		}
		for _, bad := range []string{"<?xml", "<title", "<!--", "> <", ">\n"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s: icon holds %q", name, bad)
			}
		}
		if !strings.HasPrefix(s, `<svg class="ic" aria-hidden="true" focusable="false"`) {
			t.Errorf("%s: svg start %.80s", name, s)
		}
	}
	for name, h := range icons.os {
		if s := string(h); strings.Contains(s, "id=") || strings.Contains(s, "<title") || !strings.Contains(s, ` fill="`) || !strings.Contains(s, `class="ic"`) {
			t.Errorf("%s: icon %.100s", name, s)
		}
	}
	if len(icons.browser) != 8 || len(icons.os) != 7 {
		t.Errorf("icons: %d browsers, %d systems", len(icons.browser), len(icons.os))
	}
}

const macUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36"

func TestBrowserRowPage(t *testing.T) {
	s, _ := shareServer(t)
	vis := func(ua string) string {
		return fromPeer(s, "/", "127.0.0.1:5000", "X-Real-IP", "203.0.113.9", "User-Agent", ua).Body.String()
	}
	b := vis(macUA)
	row := b[strings.Index(b, "<dt>Browser</dt>"):]
	row = row[:strings.Index(row, "</dd>")]
	for _, x := range []string{`<dd class="br">`, `<svg class="ic"`, `<span>Chrome 156</span>`, `<span class="sep">·</span>`,
		`<span class="os" role="img" aria-label="macOS" title="macOS">`, `fill="currentColor"`, `<button class="hd" type="button" hidden>Headers</button>`} {
		if !strings.Contains(row, x) {
			t.Errorf("row lacks %s:\n%s", x, row)
		}
	}
	if strings.Contains(row, "macOS</span>") {
		t.Error("the OS name is shown as text")
	}
	if o, n := strings.Index(row, `class="os"`), strings.Index(row, "Chrome 156"); o < 0 || o > n {
		t.Error("the OS logo is not first")
	}
	if strings.Contains(row, `id="`) && !strings.Contains(row, `id="b-`) {
		t.Error("unprefixed id")
	}
	head := b[strings.Index(b, `<div class="head`):]
	head = head[:strings.Index(head, "</div>")]
	if strings.Contains(head, `class="hd"`) || !strings.Contains(head, `class="sh"`) {
		t.Errorf("head: %s", head)
	}
	if i, j := strings.Index(b, "<dt>Network</dt>"), strings.Index(b, "<dt>Browser</dt>"); i < 0 || j < i {
		t.Error("the Browser row is not after Network")
	}
	// Unknown browser and system: a dash and the button; an empty User-Agent too.
	for _, ua := range []string{"curl/8.7.1", ""} {
		r := vis(ua)
		row := r[strings.Index(r, "<dt>Browser</dt>"):]
		row = row[:strings.Index(row, "</dd>")]
		if !strings.Contains(row, `<span>—</span>`) || strings.Contains(row, "<svg") || strings.Contains(row, "sep") || !strings.Contains(row, "Headers") {
			t.Errorf("unknown %q: %s", ua, row)
		}
	}
	// Brave comes from Sec-CH-UA.
	r := fromPeer(s, "/", "127.0.0.1:5000", "X-Real-IP", "203.0.113.9", "User-Agent", macUA, "Sec-CH-UA", `"Brave";v="156"`).Body.String()
	if !strings.Contains(r, "<span>Brave 156</span>") {
		t.Error("no Brave")
	}
	// Windows is drawn in code.
	r = vis("Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:146.0) Gecko/20100101 Firefox/146.0")
	if !strings.Contains(r, `aria-label="Windows"`) || !strings.Contains(r, `fill="#0078D4"`) {
		t.Error("no Windows mark")
	}
	if w := fromPeer(s, "/", "127.0.0.1:5000", "X-Real-IP", "203.0.113.9", "User-Agent", macUA); w.Header().Get("Cache-Control") != "private, no-store" {
		t.Errorf("cache %q", w.Header().Get("Cache-Control"))
	}
}

func TestBrowserRowOnlyOnTheVisitorPage(t *testing.T) {
	s, m := shareServer(t)
	hdr := []string{"X-Real-IP", "203.0.113.9", "User-Agent", macUA}
	for _, target := range []string{"/?203.0.113.9", "/?1.2.3", "/?10.1.2.3", "/as64500", "/download", "/status"} {
		b := fromPeer(s, target, "127.0.0.1:5000", hdr...).Body.String()
		if strings.Contains(b, "<dt>Browser</dt>") || strings.Contains(b, `class="ic"`) || strings.Contains(b, "b-") && strings.Contains(b, `id="b-`) {
			t.Errorf("%s has the Browser row or icons", target)
		}
	}
	if b := fromPeer(s, "/", "127.0.0.1:5000", "User-Agent", macUA).Body.String(); strings.Contains(b, "<dt>Browser</dt>") { // unknown address
		t.Error("unknown address has the row")
	}
	id := share(t, s, "203.0.113.9", "")
	if v := m.kv["report:"+id]; strings.Contains(v, "Chrome") || strings.Contains(v, "Mozilla") || strings.Contains(v, "macOS") {
		t.Error("the report holds the user agent")
	}
	b := fromPeer(s, "/report/"+id, "127.0.0.1:5000", hdr...).Body.String()
	if strings.Contains(b, "<dt>Browser</dt>") || strings.Contains(b, `class="ic"`) || strings.Contains(b, `class="hd"`) {
		t.Error("the report page has the row")
	}
}

func TestAPIHint(t *testing.T) {
	s, _ := shareServer(t)
	b := fromPeer(s, "/", "127.0.0.1:5000", "X-Real-IP", "203.0.113.9").Body.String()
	want := `<p class="hint">API: <a href="/json">curl openipdata.org/json</a></p>`
	if !strings.Contains(b, want) {
		t.Errorf("visitor hint missing")
	}
	if i, j, k := strings.Index(b, want), strings.Index(b, "</section>"), strings.Index(b, "<h2>Accuracy"); !(j < i && i < k) {
		t.Error("the hint is not between the panels")
	}
	if strings.Contains(b, "/json/203.0.113.9") {
		t.Error("visitor address in the hint")
	}
	for target, hint := range map[string]string{
		"/?203.0.113.9": `<a href="/json/203.0.113.9">curl openipdata.org/json/203.0.113.9</a>`,
	} {
		b := fromPeer(s, target, "127.0.0.1:5000", "X-Real-IP", "198.51.100.7").Body.String()
		if !strings.Contains(b, `<p class="hint">API: `+hint+`</p>`) {
			t.Errorf("%s: hint missing", target)
		}
		if strings.Contains(b, `"/json"`) || strings.Contains(b, "198.51.100.7") {
			t.Errorf("%s: wrong address in the hint", target)
		}
	}
	for _, target := range []string{"/?1.2.3", "/?10.1.2.3", "/as64500", "/download", "/top-asn-by-ip-address"} {
		if b := fromPeer(s, target, "127.0.0.1:5000", "X-Real-IP", "203.0.113.9").Body.String(); strings.Contains(b, `class="hint"`) {
			t.Errorf("%s has the hint", target)
		}
	}
	if b := fromPeer(s, "/", "127.0.0.1:5000").Body.String(); strings.Contains(b, `class="hint"`) { // address unknown
		t.Error("unknown address has the hint")
	}
	if b := fromPeer(s, "/?2001:db8::1", "127.0.0.1:5000").Body.String(); !strings.Contains(b, `href="/json/2001:db8::1"`) && !strings.Contains(b, `class="msg"`) {
		t.Error("IPv6 lookup hint")
	}
}
