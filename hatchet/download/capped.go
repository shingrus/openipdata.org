package download

import (
	"errors"
	"net/http"
	"time"
)

// ErrTooLarge is returned by a Capped client when a response is over the limit.
var ErrTooLarge = errors.New("response too large")

// Capped returns a copy of c whose response bodies end in an error after limit bytes, and whose
// requests fail at once when the server announces more. File has no size limit of its own, so
// this keeps a huge body from filling the disk. A nil client gets a 2 minute timeout.
func Capped(c *http.Client, limit int64) *http.Client {
	if c == nil {
		c = &http.Client{Timeout: 2 * time.Minute}
	}
	cp := *c
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	cp.Transport = capTransport{base: base, limit: limit}
	return &cp
}

type capTransport struct {
	base  http.RoundTripper
	limit int64
}

func (t capTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > t.limit {
		resp.Body.Close()
		return nil, ErrTooLarge
	}
	resp.Body = &capBody{rc: resp.Body, left: t.limit}
	return resp, nil
}

type capBody struct {
	rc interface {
		Read([]byte) (int, error)
		Close() error
	}
	left int64
}

func (b *capBody) Read(p []byte) (int, error) {
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	if b.left < 0 {
		return 0, ErrTooLarge
	}
	return n, err
}

func (b *capBody) Close() error { return b.rc.Close() }
