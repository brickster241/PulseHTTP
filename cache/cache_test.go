package cache

import (
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/brickster241/PulseHTTP/httpcore"
	"github.com/brickster241/PulseHTTP/middleware"
)

func makeReq(method, target string) *httpcore.Request {
	path, _, _ := strings3(target)
	return &httpcore.Request{
		Method: method, Path: path, RawTarget: target,
		Query: url.Values{}, Headers: httpcore.NewHeaders(),
		PathParams: map[string]string{}, Meta: map[string]any{},
		RemoteAddr: "10.0.0.1:1",
	}
}

func strings3(target string) (string, string, bool) {
	for i := 0; i < len(target); i++ {
		if target[i] == '?' {
			return target[:i], target[i+1:], true
		}
	}
	return target, "", false
}

// countingHandler serves an incrementing value so cache hits are detectable.
func countingHandler() (httpcore.Handler, *int) {
	calls := 0
	return func(req *httpcore.Request, w *httpcore.ResponseWriter) {
		calls++
		w.Header().Set("Content-Type", "text/plain")
		w.WriteString("v" + strconv.Itoa(calls))
	}, &calls
}

func TestMissThenHit(t *testing.T) {
	c := New(8, time.Minute)
	h, calls := countingHandler()
	wrapped := middleware.Chain(h, c.Middleware())

	w := httpcore.NewResponseWriter()
	wrapped(makeReq("GET", "/a"), w)
	if w.Header().Get("X-Cache") != "MISS" || string(w.Body()) != "v1" {
		t.Fatalf("first: %s %q", w.Header().Get("X-Cache"), w.Body())
	}
	w = httpcore.NewResponseWriter()
	wrapped(makeReq("GET", "/a"), w)
	if w.Header().Get("X-Cache") != "HIT" || string(w.Body()) != "v1" {
		t.Fatalf("second: %s %q", w.Header().Get("X-Cache"), w.Body())
	}
	if *calls != 1 {
		t.Fatalf("handler ran %d times, want 1", *calls)
	}
}

func TestIfNoneMatchGets304(t *testing.T) {
	c := New(8, time.Minute)
	h, _ := countingHandler()
	wrapped := middleware.Chain(h, c.Middleware())

	w := httpcore.NewResponseWriter()
	wrapped(makeReq("GET", "/a"), w)
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("cached 200 must carry an ETag")
	}

	req := makeReq("GET", "/a")
	req.Headers.Set("If-None-Match", etag)
	w = httpcore.NewResponseWriter()
	wrapped(req, w)
	if w.Status() != 304 {
		t.Fatalf("got %d, want 304", w.Status())
	}
}

func TestTTLExpiryRefetches(t *testing.T) {
	c := New(8, 30*time.Millisecond)
	h, calls := countingHandler()
	wrapped := middleware.Chain(h, c.Middleware())

	wrapped(makeReq("GET", "/a"), httpcore.NewResponseWriter())
	time.Sleep(60 * time.Millisecond)
	w := httpcore.NewResponseWriter()
	wrapped(makeReq("GET", "/a"), w)
	if w.Header().Get("X-Cache") != "MISS" || *calls != 2 {
		t.Fatalf("expired entry served: %s, calls=%d", w.Header().Get("X-Cache"), *calls)
	}
}

func TestLRUEviction(t *testing.T) {
	c := New(2, time.Minute)
	h, _ := countingHandler()
	wrapped := middleware.Chain(h, c.Middleware())

	wrapped(makeReq("GET", "/a"), httpcore.NewResponseWriter())
	wrapped(makeReq("GET", "/b"), httpcore.NewResponseWriter())
	wrapped(makeReq("GET", "/a"), httpcore.NewResponseWriter()) // /a now MRU
	wrapped(makeReq("GET", "/c"), httpcore.NewResponseWriter()) // evicts /b

	// Check the survivor FIRST: probing the victim would re-insert it and
	// evict the survivor we are about to assert on.
	w := httpcore.NewResponseWriter()
	wrapped(makeReq("GET", "/a"), w)
	if w.Header().Get("X-Cache") != "HIT" {
		t.Fatal("/a should have survived as MRU")
	}
	w = httpcore.NewResponseWriter()
	wrapped(makeReq("GET", "/b"), w)
	if w.Header().Get("X-Cache") != "MISS" {
		t.Fatal("/b should have been the LRU eviction victim")
	}
}

func TestWriteInvalidatesPath(t *testing.T) {
	c := New(8, time.Minute)
	h, calls := countingHandler()
	wrapped := middleware.Chain(h, c.Middleware())

	wrapped(makeReq("GET", "/doc"), httpcore.NewResponseWriter())
	wrapped(makeReq("POST", "/doc"), httpcore.NewResponseWriter())

	w := httpcore.NewResponseWriter()
	wrapped(makeReq("GET", "/doc"), w)
	if w.Header().Get("X-Cache") != "MISS" {
		t.Fatal("POST must invalidate the cached GET for the same path")
	}
	if *calls != 3 { // GET miss, POST, GET refetch
		t.Fatalf("calls = %d, want 3", *calls)
	}
}
