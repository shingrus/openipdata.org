package arin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"openipdata.org/hatchet/download"
)

// Stop rules of a run: a stop is not an error, the unfinished batch is not stored.
const (
	maxLimited     = 5               // answers of 429 in a row
	maxFailedRow   = 10              // failed lookups in a row
	maxFailedShare = 5               // a batch with more than one failed lookup in this many is not stored
	minFailedBatch = 5               // and only when at least this many failed (a small batch at the end of the walk)
	maxTries       = 3               // tries of one lookup that gets no answer
	maxBackoff     = 5 * time.Minute // longest wait after a 429
	stopRate       = "rate limited"
	stopFailing    = "failing"
	defaultPause   = 2 * time.Second
	defaultWait    = 15 * time.Second
	defaultRetryU  = time.Second
)

// errStopped ends a lookup when the run has been stopped (by a rule above, or by the budget).
var errStopped = errors.New("lookups stopped")

// asker sends the lookups of one run. Its rate limit and its stop counters are shared by the
// workers.
type asker struct {
	client   *http.Client
	base     string
	interval time.Duration
	pause    time.Duration
	backoff  time.Duration
	unit     time.Duration
	maxWait  time.Duration // the longest pause after a 429

	mu         sync.Mutex
	next       time.Time // the earliest start of the next request
	pausedTill time.Time // the end of the pause after a 429; no request starts before it
	limited    int       // pauses after a 429 in a row (answers of 429 during a pause do not count again)
	failedRow  int       // failed lookups in a row
	stop       string    // set when a stop rule fired
	requests   int
	retries    int
	rateLimits int
	cancel     context.CancelFunc // ends the batch that is running when a stop rule fires
}

// wait blocks until the next request may start: at most one start per interval over all workers,
// and none during the pause after a 429. A worker that already had its turn when a pause began
// takes a new turn after the pause.
func (a *asker) wait(ctx context.Context) error {
	for {
		a.mu.Lock()
		now := time.Now()
		at := a.next
		if at.Before(now) {
			at = now
		}
		if at.Before(a.pausedTill) {
			at = a.pausedTill
		}
		a.next = at.Add(a.interval)
		a.mu.Unlock()
		if err := sleep(ctx, time.Until(at)); err != nil {
			return err
		}
		a.mu.Lock()
		paused := time.Now().Before(a.pausedTill)
		if !paused {
			a.requests++
		}
		a.mu.Unlock()
		if !paused {
			return nil
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// stopRun records the first stop rule that fired and ends the running batch.
func (a *asker) stopRun(reason string) {
	a.mu.Lock()
	if a.stop == "" {
		a.stop = reason
	}
	cancel := a.cancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *asker) stopped() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stop
}

// answered resets the counter of failed lookups; failedLookup adds to it and may stop the run.
func (a *asker) lookupDone(failed bool) {
	a.mu.Lock()
	if !failed {
		a.failedRow = 0
		a.mu.Unlock()
		return
	}
	a.failedRow++
	stop := a.failedRow >= maxFailedRow
	a.mu.Unlock()
	if stop {
		a.stopRun(stopFailing)
	}
}

// lookup asks about one address. It returns the kind of the answer and, for kindFound and
// kindWide, the record. A cancelled ctx or a stopped run gives an error; an ARIN problem never
// does (it is kindFailed).
func (a *asker) lookup(ctx context.Context, addr netip.Addr) (record, string, error) {
	tries := 0
	for {
		if err := a.wait(ctx); err != nil {
			return record{}, "", err
		}
		if a.stopped() != "" {
			return record{}, "", errStopped
		}
		status, header, body, err := a.get(ctx, addr)
		if ctx.Err() != nil {
			return record{}, "", ctx.Err()
		}
		switch {
		case err == nil && status == http.StatusTooManyRequests:
			// Every worker pauses, not only this one: the pause moves the start of the next
			// request. The requests that were on their way when the pause began can answer 429
			// too; they belong to the same pause and are not counted as another one.
			now := time.Now()
			a.mu.Lock()
			a.rateLimits++
			stop := false
			if !now.Before(a.pausedTill) {
				a.limited++
				if stop = a.limited >= maxLimited; !stop {
					wait := a.backoff << (a.limited - 1)
					if s, perr := strconv.Atoi(header.Get("Retry-After")); perr == nil && s > 0 {
						wait = a.maxWait // also for a value too large to multiply
						if int64(s) < int64(a.maxWait/a.unit) {
							wait = time.Duration(s) * a.unit
						}
					}
					a.pausedTill = now.Add(min(wait, a.maxWait))
					if a.next.Before(a.pausedTill) {
						a.next = a.pausedTill
					}
				}
			}
			a.mu.Unlock()
			if stop {
				a.stopRun(stopRate)
				return record{}, "", errStopped
			}
			continue // ask the same address again, after the pause
		case err == nil:
			a.mu.Lock()
			if !time.Now().Before(a.pausedTill) {
				a.limited = 0
			}
			a.mu.Unlock()
		}
		if err == nil {
			switch {
			case status >= 300 && status < 400:
				a.lookupDone(false)
				return record{}, kindNotARIN, nil
			case status == http.StatusNotFound:
				a.lookupDone(false)
				return record{}, kindNotFound, nil
			case status == http.StatusOK:
				if body == nil { // longer than the cap
					a.lookupDone(false)
					return record{}, kindBad, nil
				}
				rec, kind := parseAnswer(body, addr)
				a.lookupDone(false)
				return rec, kind, nil
			}
		}
		// Another status or a network error: a few more tries, then the lookup is failed.
		tries++
		if tries >= maxTries {
			a.lookupDone(true)
			return record{}, kindFailed, nil
		}
		a.mu.Lock()
		a.retries++
		a.mu.Unlock()
		if err := sleep(ctx, a.pause); err != nil {
			return record{}, "", err
		}
	}
}

// get sends one request. The body is nil when it is longer than maxBody. The error is never
// shown or logged: it can quote the URL, which holds the address.
func (a *asker) get(ctx context.Context, addr netip.Addr) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/ip/"+addr.String(), nil)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("User-Agent", download.UserAgent)
	req.Header.Set("Accept", "application/rdap+json")
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, resp.Header, nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return 0, nil, nil, err
	}
	if len(body) > maxBody {
		body = nil
	}
	return resp.StatusCode, resp.Header, body, nil
}
