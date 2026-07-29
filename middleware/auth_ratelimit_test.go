package middleware

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/brickster241/PulseHTTP/httpcore"
)

func makeReq(method, path string) *httpcore.Request {
	return &httpcore.Request{
		Method: method, Path: path, RawTarget: path,
		Query: url.Values{}, Headers: httpcore.NewHeaders(),
		PathParams: map[string]string{}, Meta: map[string]any{},
		RemoteAddr: "10.0.0.1:55555",
	}
}

func okHandler(req *httpcore.Request, w *httpcore.ResponseWriter) { w.WriteString("ok") }

func authed() httpcore.Handler {
	return Chain(okHandler, BearerAuth(AuthConfig{
		Tokens: map[string]Role{"u-token": RoleUser, "a-token": RoleAdmin},
		RequiredRole: func(path string) Role {
			if strings.HasPrefix(path, "/admin/") {
				return RoleAdmin
			}
			return RoleAnonymous
		},
	}))
}

func TestNoCredentialsIs401(t *testing.T) {
	w := httpcore.NewResponseWriter()
	authed()(makeReq("GET", "/admin/x"), w)
	if w.Status() != 401 {
		t.Fatalf("got %d, want 401", w.Status())
	}
	if !strings.Contains(w.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Fatal("401 must carry WWW-Authenticate: Bearer")
	}
}

func TestUnknownTokenIs401(t *testing.T) {
	req := makeReq("GET", "/admin/x")
	req.Headers.Set("Authorization", "Bearer nope")
	w := httpcore.NewResponseWriter()
	authed()(req, w)
	if w.Status() != 401 {
		t.Fatalf("got %d, want 401", w.Status())
	}
}

func TestAuthenticatedButUnprivilegedIs403(t *testing.T) {
	req := makeReq("GET", "/admin/x")
	req.Headers.Set("Authorization", "Bearer u-token")
	w := httpcore.NewResponseWriter()
	authed()(req, w)
	if w.Status() != 403 {
		t.Fatalf("got %d, want 403 (NOT 401: identity was proven)", w.Status())
	}
	if w.Header().Has("WWW-Authenticate") {
		t.Fatal("403 must not ask the client to re-authenticate")
	}
}

func TestAdminPasses(t *testing.T) {
	req := makeReq("GET", "/admin/x")
	req.Headers.Set("Authorization", "Bearer a-token")
	w := httpcore.NewResponseWriter()
	authed()(req, w)
	if w.Status() != 200 {
		t.Fatalf("got %d, want 200", w.Status())
	}
}

func TestPublicPathSkipsAuth(t *testing.T) {
	w := httpcore.NewResponseWriter()
	authed()(makeReq("GET", "/public"), w)
	if w.Status() != 200 {
		t.Fatalf("got %d, want 200", w.Status())
	}
}

func TestRateLimitBurstThen429(t *testing.T) {
	clock := time.Unix(1000, 0)
	rl := NewRateLimiter(RateLimitConfig{
		RequestsPerSec: 1,
		Burst:          3,
		now:            func() time.Time { return clock },
	})
	defer rl.Stop()
	h := Chain(okHandler, rl.Middleware())

	for i := 0; i < 3; i++ {
		w := httpcore.NewResponseWriter()
		h(makeReq("GET", "/"), w)
		if w.Status() != 200 {
			t.Fatalf("burst request %d: got %d, want 200", i, w.Status())
		}
	}
	w := httpcore.NewResponseWriter()
	h(makeReq("GET", "/"), w)
	if w.Status() != 429 {
		t.Fatalf("got %d, want 429", w.Status())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}

	// One second later a single token has refilled.
	clock = clock.Add(time.Second)
	w = httpcore.NewResponseWriter()
	h(makeReq("GET", "/"), w)
	if w.Status() != 200 {
		t.Fatalf("after refill: got %d, want 200", w.Status())
	}
	w = httpcore.NewResponseWriter()
	h(makeReq("GET", "/"), w)
	if w.Status() != 429 {
		t.Fatalf("second request after single refill: got %d, want 429", w.Status())
	}
}

func TestRateLimitIsolatesClients(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{RequestsPerSec: 1, Burst: 1})
	defer rl.Stop()
	h := Chain(okHandler, rl.Middleware())

	a := makeReq("GET", "/")
	a.RemoteAddr = "10.0.0.1:1"
	b := makeReq("GET", "/")
	b.RemoteAddr = "10.0.0.2:1"

	w := httpcore.NewResponseWriter()
	h(a, w)
	if w.Status() != 200 {
		t.Fatalf("client A first: %d", w.Status())
	}
	w = httpcore.NewResponseWriter()
	h(a, w)
	if w.Status() != 429 {
		t.Fatalf("client A second: got %d, want 429", w.Status())
	}
	w = httpcore.NewResponseWriter()
	h(b, w)
	if w.Status() != 200 {
		t.Fatalf("client B must have its own bucket: got %d", w.Status())
	}
}
