package main

// A tiny Redis client for one command: HGET beacon:<token> resolver_ip, over
// loopback. Standard library only. Neither the token nor the value is ever
// logged or put in an error: errors are one of a few fixed kinds.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
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

// hget returns the value of the field, found = false for a null reply. Any
// other outcome is an error of a fixed kind.
func (c *redisClient) hget(key, field string) (val string, found bool, err error) {
	val, found, err = c.do(key, field)
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case err == nil:
		c.lastErr = ""
	case err.Error() != c.lastErr:
		c.lastErr = err.Error()
		log.Printf("redis: %v", err)
	}
	return
}

func (c *redisClient) do(key, field string) (string, bool, error) {
	dl := time.Now().Add(redisDeadline)
	conn, err := (&net.Dialer{Deadline: dl}).Dial("tcp", c.addr)
	if err != nil {
		return "", false, errRedisConnect
	}
	defer conn.Close()
	conn.SetDeadline(dl)
	if _, err := fmt.Fprintf(conn, "*3\r\n$4\r\nHGET\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n", len(key), key, len(field), field); err != nil {
		return "", false, redisErr(err)
	}
	br := bufio.NewReaderSize(conn, 128)
	line, err := br.ReadSlice('\n') // a longer line is bufio.ErrBufferFull: unexpected
	if err != nil {
		return "", false, redisErr(err)
	}
	if len(line) < 4 || line[0] != '$' || line[len(line)-2] != '\r' {
		return "", false, errRedisReply // error reply, integer, status or garbage
	}
	n, err := strconv.Atoi(string(line[1 : len(line)-2]))
	if err != nil || n < -1 || n > redisMaxValue {
		return "", false, errRedisReply
	}
	if n == -1 {
		return "", false, nil
	}
	buf := make([]byte, n+2)
	if _, err := io.ReadFull(br, buf); err != nil {
		return "", false, redisErr(err)
	}
	if buf[n] != '\r' || buf[n+1] != '\n' {
		return "", false, errRedisReply
	}
	return string(buf[:n]), true, nil
}

func redisErr(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errRedisTimeout
	}
	return errRedisReply
}
