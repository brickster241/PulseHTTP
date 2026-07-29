// Package middleware provides the composable request-processing chain:
// structured logging, panic isolation, request IDs, bearer auth (401 vs 403),
// and token-bucket rate limiting (429 + Retry-After).
package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/brickster241/PulseHTTP/httpcore"
)

// Middleware wraps a handler with additional behavior.
type Middleware func(httpcore.Handler) httpcore.Handler

// Chain applies middlewares so the first listed is the outermost.
func Chain(h httpcore.Handler, mws ...Middleware) httpcore.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// Recover converts handler panics into 500s so one bad route can't take the
// process down. The server has a second recover as a last line of defense.
func Recover() Middleware {
	return func(next httpcore.Handler) httpcore.Handler {
		return func(req *httpcore.Request, w *httpcore.ResponseWriter) {
			defer func() {
				if r := recover(); r != nil {
					w.Error(httpcore.StatusInternalServerError, "internal error")
				}
			}()
			next(req, w)
		}
	}
}

// RequestID stamps every exchange with a 12-hex-char id, echoed in the
// X-Request-ID response header and available to logs — the correlation
// handle you grep for when a customer pastes an error.
func RequestID() Middleware {
	return func(next httpcore.Handler) httpcore.Handler {
		return func(req *httpcore.Request, w *httpcore.ResponseWriter) {
			id := req.Headers.Get("X-Request-ID")
			if id == "" {
				var b [6]byte
				rand.Read(b[:])
				id = hex.EncodeToString(b[:])
			}
			req.Meta["request_id"] = id
			w.Header().Set("X-Request-ID", id)
			next(req, w)
		}
	}
}

// accessRecord is one structured access-log line.
type accessRecord struct {
	Time      string `json:"ts"`
	RequestID string `json:"request_id,omitempty"`
	Remote    string `json:"remote"`
	Method    string `json:"method"`
	Target    string `json:"target"`
	Status    int    `json:"status"`
	Bytes     int    `json:"bytes"`
	DurMicros int64  `json:"dur_us"`
	UserAgent string `json:"ua,omitempty"`
}

// Logging emits one JSON line per request to out (safe for concurrent use).
func Logging(out io.Writer) Middleware {
	var mu sync.Mutex
	return func(next httpcore.Handler) httpcore.Handler {
		return func(req *httpcore.Request, w *httpcore.ResponseWriter) {
			start := time.Now()
			next(req, w)
			rec := accessRecord{
				Time:      start.UTC().Format(time.RFC3339Nano),
				Remote:    req.RemoteAddr,
				Method:    req.Method,
				Target:    req.RawTarget,
				Status:    w.Status(),
				Bytes:     w.BodyLen(),
				DurMicros: time.Since(start).Microseconds(),
				UserAgent: req.Headers.Get("User-Agent"),
			}
			if id, ok := req.Meta["request_id"].(string); ok {
				rec.RequestID = id
			}
			line, _ := json.Marshal(rec)
			mu.Lock()
			out.Write(append(line, '\n'))
			mu.Unlock()
		}
	}
}
