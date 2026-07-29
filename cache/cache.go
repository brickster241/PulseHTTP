// Package cache is an in-process LRU response cache with TTL freshness and
// ETag revalidation: fresh hits skip the handler entirely, and clients that
// present a matching If-None-Match get a bodyless 304.
package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brickster241/PulseHTTP/httpcore"
	"github.com/brickster241/PulseHTTP/middleware"
)

type entry struct {
	key         string
	body        []byte
	contentType string
	etag        string
	expires     time.Time
}

type Cache struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	order    *list.List               // front = most recently used
	items    map[string]*list.Element // key -> element (entry)

	hits, misses uint64
}

func New(capacity int, ttl time.Duration) *Cache {
	return &Cache{
		capacity: capacity,
		ttl:      ttl,
		order:    list.New(),
		items:    make(map[string]*list.Element),
	}
}

func (c *Cache) get(key string, now time.Time) (*entry, bool) {
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*entry)
	if now.After(e.expires) {
		c.order.Remove(el)
		delete(c.items, key)
		return nil, false
	}
	c.order.MoveToFront(el)
	return e, true
}

func (c *Cache) put(e *entry) {
	if el, ok := c.items[e.key]; ok {
		c.order.Remove(el)
		delete(c.items, e.key)
	}
	c.items[e.key] = c.order.PushFront(e)
	for c.order.Len() > c.capacity {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*entry).key)
	}
}

func (c *Cache) invalidate(key string) {
	if el, ok := c.items[key]; ok {
		c.order.Remove(el)
		delete(c.items, key)
	}
}

// Stats returns hits, misses, and current size.
func (c *Cache) Stats() (hits, misses uint64, size int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, c.order.Len()
}

func etagFor(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// Middleware caches successful GET responses keyed by the raw target.
// Mutating verbs on a path invalidate that path's GET entry, so a stale
// read never survives a write the server itself observed.
func (c *Cache) Middleware() middleware.Middleware {
	return func(next httpcore.Handler) httpcore.Handler {
		return func(req *httpcore.Request, w *httpcore.ResponseWriter) {
			key := req.RawTarget
			if req.Method != "GET" {
				next(req, w)
				if req.Method == "POST" || req.Method == "PUT" ||
					req.Method == "DELETE" || req.Method == "PATCH" {
					c.mu.Lock()
					c.invalidate(req.Path) // query-less form
					c.invalidate(key)
					c.mu.Unlock()
				}
				return
			}

			// Cache-Control: no-cache on the REQUEST forces revalidation —
			// skip the stored copy and let the handler produce a fresh one.
			bypass := req.Headers.ContainsToken("Cache-Control", "no-cache")

			now := time.Now()
			var e *entry
			ok := false
			if !bypass {
				c.mu.Lock()
				e, ok = c.get(key, now)
				if ok {
					c.hits++
				} else {
					c.misses++
				}
				c.mu.Unlock()
			}

			if ok {
				w.Header().Set("ETag", e.etag)
				w.Header().Set("X-Cache", "HIT")
				if req.Headers.Get("If-None-Match") == e.etag {
					w.WriteHeader(httpcore.StatusNotModified)
					w.Hijack()
					return
				}
				if e.contentType != "" {
					w.Header().Set("Content-Type", e.contentType)
				}
				w.WriteHeader(httpcore.StatusOK)
				w.SetBody(e.body)
				w.Hijack()
				return
			}

			next(req, w)

			// Cache-Control on the RESPONSE governs storage: no-store is an
			// absolute opt-out, max-age=N overrides the default TTL.
			respCC := w.Header().Get("Cache-Control")
			if strings.Contains(respCC, "no-store") {
				return
			}
			ttl := c.ttl
			if idx := strings.Index(respCC, "max-age="); idx != -1 {
				if secs, err := strconv.Atoi(strings.TrimRight(respCC[idx+8:], " ,;")); err == nil && secs >= 0 {
					ttl = time.Duration(secs) * time.Second
				}
			}

			if w.Status() == httpcore.StatusOK {
				body := append([]byte(nil), w.Body()...)
				etag := etagFor(body)
				c.mu.Lock()
				c.put(&entry{
					key:         key,
					body:        body,
					contentType: w.Header().Get("Content-Type"),
					etag:        etag,
					expires:     now.Add(ttl),
				})
				c.mu.Unlock()
				w.Header().Set("ETag", etag)
				w.Header().Set("X-Cache", "MISS")
				// A brand-new response can still revalidate an up-to-date client.
				if req.Headers.Get("If-None-Match") == etag {
					w.WriteHeader(httpcore.StatusNotModified)
					w.SetBody(nil)
				}
			}
		}
	}
}
