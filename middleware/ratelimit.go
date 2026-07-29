package middleware

import (
	"math"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/brickster241/PulseHTTP/httpcore"
)

// RateLimitConfig configures a per-client token bucket.
type RateLimitConfig struct {
	RequestsPerSec float64 // steady-state refill rate
	Burst          float64 // bucket capacity
	// Key extracts the client identity; defaults to the remote IP.
	Key func(*httpcore.Request) string
	// now is injectable for tests.
	now func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// RateLimiter is a lazy token bucket per key: no background refill goroutine,
// tokens are recomputed from elapsed time on each request. A janitor evicts
// buckets idle long enough to be full again, bounding memory under IP churn.
type RateLimiter struct {
	cfg     RateLimitConfig
	mu      sync.Mutex
	buckets map[string]*bucket
	stop    chan struct{}
}

func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	if cfg.Key == nil {
		cfg.Key = func(req *httpcore.Request) string {
			host, _, err := net.SplitHostPort(req.RemoteAddr)
			if err != nil {
				return req.RemoteAddr
			}
			return host
		}
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	rl := &RateLimiter{
		cfg:     cfg,
		buckets: make(map[string]*bucket),
		stop:    make(chan struct{}),
	}
	go rl.janitor()
	return rl
}

// Stop terminates the janitor (for tests and graceful shutdown).
func (rl *RateLimiter) Stop() { close(rl.stop) }

func (rl *RateLimiter) janitor() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-rl.stop:
			return
		case <-ticker.C:
			idle := time.Duration(rl.cfg.Burst/rl.cfg.RequestsPerSec)*time.Second + time.Minute
			cutoff := rl.cfg.now().Add(-idle)
			rl.mu.Lock()
			for k, b := range rl.buckets {
				if b.last.Before(cutoff) {
					delete(rl.buckets, k)
				}
			}
			rl.mu.Unlock()
		}
	}
}

// take attempts to consume one token; on refusal it returns the wait until a
// token is available.
func (rl *RateLimiter) take(key string) (allowed bool, remaining int, wait time.Duration) {
	now := rl.cfg.now()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: rl.cfg.Burst, last: now}
		rl.buckets[key] = b
	}
	b.tokens = math.Min(rl.cfg.Burst, b.tokens+now.Sub(b.last).Seconds()*rl.cfg.RequestsPerSec)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, int(b.tokens), 0
	}
	deficit := 1 - b.tokens
	return false, 0, time.Duration(deficit / rl.cfg.RequestsPerSec * float64(time.Second))
}

// Middleware answers 429 with Retry-After and X-RateLimit-* headers when the
// bucket is empty — a client can tell being throttled (429, slow down and
// retry) apart from being denied (403, retrying will never help).
func (rl *RateLimiter) Middleware() Middleware {
	limit := strconv.Itoa(int(rl.cfg.Burst))
	return func(next httpcore.Handler) httpcore.Handler {
		return func(req *httpcore.Request, w *httpcore.ResponseWriter) {
			allowed, remaining, wait := rl.take(rl.cfg.Key(req))
			w.Header().Set("X-RateLimit-Limit", limit)
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
			if !allowed {
				retryAfter := int(math.Ceil(wait.Seconds()))
				if retryAfter < 1 {
					retryAfter = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				w.Error(httpcore.StatusTooManyRequests, "rate limit exceeded; retry after "+strconv.Itoa(retryAfter)+"s")
				return
			}
			next(req, w)
		}
	}
}
