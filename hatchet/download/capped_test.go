package download

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// The download is capped: exactly the limit passes, one byte more fails, and a body that announces
// too much fails at once.
func TestCapped(t *testing.T) {
	const limit = 1000
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/announced":
			w.Header().Set("Content-Length", "5000")
			w.Write([]byte(strings.Repeat("x", 5000)))
		case "/announced-one":
			w.Header().Set("Content-Length", strconv.Itoa(limit+1))
			w.Write([]byte(strings.Repeat("x", limit+1)))
		case "/stream":
			w.(http.Flusher).Flush() // chunked: no Content-Length
			for i := 0; i < 100; i++ {
				w.Write([]byte(strings.Repeat("x", 100)))
				w.(http.Flusher).Flush()
			}
		case "/stream-one":
			w.(http.Flusher).Flush()
			w.Write([]byte(strings.Repeat("x", limit+1)))
		case "/exact":
			w.Write([]byte(strings.Repeat("x", limit)))
		case "/exact-stream":
			w.(http.Flusher).Flush()
			w.Write([]byte(strings.Repeat("x", limit)))
		}
	}))
	defer srv.Close()
	c := Capped(srv.Client(), limit)
	for path, wantErr := range map[string]bool{"/announced": true, "/announced-one": true, "/stream": true,
		"/stream-one": true, "/exact": false, "/exact-stream": false} {
		resp, err := c.Get(srv.URL + path)
		if err == nil {
			var b []byte
			b, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			if err == nil && len(b) != limit {
				t.Errorf("%s: %d bytes", path, len(b))
			}
			if err != nil && len(b) > limit {
				t.Errorf("%s: read %d bytes past the limit", path, len(b))
			}
		}
		if (err != nil) != wantErr {
			t.Errorf("%s: err = %v, want error %v", path, err, wantErr)
		}
	}
	if Capped(nil, limit).Timeout == 0 {
		t.Error("a nil client has no timeout")
	}
}
