// Package metrics keeps lock-cheap request telemetry: totals by status
// class, an exact-count latency histogram, and interpolated percentiles,
// exposed as JSON for a /metrics route.
package metrics

import (
	"encoding/json"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brickster241/PulseHTTP/httpcore"
	"github.com/brickster241/PulseHTTP/middleware"
)

// bucketBoundsUS are histogram upper bounds in microseconds (last = +Inf).
var bucketBoundsUS = []int64{
	100, 250, 500, 1_000, 2_500, 5_000, 10_000,
	25_000, 50_000, 100_000, 250_000, 1_000_000,
}

type Registry struct {
	start    time.Time
	total    atomic.Uint64
	inflight atomic.Int64
	sumUS    atomic.Int64

	mu       sync.Mutex
	byStatus map[int]uint64
	buckets  []uint64 // len(bucketBoundsUS)+1, last is overflow
}

func NewRegistry() *Registry {
	return &Registry{
		start:    time.Now(),
		byStatus: make(map[int]uint64),
		buckets:  make([]uint64, len(bucketBoundsUS)+1),
	}
}

// Observe records one completed exchange.
func (r *Registry) Observe(status int, dur time.Duration) {
	r.total.Add(1)
	us := dur.Microseconds()
	r.sumUS.Add(us)
	idx := sort.Search(len(bucketBoundsUS), func(i int) bool { return us <= bucketBoundsUS[i] })
	r.mu.Lock()
	r.byStatus[status]++
	r.buckets[idx]++
	r.mu.Unlock()
}

// percentile linearly interpolates within the winning bucket. Exact enough
// for operational dashboards; the benchmark harness computes exact ranks.
func (r *Registry) percentile(p float64, buckets []uint64, total uint64) float64 {
	if total == 0 {
		return 0
	}
	rank := p * float64(total)
	var cum uint64
	for i, cnt := range buckets {
		prev := cum
		cum += cnt
		if float64(cum) >= rank {
			lo := int64(0)
			if i > 0 {
				lo = bucketBoundsUS[i-1]
			}
			hi := int64(2_000_000)
			if i < len(bucketBoundsUS) {
				hi = bucketBoundsUS[i]
			}
			if cnt == 0 {
				return float64(hi)
			}
			frac := (rank - float64(prev)) / float64(cnt)
			return float64(lo) + frac*float64(hi-lo)
		}
	}
	return float64(bucketBoundsUS[len(bucketBoundsUS)-1])
}

// Snapshot is the JSON shape served at /metrics.
type Snapshot struct {
	UptimeSec   float64          `json:"uptime_sec"`
	Total       uint64           `json:"requests_total"`
	InFlight    int64            `json:"requests_in_flight"`
	ByStatus    map[string]uint64 `json:"by_status"`
	AvgLatencyMS float64         `json:"latency_avg_ms"`
	P50MS       float64          `json:"latency_p50_ms"`
	P90MS       float64          `json:"latency_p90_ms"`
	P99MS       float64          `json:"latency_p99_ms"`
}

func (r *Registry) snapshot() Snapshot {
	total := r.total.Load()
	r.mu.Lock()
	byStatus := make(map[string]uint64, len(r.byStatus))
	for k, v := range r.byStatus {
		byStatus[itoa(k)] = v
	}
	buckets := append([]uint64(nil), r.buckets...)
	r.mu.Unlock()

	snap := Snapshot{
		UptimeSec: time.Since(r.start).Seconds(),
		Total:     total,
		InFlight:  r.inflight.Load(),
		ByStatus:  byStatus,
	}
	if total > 0 {
		snap.AvgLatencyMS = float64(r.sumUS.Load()) / float64(total) / 1000
		snap.P50MS = r.percentile(0.50, buckets, total) / 1000
		snap.P90MS = r.percentile(0.90, buckets, total) / 1000
		snap.P99MS = r.percentile(0.99, buckets, total) / 1000
	}
	return snap
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// Middleware measures every request, including ones that fail deeper in the
// chain — instrument at the edge, observe everything.
func (r *Registry) Middleware() middleware.Middleware {
	return func(next httpcore.Handler) httpcore.Handler {
		return func(req *httpcore.Request, w *httpcore.ResponseWriter) {
			r.inflight.Add(1)
			start := time.Now()
			next(req, w)
			r.inflight.Add(-1)
			r.Observe(w.Status(), time.Since(start))
		}
	}
}

// Handler serves the JSON snapshot.
func (r *Registry) Handler() httpcore.Handler {
	return func(req *httpcore.Request, w *httpcore.ResponseWriter) {
		payload, _ := json.MarshalIndent(r.snapshot(), "", "  ")
		w.JSON(httpcore.StatusOK, payload)
	}
}
