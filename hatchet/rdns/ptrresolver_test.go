package rdns

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestCheckPTRResolver(t *testing.T) {
	for addr, ok := range map[string]bool{
		"":                 true,
		"127.0.0.1:5335":   true,
		"127.1.2.3:53":     true,
		"[::1]:5335":       true,
		"127.0.0.1:65535":  true,
		"127.0.0.1":        false,
		"127.0.0.1:0":      false,
		"127.0.0.1:65536":  false,
		"127.0.0.1:-1":     false,
		"127.0.0.1:053":    false,
		"127.0.0.1:abc":    false,
		"localhost:5335":   false,
		"8.8.8.8:53":       false,
		"10.0.0.1:53":      false,
		"0.0.0.0:53":       false,
		"[::]:53":          false,
		"[fe80::1]:53":     false,
		"::1:5335":         false,
		"127.0.0.1:53 ":    false,
		" 127.0.0.1:53":    false,
		"http://127.0.0.1": false,
	} {
		err := CheckPTRResolver(addr)
		if (err == nil) != ok {
			t.Errorf("CheckPTRResolver(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
}

// fakeDNS answers PTR questions over UDP: names containing "9.7.6.5" get a PTR record,
// everything else NXDOMAIN. It returns the address and the number of questions seen.
func fakeDNS(t *testing.T) (addr string, questions func() int) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no UDP on loopback: %v", err)
	}
	t.Cleanup(func() { pc.Close() })
	var n int
	var mu = make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 512)
		for {
			size, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			q := append([]byte(nil), buf[:size]...)
			if len(q) < 17 {
				continue
			}
			end := 12
			for end < len(q) && q[end] != 0 {
				end += int(q[end]) + 1
			}
			end += 5 // root label, type, class
			if end > len(q) {
				continue
			}
			mu <- struct{}{}
			n++
			<-mu
			qname := strings.ToLower(string(q[13 : end-5])) // labels with length bytes; good enough to search
			resp := append([]byte(nil), q[:end]...)
			flags := uint16(0x8180) // response, recursion desired and available
			if strings.Contains(qname, "9\x017\x016\x015") {
				binary.BigEndian.PutUint16(resp[6:], 1)
				resp = append(resp, 0xC0, 0x0C, 0, 12, 0, 1, 0, 0, 0, 60)
				rdata := []byte("\x04host\x07example\x03net\x00")
				resp = binary.BigEndian.AppendUint16(resp, uint16(len(rdata)))
				resp = append(resp, rdata...)
			} else {
				flags |= 3 // NXDOMAIN
			}
			binary.BigEndian.PutUint16(resp[2:], flags)
			pc.WriteTo(resp, from)
		}
	}()
	return pc.LocalAddr().String(), func() int { mu <- struct{}{}; defer func() { <-mu }(); return n }
}

func TestPTRResolverUsesLocalAddress(t *testing.T) {
	addr, questions := fakeDNS(t)
	var dohCalls int
	doh := func(context.Context, netip.Addr) ptrAnswer {
		dohCalls++
		return ptrAnswer{PTR: "via.doh.example", Status: statusOK}
	}
	r := &ptrResolver{system: systemLookup(addr), doh: doh}

	got, viaDoH := r.lookup(context.Background(), netip.MustParseAddr("5.6.7.9"))
	if got != (ptrAnswer{PTR: "host.example.net", Status: statusOK}) || viaDoH {
		t.Errorf("ok lookup: %+v viaDoH=%v", got, viaDoH)
	}
	got, viaDoH = r.lookup(context.Background(), netip.MustParseAddr("5.6.7.10"))
	if got != (ptrAnswer{Status: statusNotFound}) || viaDoH {
		t.Errorf("nxdomain lookup: %+v viaDoH=%v", got, viaDoH)
	}
	if dohCalls != 0 || questions() < 2 {
		t.Errorf("doh calls %d, questions seen %d", dohCalls, questions())
	}

	// A dead port: the system lookup fails and the (fake) DoH answers.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	dead := pc.LocalAddr().String()
	pc.Close()
	r = &ptrResolver{system: systemLookup(dead), doh: doh}
	start := time.Now()
	got, viaDoH = r.lookup(context.Background(), netip.MustParseAddr("5.6.7.9"))
	if got.PTR != "via.doh.example" || !viaDoH || dohCalls != 1 {
		t.Errorf("dead port: %+v viaDoH=%v doh calls=%d (after %v)", got, viaDoH, dohCalls, time.Since(start))
	}
}

func TestPTRResolverEmptyAddressUsesDefault(t *testing.T) {
	old := defaultLookupAddr
	defer func() { defaultLookupAddr = old }()
	var asked string
	defaultLookupAddr = func(_ context.Context, ip string) ([]string, error) {
		asked = ip
		return []string{"Stub.Example."}, nil
	}
	r := newPTRResolver("")
	got, viaDoH := r.lookup(context.Background(), netip.MustParseAddr("5.6.7.9"))
	if asked != "5.6.7.9" || got != (ptrAnswer{PTR: "stub.example", Status: statusOK}) || viaDoH {
		t.Errorf("asked %q, got %+v viaDoH=%v", asked, got, viaDoH)
	}
}
