package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brickster241/PulseHTTP/httpcore"
)

// upstream boots a PulseHTTP server that identifies itself in X-Origin.
func upstream(t *testing.T, name string) (string, *httpcore.Server) {
	t.Helper()
	srv := httpcore.NewServer(httpcore.Config{
		Addr: "127.0.0.1:0",
		Handler: func(req *httpcore.Request, w *httpcore.ResponseWriter) {
			w.Header().Set("X-Origin", name)
			w.WriteString("from " + name)
		},
	})
	go srv.ListenAndServe()
	<-srv.Ready()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return srv.Addr(), srv
}

// frontend boots a proxy-mode PulseHTTP server over the pool.
func frontend(t *testing.T, pool *Pool) string {
	t.Helper()
	srv := httpcore.NewServer(httpcore.Config{Addr: "127.0.0.1:0", Handler: pool.Handler()})
	go srv.ListenAndServe()
	<-srv.Ready()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return srv.Addr()
}

func get(t *testing.T, addr string) (int, map[string]string, string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	status, _ := strconv.Atoi(strings.SplitN(statusLine, " ", 3)[1])
	headers := map[string]string{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("headers: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			headers[strings.ToLower(k)] = strings.TrimSpace(v)
		}
	}
	body := ""
	if n, _ := strconv.Atoi(headers["content-length"]); n > 0 {
		buf := make([]byte, n)
		io.ReadFull(br, buf)
		body = string(buf)
	}
	return status, headers, body
}

func TestRoundRobinAlternates(t *testing.T) {
	a, _ := upstream(t, "alpha")
	b, _ := upstream(t, "beta")
	pool := NewPool(Config{Upstreams: []string{a, b}, HealthInterval: 100 * time.Millisecond})
	defer pool.Stop()
	front := frontend(t, pool)

	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		status, headers, _ := get(t, front)
		if status != 200 {
			t.Fatalf("request %d: status %d", i, status)
		}
		seen[headers["x-origin"]]++
	}
	if seen["alpha"] != 3 || seen["beta"] != 3 {
		t.Fatalf("round-robin skewed: %v", seen)
	}
}

func TestFailoverSurvivesUpstreamDeath(t *testing.T) {
	a, srvA := upstream(t, "alpha")
	b, _ := upstream(t, "beta")
	pool := NewPool(Config{Upstreams: []string{a, b}, HealthInterval: 50 * time.Millisecond})
	defer pool.Stop()
	front := frontend(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	srvA.Shutdown(ctx)
	cancel()

	for i := 0; i < 5; i++ {
		status, headers, _ := get(t, front)
		if status != 200 {
			t.Fatalf("request %d after upstream death: status %d", i, status)
		}
		if headers["x-origin"] != "beta" {
			t.Fatalf("request %d served by dead upstream %q", i, headers["x-origin"])
		}
	}
}

func TestAllUpstreamsDownIs503(t *testing.T) {
	pool := NewPool(Config{
		Upstreams:      []string{"127.0.0.1:1"}, // nothing listens on port 1
		HealthInterval: 50 * time.Millisecond,
	})
	defer pool.Stop()
	front := frontend(t, pool)

	time.Sleep(150 * time.Millisecond) // let the health check mark it down
	status, headers, _ := get(t, front)
	if status != 503 {
		t.Fatalf("got %d, want 503 (no healthy upstreams)", status)
	}
	if headers["retry-after"] == "" {
		t.Fatal("503 should hint Retry-After")
	}
}

func TestXForwardedForAppended(t *testing.T) {
	var gotXFF string
	srv := httpcore.NewServer(httpcore.Config{
		Addr: "127.0.0.1:0",
		Handler: func(req *httpcore.Request, w *httpcore.ResponseWriter) {
			gotXFF = req.Headers.Get("X-Forwarded-For")
			w.WriteString("ok")
		},
	})
	go srv.ListenAndServe()
	<-srv.Ready()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	pool := NewPool(Config{Upstreams: []string{srv.Addr()}})
	defer pool.Stop()
	front := frontend(t, pool)

	if status, _, _ := get(t, front); status != 200 {
		t.Fatalf("status %d", status)
	}
	if gotXFF == "" {
		t.Fatal("proxy must add X-Forwarded-For")
	}
}

// TestKeepAliveConnectionPooling: sequential requests through the balancer
// must ride the SAME upstream connection — the upstream should observe one
// client address, not one per request.
func TestKeepAliveConnectionPooling(t *testing.T) {
	var mu sync.Mutex
	remotes := map[string]bool{}
	srv := httpcore.NewServer(httpcore.Config{
		Addr: "127.0.0.1:0",
		Handler: func(req *httpcore.Request, w *httpcore.ResponseWriter) {
			mu.Lock()
			remotes[req.RemoteAddr] = true
			mu.Unlock()
			w.WriteString("pooled")
		},
	})
	go srv.ListenAndServe()
	<-srv.Ready()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	pool := NewPool(Config{Upstreams: []string{srv.Addr()}})
	defer pool.Stop()
	front := frontend(t, pool)

	for i := 0; i < 6; i++ {
		if status, _, _ := get(t, front); status != 200 {
			t.Fatalf("request %d: status %d", i, status)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(remotes) != 1 {
		t.Fatalf("upstream saw %d distinct connections, want 1 (pooling broken): %v", len(remotes), remotes)
	}
}
