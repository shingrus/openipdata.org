package rdns

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReverseName(t *testing.T) {
	if got := reverseName(netip.MustParseAddr("5.6.7.9")); got != "9.7.6.5.in-addr.arpa" {
		t.Errorf("reverseName = %s", got)
	}
	if got := reverseName(netip.MustParseAddr("192.0.2.254")); got != "254.2.0.192.in-addr.arpa" {
		t.Errorf("reverseName = %s", got)
	}
}

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"AE1.FRA01.Example.NET.": "ae1.fra01.example.net",
		" host.example.net ":     "host.example.net",
		"":                       "",
		".":                      "",
		"bad name.example":       "",
		strings.Repeat("a", 254): "",
	} {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseDoH(t *testing.T) {
	for name, c := range map[string]struct {
		body string
		want ptrAnswer
	}{
		"answer": {`{"Status":0,"TC":false,"RD":true,"RA":true,"AD":false,"CD":false,"Question":[{"name":"9.7.6.5.in-addr.arpa","type":12}],
			"Answer":[{"name":"9.7.6.5.in-addr.arpa","type":12,"TTL":300,"data":"AE1.Fra01.Example.NET."}]}`, ptrAnswer{"ae1.fra01.example.net", statusOK}},
		"cname first": {`{"Status":0,"Answer":[{"name":"x","type":5,"TTL":1,"data":"y."},{"name":"y","type":12,"TTL":1,"data":"host.example.net."}]}`, ptrAnswer{"host.example.net", statusOK}},
		"nxdomain":    {`{"Status":3,"Question":[{"name":"9.7.6.5.in-addr.arpa","type":12}],"Authority":[{"name":"5.in-addr.arpa","type":6,"TTL":60,"data":"x"}]}`, ptrAnswer{"", statusNotFound}},
		"no answer":   {`{"Status":0,"Question":[{"name":"9.7.6.5.in-addr.arpa","type":12}]}`, ptrAnswer{"", statusNotFound}},
		"only a SOA":  {`{"Status":0,"Answer":[{"name":"x","type":6,"TTL":1,"data":"ns. host."}]}`, ptrAnswer{"", statusNotFound}},
		"servfail":    {`{"Status":2}`, ptrAnswer{"", statusError}},
		"not json":    {`<html>`, ptrAnswer{"", statusError}},
		"empty":       {``, ptrAnswer{"", statusError}},
	} {
		if got := parseDoH([]byte(c.body)); got != c.want {
			t.Errorf("%s: parseDoH = %+v, want %+v", name, got, c.want)
		}
	}
}

func TestDoHClient(t *testing.T) {
	var gotQuery, gotAccept string
	status := http.StatusOK
	body := `{"Status":0,"Answer":[{"name":"9.7.6.5.in-addr.arpa","type":12,"TTL":300,"data":"edge1.fra.example.net."}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotAccept = r.URL.RawQuery, r.Header.Get("Accept")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	defer server.Close()
	d := &dohClient{base: server.URL, http: server.Client()}
	ip := netip.MustParseAddr("5.6.7.9")

	if got := d.lookup(context.Background(), ip); got != (ptrAnswer{"edge1.fra.example.net", statusOK}) {
		t.Errorf("answer = %+v", got)
	}
	if gotQuery != "name=9.7.6.5.in-addr.arpa&type=PTR" || gotAccept != "application/dns-json" {
		t.Errorf("request: query %q, Accept %q", gotQuery, gotAccept)
	}
	status = http.StatusBadGateway
	if got := d.lookup(context.Background(), ip); got.Status != statusError {
		t.Errorf("HTTP 502: %+v", got)
	}
	// An unreachable server is an error answer; nothing of the URL (the IP) is passed on.
	server.Close()
	if got := d.lookup(context.Background(), ip); got.Status != statusError {
		t.Errorf("closed server: %+v", got)
	}
}

func TestPTRResolverFallsBackToDoH(t *testing.T) {
	ip := netip.MustParseAddr("5.6.7.9")
	dohAnswer := ptrAnswer{"via.doh.example", statusOK}
	var dohCalls int
	newResolver := func(names []string, err error) *ptrResolver {
		dohCalls = 0
		return &ptrResolver{
			system: func(context.Context, string) ([]string, error) { return names, err },
			doh:    func(context.Context, netip.Addr) ptrAnswer { dohCalls++; return dohAnswer },
		}
	}
	for name, c := range map[string]struct {
		names   []string
		err     error
		want    ptrAnswer
		viaDoH  bool
		dohUsed int
	}{
		"system name":  {[]string{"Host.Example.NET."}, nil, ptrAnswer{"host.example.net", statusOK}, false, 0},
		"first usable": {[]string{" ", "b.example."}, nil, ptrAnswer{"b.example", statusOK}, false, 0},
		"no names":     {nil, nil, ptrAnswer{"", statusNotFound}, false, 0},
		"nxdomain":     {nil, &net.DNSError{Err: "no such host", IsNotFound: true}, ptrAnswer{"", statusNotFound}, false, 0},
		"timeout":      {nil, &net.DNSError{Err: "i/o timeout", IsTimeout: true}, dohAnswer, true, 1},
		"servfail":     {nil, &net.DNSError{Err: "server misbehaving", IsTemporary: true}, dohAnswer, true, 1},
		"plain error":  {nil, errors.New("resolver down"), dohAnswer, true, 1},
	} {
		r := newResolver(c.names, c.err)
		got, viaDoH := r.lookup(context.Background(), ip)
		if got != c.want || viaDoH != c.viaDoH || dohCalls != c.dohUsed {
			t.Errorf("%s: got %+v viaDoH=%v doh calls=%d", name, got, viaDoH, dohCalls)
		}
	}
	// When both fail the answer is an error, to be retried next run.
	r := &ptrResolver{
		system: func(context.Context, string) ([]string, error) { return nil, errors.New("down") },
		doh:    func(context.Context, netip.Addr) ptrAnswer { return ptrAnswer{Status: statusError} },
	}
	if got, _ := r.lookup(context.Background(), ip); got.Status != statusError {
		t.Errorf("both down: %+v", got)
	}
	// A stopped run does not fall back.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r = newResolver(nil, errors.New("canceled"))
	if got, viaDoH := r.lookup(ctx, ip); got.Status != statusError || viaDoH || dohCalls != 0 {
		t.Errorf("canceled: %+v %v %d", got, viaDoH, dohCalls)
	}
}

// The system lookup gets a timeout of its own.
func TestPTRResolverSystemTimeout(t *testing.T) {
	var deadline time.Time
	r := &ptrResolver{
		system: func(ctx context.Context, _ string) ([]string, error) {
			deadline, _ = ctx.Deadline()
			return []string{"a.example"}, nil
		},
	}
	start := time.Now()
	r.lookup(context.Background(), netip.MustParseAddr("5.6.7.9"))
	if d := deadline.Sub(start); d <= 0 || d > systemTimeout+time.Second {
		t.Errorf("deadline in %v", d)
	}
}

func TestResolveAllBoundedConcurrency(t *testing.T) {
	var running, peak atomic.Int32
	var mu sync.Mutex
	seen := map[string]bool{}
	r := &ptrResolver{
		system: func(_ context.Context, ip string) ([]string, error) {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			running.Add(-1)
			mu.Lock()
			seen[ip] = true
			mu.Unlock()
			if strings.HasSuffix(ip, ".7") {
				return nil, &net.DNSError{IsNotFound: true}
			}
			return []string{"h-" + ip + ".example."}, nil
		},
	}
	var ips []netip.Addr
	for i := 0; i < 50; i++ {
		ips = append(ips, netip.AddrFrom4([4]byte{5, 6, 7, byte(i)}))
	}
	answers, doh := resolveAll(context.Background(), r, ips, 4)
	if len(answers) != 50 || doh != 0 || len(seen) != 50 {
		t.Fatalf("answers %d, doh %d, looked up %d", len(answers), doh, len(seen))
	}
	if peak.Load() > 4 || peak.Load() < 2 {
		t.Errorf("peak concurrency %d, want 2..4", peak.Load())
	}
	// The answers are in the order of the IPs.
	if answers[3] != (ptrAnswer{"h-5.6.7.3.example", statusOK}) || answers[7] != (ptrAnswer{"", statusNotFound}) {
		t.Errorf("answers = %+v %+v", answers[3], answers[7])
	}
	if answers, doh := resolveAll(context.Background(), r, nil, 4); len(answers) != 0 || doh != 0 {
		t.Errorf("no IPs: %v %d", answers, doh)
	}
}

// A failed lookup must not wipe a name that is there: it may replace another
// error, and a new IP gets its error row.
func TestStoreNamesKeepsGoodAnswers(t *testing.T) {
	for _, part := range []string{
		"INSERT INTO public.ip_ptr_names AS t",
		"ON CONFLICT (ip) DO UPDATE",
		"excluded.status <> 'error' OR t.status = 'error'",
	} {
		if !strings.Contains(storeNamesSQL, part) {
			t.Errorf("storeNamesSQL lacks %q", part)
		}
	}
}
