package httpcore

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Config tunes the server. Zero values fall back to the defaults below —
// every timeout exists to bound a specific misbehavior (slowloris, dead
// peers, oversized payloads), and each maps to a specific status code.
type Config struct {
	Addr        string
	Handler     Handler
	Limits      Limits
	ReadTimeout time.Duration // whole request must arrive within this -> 408
	IdleTimeout time.Duration // keep-alive wait for the next request -> silent close
	WriteTimeout time.Duration
	MaxRequestsPerConn int // 0 = unlimited
	ShutdownGrace      time.Duration
	// TLS, when set, wraps the raw TCP listener so the same HTTP/1.1 engine
	// serves https. The protocol layer above is unchanged — TLS is a
	// transport concern and stays one.
	TLS *tls.Config
}

func (c Config) withDefaults() Config {
	if c.Limits == (Limits{}) {
		c.Limits = DefaultLimits()
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = 10 * time.Second
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 30 * time.Second
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = 10 * time.Second
	}
	if c.ShutdownGrace == 0 {
		c.ShutdownGrace = 5 * time.Second
	}
	return c
}

// Server is a from-scratch HTTP/1.1 server on a raw TCP listener: accept
// loop, one goroutine per connection, bounded parsing, keep-alive, and
// graceful drain. net/http is deliberately absent from this path.
type Server struct {
	cfg      Config
	listener net.Listener

	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	draining atomic.Bool

	// Ready is closed once the listener is bound (tests wait on it).
	ready chan struct{}
	addr  atomic.Value // string
}

func NewServer(cfg Config) *Server {
	return &Server{
		cfg:   cfg.withDefaults(),
		conns: make(map[net.Conn]struct{}),
		ready: make(chan struct{}),
	}
}

// Addr returns the bound listen address (valid after Ready).
func (s *Server) Addr() string {
	if v := s.addr.Load(); v != nil {
		return v.(string)
	}
	return s.cfg.Addr
}

// Ready is closed when the listener is accepting.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// ListenAndServe blocks until Shutdown closes the listener.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.Addr, err)
	}
	if s.cfg.TLS != nil {
		ln = tls.NewListener(ln, s.cfg.TLS)
	}
	s.listener = ln
	s.addr.Store(ln.Addr().String())
	close(s.ready)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.draining.Load() {
				return nil // Shutdown closed the listener.
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return fmt.Errorf("accept: %w", err)
		}
		s.track(conn, true)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.track(conn, false)
			defer conn.Close()
			s.serveConn(conn)
		}()
	}
}

func (s *Server) track(c net.Conn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		s.conns[c] = struct{}{}
	} else {
		delete(s.conns, c)
	}
}

// Shutdown stops accepting, lets in-flight requests finish within the grace
// period, then force-closes stragglers.
func (s *Server) Shutdown(ctx context.Context) error {
	s.draining.Store(true)
	if s.listener != nil {
		s.listener.Close()
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	grace := time.NewTimer(s.cfg.ShutdownGrace)
	defer grace.Stop()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
	case <-grace.C:
	}
	// Force-close whatever is left.
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	<-done
	return nil
}

// serveConn runs the keep-alive request loop for one connection.
func (s *Server) serveConn(conn net.Conn) {
	br := bufio.NewReaderSize(conn, 16<<10)
	bw := bufio.NewWriterSize(conn, 16<<10)
	served := 0

	for {
		if s.draining.Load() && served > 0 {
			return // Finish nothing new during drain.
		}

		// Wait for the first byte of the next request (idle period).
		conn.SetReadDeadline(time.Now().Add(s.cfg.IdleTimeout))
		if _, err := br.Peek(1); err != nil {
			return // Peer closed or idled out: silent close, not an error.
		}

		// The request now has ReadTimeout to arrive completely.
		conn.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
		req, err := ParseRequest(br, s.cfg.Limits, conn.RemoteAddr().String())
		if err != nil {
			if err == errCleanClose {
				return
			}
			status, reason := StatusBadRequest, err.Error()
			var pe *ParseError
			if errors.As(err, &pe) {
				status = pe.Status
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				// Started a request but could not finish it in time.
				status, reason = StatusRequestTimeout, "request not received in time"
			}
			s.writeError(conn, bw, status, reason)
			return
		}

		w := NewResponseWriter()
		w.isHead = req.Method == "HEAD"
		s.safeHandle(req, w)

		served++
		keepAlive := !req.wantClose() && !s.draining.Load() &&
			(s.cfg.MaxRequestsPerConn == 0 || served < s.cfg.MaxRequestsPerConn)

		conn.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
		if err := w.serialize(bw, "HTTP/1.1", keepAlive); err != nil {
			return
		}
		if !keepAlive {
			return
		}
	}
}

// safeHandle isolates handler panics: the connection answers 500 instead of
// tearing down the whole process.
func (s *Server) safeHandle(req *Request, w *ResponseWriter) {
	defer func() {
		if r := recover(); r != nil {
			w.hijacked = false
			w.Error(StatusInternalServerError, "internal error")
		}
	}()
	s.cfg.Handler(req, w)
}

func (s *Server) writeError(conn net.Conn, bw *bufio.Writer, status int, _ string) {
	w := NewResponseWriter()
	w.Error(status, "")
	if status == StatusRequestTimeout || status == StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	conn.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
	w.serialize(bw, "HTTP/1.1", false)
}
