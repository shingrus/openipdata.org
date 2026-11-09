package geofeeds

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// fetchFTP downloads an anonymous-FTP file to path unless the local copy is
// current, like fetchFile: the file's modification time holds the server's
// MDTM time (without one, an unchanged SIZE counts as current). Passive mode
// only, IPv4 only, and only public addresses.
func fetchFTP(ctx context.Context, rawURL, path string, allowPrivate bool) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "21"
	}
	dialer := &net.Dialer{Timeout: 20 * time.Second}
	if !allowPrivate {
		dialer.Control = publicOnly
	}
	ctrl, err := dialer.DialContext(ctx, "tcp4", net.JoinHostPort(host, port))
	if err != nil {
		return "", err
	}
	defer ctrl.Close()
	stop := context.AfterFunc(ctx, func() { ctrl.Close() })
	defer stop()
	ctrl.SetDeadline(time.Now().Add(30 * time.Minute))
	conn := textproto.NewConn(ctrl)

	if _, _, err := conn.ReadResponse(220); err != nil {
		return "", fmt.Errorf("ftp greeting: %w", err)
	}
	cmd := func(expect int, format string, args ...any) (string, error) {
		if _, err := conn.Cmd(format, args...); err != nil {
			return "", err
		}
		_, msg, err := conn.ReadResponse(expect)
		return msg, err
	}
	if _, err := conn.Cmd("USER anonymous"); err != nil {
		return "", err
	}
	code, _, err := conn.ReadResponse(0)
	if err != nil && code != 331 {
		return "", fmt.Errorf("ftp login: %w", err)
	}
	if code == 331 {
		if _, err := cmd(230, "PASS anonymous@"); err != nil {
			return "", fmt.Errorf("ftp login: %w", err)
		}
	}
	if _, err := cmd(200, "TYPE I"); err != nil {
		return "", err
	}

	file := u.Path
	var modified time.Time
	if msg, err := cmd(213, "MDTM %s", file); err == nil {
		modified, _ = time.Parse("20060102150405", strings.TrimSpace(msg))
	}
	if info, err := os.Stat(path); err == nil {
		current := !modified.IsZero() && info.ModTime().Equal(modified)
		if modified.IsZero() {
			// No usable MDTM: an unchanged size is the next best sign.
			if msg, err := cmd(213, "SIZE %s", file); err == nil {
				size, err := strconv.ParseInt(strings.TrimSpace(msg), 10, 64)
				current = err == nil && size == info.Size()
			}
		}
		if current {
			cmd(221, "QUIT")
			return statusCached, nil
		}
	}

	// EPSV gives only a port; the data connection goes to the same host,
	// never to an address the server names.
	msg, err := cmd(229, "EPSV")
	if err != nil {
		return "", fmt.Errorf("ftp EPSV: %w", err)
	}
	dataPort, err := epsvPort(msg)
	if err != nil {
		return "", err
	}
	remote, _, _ := net.SplitHostPort(ctrl.RemoteAddr().String())
	data, err := dialer.DialContext(ctx, "tcp4", net.JoinHostPort(remote, strconv.Itoa(dataPort)))
	if err != nil {
		return "", err
	}
	defer data.Close()
	stopData := context.AfterFunc(ctx, func() { data.Close() })
	defer stopData()
	data.SetDeadline(time.Now().Add(30 * time.Minute))
	if _, err := conn.Cmd("RETR %s", file); err != nil {
		return "", err
	}
	if _, _, err := conn.ReadResponse(1); err != nil { // 125 or 150
		return "", fmt.Errorf("ftp RETR: %w", err)
	}

	tmp := path + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	_, copyErr := io.Copy(out, bufio.NewReader(data))
	data.Close()
	if err := out.Close(); err != nil {
		return "", err
	}
	if copyErr != nil {
		return "", copyErr
	}
	if _, _, err := conn.ReadResponse(226); err != nil {
		return "", fmt.Errorf("ftp transfer: %w", err)
	}
	cmd(221, "QUIT")
	if !modified.IsZero() {
		if err := os.Chtimes(tmp, modified, modified); err != nil {
			return "", err
		}
	}
	return statusDownloaded, os.Rename(tmp, path)
}

// epsvPort reads the port from "Entering Extended Passive Mode (|||port|)".
func epsvPort(msg string) (int, error) {
	start, end := strings.Index(msg, "(|||"), strings.LastIndex(msg, "|)")
	if start < 0 || end <= start+4 {
		return 0, errors.New("ftp: bad EPSV reply")
	}
	port, err := strconv.Atoi(msg[start+4 : end])
	if err != nil || port <= 0 || port > 65535 {
		return 0, errors.New("ftp: bad EPSV port")
	}
	return port, nil
}
