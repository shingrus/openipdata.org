package main

// A tiny Redis client over loopback, one connection per command: HGET (the
// resolver of a beacon name), GET, SET with EX and SET XX KEEPTTL (shared
// reports). Standard library only. No key, token, id or value is ever logged
// or put in an error: errors are one of a few fixed kinds.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	redisDeadline = 300 * time.Millisecond // dial, write and read together
	redisMaxValue = 64                     // longest reply value (an address is shorter)
)

var (
	errRedisConnect = errors.New("cannot connect")
	errRedisTimeout = errors.New("timeout")
	errRedisReply   = errors.New("unexpected reply")
)

type redisClient struct {
	addr string

	mu      sync.Mutex
	lastErr string // the kind logged last; cleared by a good answer
}

// checkRedisAddr accepts host:port with a loopback IP or "localhost".
func checkRedisAddr(v string) error {
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return errors.New("not host:port")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return errors.New("invalid port")
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("must be a loopback address")
	}
	return nil
}

// reply is one Redis answer: a status line ("+OK"), an integer, or a bulk
// string (null = the key does not exist).
type reply struct {
	kind byte // '+', ':' or '$'
	text string
	null bool
}

// call sends one command and reads one answer. A bulk longer than max, an
// error reply and anything unexpected are errors of fixed kinds. Nothing of
// the command or the answer is logged or put in an error.
func (c *redisClient) call(max int, args ...string) (reply, error) {
	dl := time.Now().Add(redisDeadline)
	conn, err := (&net.Dialer{Deadline: dl}).Dial("tcp", c.addr)
	if err != nil {
		return reply{}, errRedisConnect
	}
	defer conn.Close()
	conn.SetDeadline(dl)
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := io.WriteString(conn, b.String()); err != nil {
		return reply{}, redisErr(err)
	}
	br := bufio.NewReaderSize(conn, 128)
	line, err := br.ReadSlice('\n') // a longer line is bufio.ErrBufferFull: unexpected
	if err != nil {
		return reply{}, redisErr(err)
	}
	if len(line) < 4 || line[len(line)-2] != '\r' {
		return reply{}, errRedisReply
	}
	body := string(line[1 : len(line)-2])
	switch line[0] {
	case '+', ':':
		return reply{kind: line[0], text: body}, nil
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil || n < -1 || n > max {
			return reply{}, errRedisReply
		}
		if n == -1 {
			return reply{kind: '$', null: true}, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(br, buf); err != nil {
			return reply{}, redisErr(err)
		}
		if buf[n] != '\r' || buf[n+1] != '\n' {
			return reply{}, errRedisReply
		}
		return reply{kind: '$', text: string(buf[:n])}, nil
	}
	return reply{}, errRedisReply // error reply or garbage
}

// note logs the kind of a failure once until the next good answer.
func (c *redisClient) note(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case err == nil:
		c.lastErr = ""
	case err.Error() != c.lastErr:
		c.lastErr = err.Error()
		log.Printf("redis: %v", err)
	}
}

// hget returns the value of the field, found = false for a null reply. Any
// other outcome is an error of a fixed kind.
func (c *redisClient) hget(key, field string) (val string, found bool, err error) {
	r, err := c.call(redisMaxValue, "HGET", key, field)
	c.note(err)
	if err == nil && r.kind != '$' {
		err = errRedisReply
	}
	if err != nil || r.null {
		return "", false, err
	}
	return r.text, true, nil
}

// get returns the string at key (at most max bytes), found = false for none.
func (c *redisClient) get(key string, max int) (val string, found bool, err error) {
	r, err := c.call(max, "GET", key)
	c.note(err)
	if err == nil && r.kind != '$' {
		err = errRedisReply
	}
	if err != nil || r.null {
		return "", false, err
	}
	return r.text, true, nil
}

// setEX stores val at key, to expire after ttl (whole seconds).
func (c *redisClient) setEX(key, val string, ttl time.Duration) error {
	r, err := c.call(8, "SET", key, val, "EX", strconv.Itoa(int(ttl/time.Second)))
	if err == nil && (r.kind != '+' || r.text != "OK") {
		err = errRedisReply
	}
	c.note(err)
	return err
}

// replaceKeepTTL replaces the value of an existing key and keeps its
// remaining lifetime (SET XX KEEPTTL: a key that has just expired is not
// created again, so nothing is left without an expiry). found = false when
// the key does not exist.
func (c *redisClient) replaceKeepTTL(key, val string) (found bool, err error) {
	r, err := c.call(8, "SET", key, val, "XX", "KEEPTTL")
	switch {
	case err != nil:
	case r.kind == '+' && r.text == "OK":
		found = true
	case r.kind == '$' && r.null:
	default:
		err = errRedisReply
	}
	c.note(err)
	return found, err
}

func redisErr(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errRedisTimeout
	}
	return errRedisReply
}

// ping reports whether Redis answers a command (the key cannot exist; a null
// reply is an answer). Nothing is logged.
func (c *redisClient) ping() bool {
	_, err := c.call(redisMaxValue, "HGET", "status:none", "x")
	return err == nil
}
