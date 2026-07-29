// pulsehttp is the demo binary: an API server (or reverse-proxy load
// balancer with -mode proxy) exercising every subsystem — routing, auth,
// rate limiting, caching, metrics, structured logs, graceful shutdown.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brickster241/PulseHTTP/cache"
	"github.com/brickster241/PulseHTTP/httpcore"
	"github.com/brickster241/PulseHTTP/metrics"
	"github.com/brickster241/PulseHTTP/middleware"
	"github.com/brickster241/PulseHTTP/proxy"
	"github.com/brickster241/PulseHTTP/router"
)

func main() {
	var (
		addr      = flag.String("addr", ":8080", "listen address")
		mode      = flag.String("mode", "server", "server | proxy")
		upstreams = flag.String("upstreams", "", "comma-separated upstream host:port list (proxy mode)")
		strategy  = flag.String("strategy", "round-robin", "round-robin | least-conn (proxy mode)")
		rate      = flag.Float64("rate", 500, "rate limit: requests/sec per client (0 disables)")
		burst     = flag.Float64("burst", 1000, "rate limit: burst capacity per client")
		cacheCap  = flag.Int("cache", 1024, "response cache capacity in entries (0 disables)")
		cacheTTL  = flag.Duration("cache-ttl", 30*time.Second, "response cache TTL")
		quietLogs = flag.Bool("quiet", false, "suppress access logs (benchmarking)")
	)
	flag.Parse()

	reg := metrics.NewRegistry()

	var handler httpcore.Handler
	var pool *proxy.Pool
	if *mode == "proxy" {
		if *upstreams == "" {
			fmt.Fprintln(os.Stderr, "proxy mode requires -upstreams")
			os.Exit(2)
		}
		pool = proxy.NewPool(proxy.Config{
			Upstreams: strings.Split(*upstreams, ","),
			Strategy:  proxy.Strategy(*strategy),
		})
		defer pool.Stop()
		handler = pool.Handler()
	} else {
		handler = buildAPI(reg)
	}

	mws := []middleware.Middleware{
		reg.Middleware(),
	}
	if !*quietLogs {
		mws = append(mws, middleware.Logging(os.Stdout))
	}
	mws = append(mws, middleware.RequestID(), middleware.Recover())
	if *rate > 0 {
		rl := middleware.NewRateLimiter(middleware.RateLimitConfig{
			RequestsPerSec: *rate,
			Burst:          *burst,
		})
		defer rl.Stop()
		mws = append(mws, rl.Middleware())
	}
	if *mode == "server" {
		mws = append(mws, middleware.BearerAuth(middleware.AuthConfig{
			Tokens: map[string]middleware.Role{
				"user-alpha-token":  middleware.RoleUser,
				"admin-omega-token": middleware.RoleAdmin,
			},
			RequiredRole: func(path string) middleware.Role {
				switch {
				case strings.HasPrefix(path, "/admin/"):
					return middleware.RoleAdmin
				case strings.HasPrefix(path, "/api/private/"):
					return middleware.RoleUser
				default:
					return middleware.RoleAnonymous
				}
			},
		}))
		if *cacheCap > 0 {
			mws = append(mws, cache.New(*cacheCap, *cacheTTL).Middleware())
		}
	}

	srv := httpcore.NewServer(httpcore.Config{
		Addr:    *addr,
		Handler: middleware.Chain(handler, mws...),
	})

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		fmt.Fprintln(os.Stderr, "\ndraining connections...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	fmt.Fprintf(os.Stderr, "pulsehttp %s mode listening on %s\n", *mode, *addr)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// buildAPI wires the demo routes. Each route exists to demonstrate (and
// test) one behavior of the stack.
func buildAPI(reg *metrics.Registry) httpcore.Handler {
	r := router.New()

	r.GET("/", func(req *httpcore.Request, w *httpcore.ResponseWriter) {
		w.JSON(200, mustJSON(map[string]any{
			"service": "PulseHTTP",
			"proto":   "HTTP/1.1 from raw TCP",
			"routes": []string{
				"GET /healthz", "GET /metrics", "GET /api/users/:id",
				"POST /api/echo", "GET /api/heavy?n=", "GET /api/slow?ms=",
				"GET /api/private/profile", "GET /admin/stats", "GET /boom",
			},
		}))
	})

	r.GET("/healthz", func(req *httpcore.Request, w *httpcore.ResponseWriter) {
		w.JSON(200, []byte(`{"status":"ok"}`))
	})

	r.GET("/metrics", func(req *httpcore.Request, w *httpcore.ResponseWriter) {
		reg.Handler()(req, w)
	})

	r.GET("/api/users/:id", func(req *httpcore.Request, w *httpcore.ResponseWriter) {
		id, err := strconv.Atoi(req.PathParams["id"])
		if err != nil {
			w.Error(httpcore.StatusBadRequest, "user id must be numeric")
			return
		}
		w.JSON(200, mustJSON(map[string]any{
			"id":     id,
			"name":   fmt.Sprintf("user-%04d", id),
			"active": id%2 == 0,
		}))
	})

	r.POST("/api/echo", func(req *httpcore.Request, w *httpcore.ResponseWriter) {
		w.Header().Set("Content-Type", req.Headers.Get("Content-Type"))
		w.WriteHeader(200)
		w.Write(req.Body)
	})

	// CPU-bound work: n rounds of SHA-256. The cache middleware makes
	// repeat GETs of the same n return instantly — measurable HIT speedup.
	r.GET("/api/heavy", func(req *httpcore.Request, w *httpcore.ResponseWriter) {
		n, _ := strconv.Atoi(req.Query.Get("n"))
		if n <= 0 {
			n = 20000
		}
		sum := sha256.Sum256([]byte("pulse"))
		for i := 0; i < n; i++ {
			sum = sha256.Sum256(sum[:])
		}
		w.JSON(200, mustJSON(map[string]any{
			"rounds": n,
			"digest": hex.EncodeToString(sum[:8]),
		}))
	})

	r.GET("/api/slow", func(req *httpcore.Request, w *httpcore.ResponseWriter) {
		ms, _ := strconv.Atoi(req.Query.Get("ms"))
		if ms <= 0 {
			ms = 100
		}
		if ms > 10000 {
			ms = 10000
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		w.JSON(200, mustJSON(map[string]any{"slept_ms": ms}))
	})

	r.GET("/api/private/profile", func(req *httpcore.Request, w *httpcore.ResponseWriter) {
		w.JSON(200, mustJSON(map[string]any{"role": fmt.Sprint(req.Meta["role"]), "plan": "pro"}))
	})

	r.GET("/admin/stats", func(req *httpcore.Request, w *httpcore.ResponseWriter) {
		reg.Handler()(req, w)
	})

	r.GET("/boom", func(req *httpcore.Request, w *httpcore.ResponseWriter) {
		panic("deliberate panic: recovery demo")
	})

	return r.Serve
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
