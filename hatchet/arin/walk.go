package arin

import (
	"context"
	"sort"
	"sync"
	"time"

	"openipdata.org/hatchet/ipdata"
	"openipdata.org/hatchet/outputs"
	"openipdata.org/hatchet/rdns"
)

// answer is the result of one lookup of a batch.
type answer struct {
	rec  record
	kind string
}

// walkAndLook is the loop of Lookup: batch after batch from cursor until the cycle ends, the
// soft budget is used or a stop rule fires. Only a file error or a cancellation is returned.
func (p *Pipeline) walkAndLook(ctx context.Context, st lookupStore, gap rdns.GapList, ranges arinRanges,
	cursor int, started time.Time, size int, deadline time.Time, out *LookupOutput) error {
	skipHolders := out.HoldersRestore == holdersFailed
	hset := holderSet{}
	if !skipHolders {
		hset = loadHolderSet(p.dir())
	}
	interval, batch, workers, maxSteps := p.interval, p.batch, p.workers, p.maxSteps
	if interval == 0 {
		interval = lookupInterval
	}
	if batch == 0 {
		batch = lookupBatch
	}
	if workers == 0 {
		workers = lookupWorkers
	}
	if maxSteps == 0 {
		maxSteps = maxBatchSteps
	}
	at := p.at
	if at == nil {
		at = rdns.WalkAt
	}
	public := p.public
	if public == nil {
		public = ipdata.PublicV4
	}
	base := p.base
	if base == "" {
		base = rdapBase
	}
	a := &asker{client: p.lookupClient(), base: base, interval: interval, pause: p.retryPause, backoff: p.backoff, unit: p.retryUnit}
	if a.pause == 0 {
		a.pause = defaultPause
	}
	if a.backoff == 0 {
		a.backoff = defaultWait
	}
	if a.unit == 0 {
		a.unit = defaultRetryU
	}
	if a.maxWait = p.maxWait; a.maxWait == 0 {
		a.maxWait = maxBackoff
	}
	rctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	perLookup := interval // how long one lookup of a batch takes: the interval, or longer when ARIN answers slowly
	pace := outputs.Pace{Every: outputs.ProgressEvery}
	p.log("lookup: %d gap /24s, %d ARIN ranges; walk at position %d of %d", gap.Total(), len(ranges), cursor, size)

	defer func() { // the counters of the asker, also when the run stops early
		a.mu.Lock()
		out.Requests, out.Retries, out.RateLimited = a.requests, a.retries, a.rateLimits
		a.mu.Unlock()
		if out.Stopped == stopFailing || out.Stopped == stopRate {
			// The same stop run after run means the walk is not moving: this line is the only sign.
			p.log("lookup: WARNING stopped (%s) at position %d; the batch is asked again by the next run", out.Stopped, cursor)
		}
		p.log("lookup: %d positions walked, %d targets, %d requests: %d found (%d with a city), %d wide, %d not ARIN, %d not found, %d bad, %d failed, %d rate limited; %d records stored in %d batches, %d holders; cycle done=%v; stopped=%q",
			out.Steps, out.Targets, out.Requests, out.Found, out.WithCity, out.Wide, out.NotARIN, out.NotFound, out.Bad, out.Failed, out.RateLimited, out.Stored, out.Batches, out.HoldersStored, out.CycleDone, out.Stopped)
	}()

	for cursor < size {
		if rctx.Err() != nil {
			break
		}
		picked, end := walk(gap, ranges, at, public, size, cursor, batch, maxSteps)
		var recs, holders []record
		var kinds map[string]int
		now := p.clock().UTC()
		if len(picked) > 0 {
			// A batch that cannot finish inside the budget is not started: its lookups would be
			// thrown away and asked again by the next run.
			if time.Until(deadline) < time.Duration(len(picked)+batchSlack)*perLookup {
				out.Stopped = "time"
				return nil
			}
			batchStart := time.Now()
			answers, stop := p.lookBatch(rctx, a, picked, workers)
			if err := ctx.Err(); err != nil {
				return err
			}
			if stop == "" && rctx.Err() != nil {
				stop = "time"
			}
			if stop != "" { // the unfinished batch is not stored
				out.Stopped = stop
				return nil
			}
			perLookup = max(interval, time.Since(batchStart)/time.Duration(len(picked)))
			kinds = map[string]int{}
			seen := map[[2]uint32]int{}
			for _, an := range answers {
				kinds[an.kind]++
				if an.kind != kindFound {
					continue
				}
				k := [2]uint32{an.rec.Start, an.rec.End}
				if i, ok := seen[k]; ok {
					recs[i] = an.rec
					continue
				}
				seen[k] = len(recs)
				recs = append(recs, an.rec)
			}
			holders = holdersWanted(answers, hset)
			if skipHolders && len(holders) > 0 {
				// R2 may hold a copy of the file that could not be fetched: nothing is appended (a new
				// file would hide that copy), the next run restores first.
				out.HoldersSkipped += len(holders)
				holders = nil
			}
			// The position moves past the /24s of a stored batch, and they are not asked again
			// before the next cycle. A batch with many failed lookups is therefore not stored: the
			// next run asks it again.
			// Not the last batch of the walk: if its few addresses failed every time, the walk
			// would stay at that position for good and the cycle would never end.
			if end < size && kinds[kindFailed] >= minFailedBatch && kinds[kindFailed]*maxFailedShare > len(picked) {
				out.Stopped = stopFailing
				return nil
			}
			sort.Slice(recs, func(i, j int) bool {
				if recs[i].Start != recs[j].Start {
					return recs[i].Start < recs[j].Start
				}
				return recs[i].End < recs[j].End
			})
		}
		// The holders first, synced, before the journal and the position: a crash can only make the
		// next run ask the batch again and append the same rows again (compaction merges them).
		if err := appendHolders(p.dir(), holders, now); err != nil {
			return err
		}
		hset.note(holders)
		if err := st.Save(ctx, recs, end, started, now); err != nil {
			return err
		}
		out.Batches++
		out.Steps += end - cursor
		out.Targets += len(picked)
		out.Stored += len(recs)
		for _, h := range holders {
			out.HoldersStored++
			switch h.Class {
			case classEdu:
				out.HoldersEdu++
			case classGov:
				out.HoldersGov++
			case classMil:
				out.HoldersMil++
			default:
				out.HoldersOther++
			}
		}
		out.Found += kinds[kindFound]
		out.Wide += kinds[kindWide]
		out.NotARIN += kinds[kindNotARIN]
		out.NotFound += kinds[kindNotFound]
		out.Bad += kinds[kindBad]
		out.Failed += kinds[kindFailed]
		for _, r := range recs {
			if r.City != "" {
				out.WithCity++
			}
		}
		cursor = end
		if pace.Due() {
			p.log("lookup: position %d of %d; %d lookups done: %d found, %d wide, %d not found, %d failed", cursor, size,
				out.Targets, out.Found, out.Wide, out.NotFound, out.Failed)
		}
	}
	out.CycleDone = cursor >= size
	if !out.CycleDone {
		out.Stopped = "time" // the budget ran out between two batches
	}
	return nil
}

// lookBatch looks the targets up with workers sharing the rate limit of a. stop is the reason
// the run must end ("rate limited", "failing") or empty; a cancelled ctx (the budget or the run)
// also gives no answers, the caller finds out from its contexts.
func (p *Pipeline) lookBatch(ctx context.Context, a *asker, picked []target, workers int) (answers []answer, stop string) {
	bctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.mu.Lock()
	a.cancel = cancel
	a.mu.Unlock()
	answers = make([]answer, len(picked))
	var wg sync.WaitGroup
	var bad bool
	var mu sync.Mutex
	jobs := make(chan int)
	for w := 0; w < min(workers, len(picked)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				rec, kind, err := a.lookup(bctx, picked[i].Addr)
				if err != nil {
					mu.Lock()
					bad = true
					mu.Unlock()
					cancel()
					return
				}
				answers[i] = answer{rec, kind}
			}
		}()
	}
feed:
	for i := range picked {
		select {
		case jobs <- i:
		case <-bctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	a.mu.Lock()
	a.cancel = nil
	a.mu.Unlock()
	if s := a.stopped(); s != "" {
		return nil, s
	}
	if bad || bctx.Err() != nil {
		return nil, "time" // the caller replaces this when it was the run's context
	}
	return answers, ""
}
