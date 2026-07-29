// Package proxy turns PulseHTTP into a layer-7 load balancer: an upstream
// pool with active TCP health checks, round-robin or least-connections
// selection, per-try failover, and the 502-vs-503 distinction (an upstream
// answered badly vs. nobody is left to ask).
package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/brickster241/PulseHTTP/httpcore"
)

// Upstream is one backend address with liveness and load accounting, plus a
// bounded pool of idle keep-alive connections for reuse across requests.
type Upstream struct {
	Addr    string
	healthy atomic.Bool
	active  atomic.Int64 // in-flight requests, for least-connections
	conns   chan net.Conn
}

func (u *Upstream) Healthy() bool { return u.healthy.Load() }

// Strategy selects the next upstream.
type Strategy string

const (
	RoundRobin       Strategy = "round-robin"
	LeastConnections Strategy = "least-conn"
)

type Config struct {
	Upstreams      []string
	Strategy       Strategy
	DialTimeout    time.Duration
	ResponseTimeout time.Duration
	HealthInterval time.Duration
	MaxAttempts    int   // failover tries per request
	MaxBodyBytes   int64 // cap on relayed upstream bodies
	PoolSize       int   // idle keep-alive connections kept per upstream
}

func (c Config) withDefaults() Config {
	if c.Strategy == "" {
		c.Strategy = RoundRobin
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = 2 * time.Second
	}
	if c.ResponseTimeout == 0 {
		c.ResponseTimeout = 10 * time.Second
	}
	if c.HealthInterval == 0 {
		c.HealthInterval = 2 * time.Second
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 3
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = 8 << 20
	}
	if c.PoolSize == 0 {
		c.PoolSize = 32
	}
	return c
}

type Pool struct {
	cfg  Config
	ups  []*Upstream
	next atomic.Uint64
	stop chan struct{}
}

func NewPool(cfg Config) *Pool {
	cfg = cfg.withDefaults()
	p := &Pool{cfg: cfg, stop: make(chan struct{})}
	for _, addr := range cfg.Upstreams {
		u := &Upstream{Addr: addr, conns: make(chan net.Conn, cfg.PoolSize)}
		u.healthy.Store(true) // optimistic until the first check says otherwise
		p.ups = append(p.ups, u)
	}
	go p.healthLoop()
	return p
}

// Stop terminates health checking and closes pooled connections.
func (p *Pool) Stop() {
	close(p.stop)
	for _, u := range p.ups {
		for {
			select {
			case c := <-u.conns:
				c.Close()
			default:
				goto next
			}
		}
	next:
	}
}

// getConn returns a pooled keep-alive connection when one is idle, else
// dials fresh. reused tells the caller whether a stale-connection retry is
// warranted on failure.
func (p *Pool) getConn(u *Upstream) (conn net.Conn, reused bool, err error) {
	select {
	case c := <-u.conns:
		return c, true, nil
	default:
	}
	c, err := net.DialTimeout("tcp", u.Addr, p.cfg.DialTimeout)
	return c, false, err
}

// putConn parks a healthy connection for reuse; overflow closes it.
func (p *Pool) putConn(u *Upstream, conn net.Conn) {
	select {
	case u.conns <- conn:
	default:
		conn.Close()
	}
}

// Upstreams exposes the pool for status pages.
func (p *Pool) Upstreams() []*Upstream { return p.ups }

func (p *Pool) healthLoop() {
	check := func() {
		for _, u := range p.ups {
			conn, err := net.DialTimeout("tcp", u.Addr, p.cfg.DialTimeout)
			if err != nil {
				u.healthy.Store(false)
				continue
			}
			conn.Close()
			u.healthy.Store(true)
		}
	}
	check()
	t := time.NewTicker(p.cfg.HealthInterval)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			check()
		}
	}
}

// pick returns candidate upstreams in preference order for this request.
func (p *Pool) pick() []*Upstream {
	healthy := make([]*Upstream, 0, len(p.ups))
	for _, u := range p.ups {
		if u.Healthy() {
			healthy = append(healthy, u)
		}
	}
	if len(healthy) == 0 {
		return nil
	}
	switch p.cfg.Strategy {
	case LeastConnections:
		least := healthy[0]
		for _, u := range healthy[1:] {
			if u.active.Load() < least.active.Load() {
				least = u
			}
		}
		ordered := []*Upstream{least}
		for _, u := range healthy {
			if u != least {
				ordered = append(ordered, u)
			}
		}
		return ordered
	default: // round-robin
		start := int(p.next.Add(1)-1) % len(healthy)
		ordered := make([]*Upstream, 0, len(healthy))
		for i := range healthy {
			ordered = append(ordered, healthy[(start+i)%len(healthy)])
		}
		return ordered
	}
}

