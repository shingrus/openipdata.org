// openipdata-beacon turns DNS queries into a resolver address in Redis. Knot DNS serves the
// zone r.openipdata.org and sends every query through dnstap to a unix socket; for a query
// <token>.<zone> this program stores the address of the resolver that asked under beacon:<token>.
// The website reads that key. Nothing else is stored; no address, token or name is ever logged.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	dnstap "github.com/dnstap/golang-dnstap"
	"github.com/redis/go-redis/v9"
)

// version is set at build time: -ldflags "-X main.version=..."
var version = "dev"

type job struct{ token, resolver string }

// Counts of what was dropped; only these numbers are ever logged.
var queued, written, dropFull, dropRedis atomic.Int64

// Writes the address only if the key is new (first query wins) and sets the lifetime only then,
// so repeated queries cannot extend it. One round trip, atomic.
var setScript = redis.NewScript(`
if redis.call('HSETNX', KEYS[1], 'resolver_ip', ARGV[1]) == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[2])
end
return 1`)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	logger := log.New(os.Stdout, "", 0) // journald adds the timestamps
	os.Exit(run(logger))
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func run(logger *log.Logger) int {
	socket := env("DNSTAP_SOCKET", "/run/openipdata-beacon/dnstap.sock")
	zone := strings.TrimSuffix(strings.ToLower(env("BEACON_ZONE", "r.openipdata.org")), ".")
	redisAddr := env("REDIS_ADDR", "127.0.0.1:6379")
	ttl, err := strconv.Atoi(env("BEACON_TTL", "120"))
	if err != nil || ttl < 1 || ttl > 86400 {
		logger.Printf("error: BEACON_TTL must be 1 to 86400 seconds")
		return 1
	}
	if zone == "" {
		logger.Printf("error: BEACON_ZONE is empty")
		return 1
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:        redisAddr,
		DialTimeout: 300 * time.Millisecond,
		ReadTimeout: 300 * time.Millisecond,
		MaxRetries:  -1, // a failed write is dropped, never retried
	})
	defer rdb.Close()

	// A stale socket of a crashed run would make listening fail.
	_ = os.Remove(socket)
	input, err := dnstap.NewFrameStreamSockInputFromPath(socket)
	if err != nil {
		logger.Printf("error: cannot listen on %s", socket)
		return 1
	}
	// Knot runs as another user (the unit gives the socket directory to its group).
	if err := os.Chmod(socket, 0o660); err != nil {
		logger.Printf("error: cannot set the mode of %s", socket)
		return 1
	}
	defer os.Remove(socket)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	frames := make(chan []byte, 1024)
	go input.ReadInto(frames)

	jobs := make(chan job, 1024)
	var wg sync.WaitGroup
	var lastErr atomic.Int64 // unix time of the last logged Redis error
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				cctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
				err := setScript.Run(cctx, rdb, []string{"beacon:" + j.token}, j.resolver, ttl).Err()
				cancel()
				if err != nil {
					dropRedis.Add(1)
					// At most one line a minute, with no text of the error (it could hold a key).
					now := time.Now().Unix()
					if last := lastErr.Load(); now-last >= 60 && lastErr.CompareAndSwap(last, now) {
						logger.Printf("warning: Redis write failed (counted, dropped)")
					}
					continue
				}
				written.Add(1)
			}
		}()
	}

	logger.Printf("started: version %s, socket %s, zone %s, redis %s, ttl %ds", version, socket, zone, redisAddr, ttl)
	summary := time.NewTicker(10 * time.Minute)
	defer summary.Stop()
	var lastWritten, lastFull, lastRedis int64
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case frame := <-frames:
			token, resolver, ok := parseFrame(frame, zone)
			if !ok {
				continue
			}
			queued.Add(1)
			select {
			case jobs <- job{token, resolver}:
			default:
				dropFull.Add(1)
			}
		case <-summary.C:
			w, f, r := written.Load(), dropFull.Load(), dropRedis.Load()
			logger.Printf("summary: written %d, dropped queue-full %d, dropped redis-error %d (last 10 minutes)",
				w-lastWritten, f-lastFull, r-lastRedis)
			lastWritten, lastFull, lastRedis = w, f, r
		}
	}
	close(jobs)
	wg.Wait()
	logger.Printf("stopped")
	return 0
}
