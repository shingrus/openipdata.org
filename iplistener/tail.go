package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync/atomic"
	"time"
)

const maxLine = 64 << 10

// Tailer follows a log file through rotation (rename + create) and truncation.
// It starts at the end of the file, so nothing is replayed.
type Tailer struct {
	Path      string
	Poll      time.Duration
	Grace     time.Duration // how long a renamed file is still read
	Rotations atomic.Int64

	buf []byte
}

// lineReader cuts a byte stream into complete lines; a partial last line waits for its newline.
type lineReader struct {
	pending  []byte
	skipping bool // inside a line longer than maxLine
}

func (r *lineReader) feed(data []byte, emit func(string) bool) bool {
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			if !r.skipping {
				if len(r.pending)+len(data) > maxLine {
					r.skipping = true
					r.pending = r.pending[:0]
				} else {
					r.pending = append(r.pending, data...)
				}
			}
			return true
		}
		if r.skipping {
			r.skipping = false
		} else if len(r.pending)+i <= maxLine {
			line := string(append(r.pending, data[:i]...))
			if n := len(line); n > 0 && line[n-1] == '\r' {
				line = line[:n-1]
			}
			if !emit(line) {
				return false
			}
		}
		r.pending = r.pending[:0]
		data = data[i+1:]
	}
	return true
}

// Run sends every complete new line to out until ctx is done.
func (t *Tailer) Run(ctx context.Context, out chan<- string) {
	var (
		f, old   *os.File // old: the renamed file, still read during the grace period
		off      int64
		r, oldR  lineReader
		oldUntil time.Time
	)
	if t.buf == nil {
		t.buf = make([]byte, 32<<10)
	}
	grace := t.Grace
	defer func() {
		if f != nil {
			f.Close()
		}
		if old != nil {
			old.Close()
		}
	}()
	emit := func(s string) bool {
		select {
		case out <- s:
			return true
		case <-ctx.Done():
			return false
		}
	}
	open := func(seekEnd bool) {
		nf, err := os.Open(t.Path)
		if err != nil {
			return // missing between rotate and create: retry on the next poll
		}
		off = 0
		if seekEnd {
			if off, err = nf.Seek(0, io.SeekEnd); err != nil {
				nf.Close()
				return
			}
		}
		f, r = nf, lineReader{}
	}
	// drain reads file to EOF, adding the bytes read to *n (nil: not counted).
	drain := func(file *os.File, lr *lineReader, n *int64) bool {
		for {
			k, err := file.Read(t.buf)
			if n != nil {
				*n += int64(k)
			}
			if k > 0 && !lr.feed(t.buf[:k], emit) {
				return false
			}
			if err != nil || k == 0 {
				return true
			}
		}
	}

	open(true)
	for {
		if f == nil {
			open(false)
		}
		if old != nil {
			if !drain(old, &oldR, nil) {
				return
			}
			if time.Now().After(oldUntil) {
				old.Close()
				old = nil
			}
		}
		if f != nil {
			if !drain(f, &r, &off) {
				return
			}
			if pi, err := os.Stat(t.Path); err == nil {
				if fi, err := f.Stat(); err == nil && !os.SameFile(fi, pi) {
					// nginx keeps writing to the renamed file until it reopens its logs: keep reading it.
					if old != nil {
						drain(old, &oldR, nil)
						old.Close()
					}
					old, oldR, oldUntil = f, r, time.Now().Add(grace)
					f = nil
					t.Rotations.Add(1)
					continue // open the new file at once
				} else if err == nil && pi.Size() < off {
					if _, err := f.Seek(0, io.SeekStart); err == nil {
						off, r = 0, lineReader{}
						t.Rotations.Add(1)
						continue
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(t.Poll):
		}
	}
}
