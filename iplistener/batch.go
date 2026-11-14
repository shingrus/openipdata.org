package main

import (
	"context"
	"net/netip"
	"time"
)

// Writer stores a batch of visitor IPs. Its errors must not contain IPs or the DSN.
type Writer interface {
	Write(ctx context.Context, ips []netip.Addr) error
}

// Stats are counts only.
type Stats struct {
	Deduped, Sent, WriteErrors, Dropped int64
}

// Batcher dedups IPs and sends them in batches. Not safe for concurrent use:
// one goroutine calls Add, Tick and Flush.
type Batcher struct {
	Stats Stats

	w    Writer
	now  func() time.Time
	logf func(format string, args ...any)

	// Limits; fields so tests can shrink them.
	size      int           // flush at this many queued IPs
	chunk     int           // most IPs in one statement
	minGap    time.Duration // least time between two flushes
	maxWait   time.Duration // flush a non-empty batch this long after its first IP
	ttl       time.Duration // an IP sent within ttl is not queued again
	queueCap  int
	seenCap   int
	backMin   time.Duration
	backMax   time.Duration
	pruneEach time.Duration

	queue     []netip.Addr
	queued    map[netip.Addr]struct{}
	seen      map[netip.Addr]time.Time // when the IP was sent
	firstAt   time.Time                // when the oldest queued IP arrived
	lastFlush time.Time
	nextTry   time.Time // after a failure
	backoff   time.Duration
	lastPrune time.Time
}

func NewBatcher(w Writer, now func() time.Time, logf func(string, ...any)) *Batcher {
	return &Batcher{
		w: w, now: now, logf: logf,
		size: 100, chunk: 1000, minGap: 2 * time.Second, maxWait: 60 * time.Second, ttl: time.Hour,
		queueCap: 50000, seenCap: 200000, backMin: 2 * time.Second, backMax: 5 * time.Minute,
		pruneEach: time.Minute,
		queued:    map[netip.Addr]struct{}{}, seen: map[netip.Addr]time.Time{},
	}
}

// Add queues an IP unless it was sent within the last hour or is already queued.
func (b *Batcher) Add(ctx context.Context, ip netip.Addr) {
	now := b.now()
	if t, ok := b.seen[ip]; ok && now.Sub(t) < b.ttl {
		b.Stats.Deduped++
		return
	}
	if _, ok := b.queued[ip]; ok {
		b.Stats.Deduped++
		return
	}
	if len(b.queue) >= b.queueCap { // long outage: drop the oldest
		delete(b.queued, b.queue[0])
		b.queue = b.queue[1:]
		b.Stats.Dropped++
	}
	if len(b.queue) == 0 {
		b.firstAt = now
	}
	b.queue = append(b.queue, ip)
	b.queued[ip] = struct{}{}
	b.Tick(ctx)
}

// Tick flushes when due and prunes expired dedup entries; call it about twice a second.
func (b *Batcher) Tick(ctx context.Context) {
	now := b.now()
	if now.Sub(b.lastPrune) >= b.pruneEach {
		b.prune(now)
	}
	if len(b.queue) == 0 || now.Before(b.nextTry) || now.Sub(b.lastFlush) < b.minGap {
		return
	}
	if len(b.queue) >= b.size || now.Sub(b.firstAt) >= b.maxWait {
		b.flushOne(ctx)
	}
}

// Flush sends everything queued, ignoring the timing rules, until a write fails.
func (b *Batcher) Flush(ctx context.Context) {
	for len(b.queue) > 0 && b.flushOne(ctx) {
	}
}

func (b *Batcher) flushOne(ctx context.Context) bool {
	n := min(len(b.queue), b.chunk)
	batch := b.queue[:n]
	now := b.now()
	b.lastFlush = now
	if err := b.w.Write(ctx, batch); err != nil {
		b.Stats.WriteErrors++
		if b.backoff == 0 {
			b.backoff = b.backMin
		} else {
			b.backoff = min(b.backoff*2, b.backMax)
		}
		b.nextTry = now.Add(b.backoff)
		b.logf("write failed (%v); retrying in %s", err, b.backoff)
		return false
	}
	b.backoff, b.nextTry = 0, time.Time{}
	b.Stats.Sent += int64(n)
	if len(b.seen)+n > b.seenCap {
		b.prune(now)
		if len(b.seen)+n > b.seenCap {
			b.seen = map[netip.Addr]time.Time{} // still too big: forget, memory stays bounded
		}
	}
	for _, ip := range batch {
		b.seen[ip] = now
		delete(b.queued, ip)
	}
	b.queue = append([]netip.Addr(nil), b.queue[n:]...)
	b.firstAt = now
	return true
}

func (b *Batcher) prune(now time.Time) {
	b.lastPrune = now
	for ip, t := range b.seen {
		if now.Sub(t) >= b.ttl {
			delete(b.seen, ip)
		}
	}
}

// Pending is the number of queued IPs.
func (b *Batcher) Pending() int { return len(b.queue) }
