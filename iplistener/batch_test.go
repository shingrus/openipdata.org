package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type fakeWriter struct {
	batches [][]netip.Addr
	err     error
}

func (w *fakeWriter) Write(_ context.Context, ips []netip.Addr) error {
	if w.err != nil {
		return w.err
	}
	w.batches = append(w.batches, append([]netip.Addr(nil), ips...))
	return nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func ipn(i int) netip.Addr           { return netip.AddrFrom4([4]byte{8, byte(i >> 16), byte(i >> 8), byte(i)}) }
func newTest() (*Batcher, *fakeWriter, *clock, *strings.Builder) {
	w, c, logs := &fakeWriter{}, &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, &strings.Builder{}
	b := NewBatcher(w, c.now, func(f string, a ...any) { fmt.Fprintf(logs, f+"\n", a...) })
	return b, w, c, logs
}

var ctx = context.Background()

func TestFlushAt100(t *testing.T) {
	b, w, _, _ := newTest()
	for i := 0; i < 99; i++ {
		b.Add(ctx, ipn(i))
	}
	if len(w.batches) != 0 {
		t.Fatal("flushed early")
	}
	b.Add(ctx, ipn(99))
	if len(w.batches) != 1 || len(w.batches[0]) != 100 || b.Stats.Sent != 100 {
		t.Fatalf("batches %v sent %d", len(w.batches), b.Stats.Sent)
	}
}

func TestMinGap(t *testing.T) {
	b, w, c, _ := newTest()
	for i := 0; i < 100; i++ {
		b.Add(ctx, ipn(i))
	}
	for i := 100; i < 200; i++ {
		b.Add(ctx, ipn(i))
	}
	if len(w.batches) != 1 {
		t.Fatal("second flush inside 2 s")
	}
	c.add(1999 * time.Millisecond)
	b.Tick(ctx)
	if len(w.batches) != 1 {
		t.Fatal("flushed at 1.999 s")
	}
	c.add(time.Millisecond)
	b.Tick(ctx)
	if len(w.batches) != 2 || len(w.batches[1]) != 100 {
		t.Fatal("no flush after 2 s")
	}
}

func TestIdleFlush(t *testing.T) {
	b, w, c, _ := newTest()
	b.Add(ctx, ipn(1))
	c.add(59 * time.Second)
	b.Tick(ctx)
	if len(w.batches) != 0 {
		t.Fatal("flushed before 60 s")
	}
	c.add(time.Second)
	b.Tick(ctx)
	if len(w.batches) != 1 || b.Pending() != 0 {
		t.Fatal("no idle flush")
	}
}

func TestDedup(t *testing.T) {
	b, w, c, _ := newTest()
	b.Add(ctx, ipn(1))
	b.Add(ctx, ipn(1)) // queued twice in one batch
	b.Flush(ctx)
	c.add(59 * time.Minute)
	b.Add(ctx, ipn(1)) // sent within the hour
	if b.Pending() != 0 || b.Stats.Deduped != 2 {
		t.Fatalf("pending %d deduped %d", b.Pending(), b.Stats.Deduped)
	}
	c.add(2 * time.Minute)
	b.Add(ctx, ipn(1))
	b.Flush(ctx)
	if len(w.batches) != 2 {
		t.Fatalf("batches %d", len(w.batches))
	}
}

func TestSeenStaysBounded(t *testing.T) {
	b, _, _, _ := newTest()
	b.seenCap, b.chunk = 150, 100
	for i := 0; i < 1000; i++ {
		b.Add(ctx, ipn(i))
	}
	b.Flush(ctx)
	if len(b.seen) > 150 {
		t.Fatalf("seen %d", len(b.seen))
	}
}

func TestRetryKeepsBatch(t *testing.T) {
	b, w, c, logs := newTest()
	w.err = errors.New("write failed, postgres code 08006")
	for i := 0; i < 100; i++ {
		b.Add(ctx, ipn(i))
	}
	if b.Pending() != 100 || b.Stats.WriteErrors != 1 {
		t.Fatalf("pending %d errors %d", b.Pending(), b.Stats.WriteErrors)
	}
	// Backoff 2 s, then 4 s, doubling up to 5 min.
	want := []time.Duration{4, 8, 16, 32, 64, 128, 256, 300, 300}
	for _, d := range want {
		c.add(b.backoff - time.Millisecond)
		b.Tick(ctx)
		errs := b.Stats.WriteErrors
		c.add(time.Millisecond)
		b.Tick(ctx)
		if b.Stats.WriteErrors != errs+1 || b.backoff != d*time.Second {
			t.Fatalf("backoff %v, want %v", b.backoff, d*time.Second)
		}
	}
	w.err = nil
	c.add(b.backoff)
	b.Tick(ctx)
	if len(w.batches) != 1 || len(w.batches[0]) != 100 || b.Pending() != 0 || b.backoff != 0 {
		t.Fatal("batch not delivered after recovery")
	}
	if !strings.Contains(logs.String(), "postgres code 08006") {
		t.Error("error not logged")
	}
}

func TestQueueCapDropsOldest(t *testing.T) {
	b, w, _, _ := newTest()
	w.err = errors.New("down")
	b.queueCap = 150
	for i := 0; i < 200; i++ {
		b.Add(ctx, ipn(i))
	}
	if b.Pending() != 150 || b.Stats.Dropped != 50 {
		t.Fatalf("pending %d dropped %d", b.Pending(), b.Stats.Dropped)
	}
	if b.queue[0] != ipn(50) {
		t.Errorf("oldest = %v", b.queue[0])
	}
	if _, ok := b.queued[ipn(0)]; ok {
		t.Error("dropped IP still in the queued set")
	}
}

func TestShutdownFlush(t *testing.T) {
	b, w, _, _ := newTest()
	for i := 0; i < 5; i++ {
		b.Add(ctx, ipn(i))
	}
	b.Flush(ctx)
	if len(w.batches) != 1 || len(w.batches[0]) != 5 {
		t.Fatal("shutdown flush missed")
	}
}

func TestFlushChunks(t *testing.T) {
	b, w, _, _ := newTest()
	w.err = errors.New("down")
	for i := 0; i < 2500; i++ {
		b.Add(ctx, ipn(i))
	}
	w.err = nil
	b.Flush(ctx)
	if len(w.batches) != 3 || len(w.batches[0]) != 1000 || b.Pending() != 0 {
		t.Fatalf("batches %d", len(w.batches))
	}
}
