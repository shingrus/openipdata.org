// iplistener tails an nginx access log and records visitor IPs in public.visitor_ips.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	logger := log.New(os.Stdout, "", 0) // journald adds the timestamps
	code := run(logger)
	os.Exit(code)
}

func run(logger *log.Logger) int {
	dsn, logFile, keep, exclude, source := os.Getenv("PGSQL"), os.Getenv("LOG_FILE"),
		os.Getenv("FILTER"), os.Getenv("EXCLUDE"), os.Getenv("SOURCE")
	for name, v := range map[string]string{"PGSQL": dsn, "LOG_FILE": logFile, "FILTER": keep, "SOURCE": source} {
		if v == "" {
			logger.Printf("error: %s is required", name)
			return 1
		}
	}
	filter, err := NewFilter(keep, exclude)
	if err != nil {
		logger.Printf("error: FILTER or EXCLUDE is not a valid Go (RE2) regular expression")
		return 1
	}
	store, err := NewStore(dsn, source)
	if err != nil {
		logger.Printf("error: %v", err)
		return 1
	}
	defer store.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	logger.Printf("started: log file %s, source %s, filter %q, exclude %q", logFile, source, keep, exclude)

	lines := make(chan string, 256) // backpressure: the tailer waits; worst case 256 x 64 KiB
	tailer := &Tailer{Path: logFile, Poll: 500 * time.Millisecond, Grace: 30 * time.Second}
	done := make(chan struct{})
	go func() { tailer.Run(ctx, lines); close(done) }()

	batcher := NewBatcher(store, time.Now, logger.Printf)
	var c Counts
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	summary := time.NewTicker(10 * time.Minute)
	defer summary.Stop()
	for {
		select {
		case line := <-lines:
			c.Lines++
			switch ip, res := filter.Check(line); res {
			case NoMatch:
			case Excluded:
				c.Matched++
				c.Excluded++
			case BadIP:
				c.Matched++
				c.BadIP++
			case Kept:
				c.Matched++
				batcher.Add(ctx, ip)
			}
		case <-tick.C:
			batcher.Tick(ctx)
		case <-summary.C:
			logger.Print(summaryLine(c, batcher.Stats, tailer.Rotations.Load()))
		case <-ctx.Done():
			<-done
			fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			batcher.Flush(fctx)
			cancel()
			logger.Print("stopped: " + summaryLine(c, batcher.Stats, tailer.Rotations.Load()))
			return 0
		}
	}
}

// Counts are the filter counters of one run.
type Counts struct {
	Lines, Matched, Excluded, BadIP int64
}

// summaryLine has counts only, never an IP.
func summaryLine(c Counts, s Stats, rotations int64) string {
	return fmt.Sprintf("summary: lines %d, matched %d, excluded %d, bad_ip %d, deduped %d, sent %d, dropped %d, write_errors %d, rotations %d",
		c.Lines, c.Matched, c.Excluded, c.BadIP, s.Deduped, s.Sent, s.Dropped, s.WriteErrors, rotations)
}