// Handler relays the parsed request to an upstream and the upstream's
// response to the client. Connect/relay failures fail over to the next
// candidate; a request is only surfaced as 502 after every attempt failed.
func (p *Pool) Handler() httpcore.Handler {
	return func(req *httpcore.Request, w *httpcore.ResponseWriter) {
		candidates := p.pick()
		if candidates == nil {
			w.Header().Set("Retry-After", "2")
			w.Error(httpcore.StatusServiceUnavailable, "no healthy upstreams")
			return
		}
		attempts := min(p.cfg.MaxAttempts, len(candidates))
		var lastErr error
		for i := 0; i < attempts; i++ {
			u := candidates[i]
			u.active.Add(1)
			err := p.relay(u, req, w)
			u.active.Add(-1)
			if err == nil {
				w.Header().Set("X-Upstream", u.Addr)
				return
			}
			lastErr = err
			u.healthy.Store(false) // passive detection; health loop may revive it
		}
		w.Error(httpcore.StatusBadGateway, fmt.Sprintf("all %d upstream attempts failed: %v", attempts, lastErr))
	}
}

// relay performs one upstream exchange through the keep-alive pool. A pooled
// connection may have gone stale since it was parked (the upstream closed it,
// or bytes died in transit) — that failure mode gets ONE retry on a fresh
// dial before counting as an upstream failure.
func (p *Pool) relay(u *Upstream, req *httpcore.Request, w *httpcore.ResponseWriter) error {
	conn, reused, err := p.getConn(u)
	if err != nil {
		return fmt.Errorf("dial %s: %w", u.Addr, err)
	}
	err = p.relayConn(conn, u, req, w)
	if err != nil && reused {
		// Stale pooled connection: dial fresh and retry exactly once.
		conn, dialErr := net.DialTimeout("tcp", u.Addr, p.cfg.DialTimeout)
		if dialErr != nil {
			return fmt.Errorf("dial %s after stale pooled conn: %w", u.Addr, dialErr)
		}
		return p.relayConn(conn, u, req, w)
	}
	return err
}

// relayConn performs one exchange on a specific connection. On success with
// deterministic framing (Content-Length), the connection is parked for reuse;
// every other outcome closes it.
func (p *Pool) relayConn(conn net.Conn, u *Upstream, req *httpcore.Request, w *httpcore.ResponseWriter) error {
	healthyReuse := false
	defer func() {
		if healthyReuse {
			conn.SetDeadline(time.Time{})
			p.putConn(u, conn)
		} else {
			conn.Close()
		}
	}()
	conn.SetDeadline(time.Now().Add(p.cfg.ResponseTimeout))

	bw := bufio.NewWriter(conn)
	bw.WriteString(req.Method + " " + req.RawTarget + " HTTP/1.1\r\n")
	wroteXFF := false
	req.Headers.Range(func(k, v string) {
		switch strings.ToLower(k) {
		case "connection", "keep-alive", "transfer-encoding", "content-length":
			return // hop-by-hop / re-derived below
		case "x-forwarded-for":
			host, _, _ := net.SplitHostPort(req.RemoteAddr)
			bw.WriteString(k + ": " + v + ", " + host + "\r\n")
			wroteXFF = true
			return
		}
		bw.WriteString(k + ": " + v + "\r\n")
	})
	if !wroteXFF {
		host, _, _ := net.SplitHostPort(req.RemoteAddr)
		bw.WriteString("X-Forwarded-For: " + host + "\r\n")
	}
	bw.WriteString("Content-Length: " + strconv.Itoa(len(req.Body)) + "\r\n")
	bw.WriteString("Connection: keep-alive\r\n\r\n")
	bw.Write(req.Body)
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("write to %s: %w", u.Addr, err)
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read status from %s: %w", u.Addr, err)
	}
	parts := strings.SplitN(strings.TrimRight(statusLine, "\r\n"), " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/1.") {
		return fmt.Errorf("bad status line from %s: %q", u.Addr, statusLine)
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		return fmt.Errorf("bad status code from %s: %q", u.Addr, parts[1])
	}

	contentLength := int64(-1)
	upstreamHeaders := [][2]string{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read headers from %s: %w", u.Addr, err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(name) {
		case "content-length":
			contentLength, _ = strconv.ParseInt(value, 10, 64)
			continue
		case "connection", "keep-alive", "transfer-encoding", "date", "server":
			continue
		}
		upstreamHeaders = append(upstreamHeaders, [2]string{name, value})
	}

	var body []byte
	if contentLength >= 0 {
		if contentLength > p.cfg.MaxBodyBytes {
			return fmt.Errorf("upstream %s body of %d exceeds relay cap", u.Addr, contentLength)
		}
		body = make([]byte, contentLength)
		if _, err := io.ReadFull(br, body); err != nil {
			return fmt.Errorf("read body from %s: %w", u.Addr, err)
		}
		// Deterministic framing and a complete read: safe to reuse.
		healthyReuse = true
	} else {
		// No Content-Length: the only framing is EOF, so this connection
		// cannot be reused.
		body, err = io.ReadAll(io.LimitReader(br, p.cfg.MaxBodyBytes+1))
		if err != nil {
			return fmt.Errorf("read body from %s: %w", u.Addr, err)
		}
		if int64(len(body)) > p.cfg.MaxBodyBytes {
			return fmt.Errorf("upstream %s body exceeds relay cap", u.Addr)
		}
	}

	w.WriteHeader(status)
	for _, kv := range upstreamHeaders {
		w.Header().Set(kv[0], kv[1])
	}
	w.SetBody(body)
	return nil
}
