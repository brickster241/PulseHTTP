package httpcore

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startServer boots a real listener on a random port.
func startServer(t *testing.T, h Handler, mut func(*Config)) (string, *Server) {
	t.Helper()
	cfg := Config{Addr: "127.0.0.1:0", Handler: h}
	if mut != nil {
		mut(&cfg)
	}
	srv := NewServer(cfg)
	go srv.ListenAndServe()
	<-srv.Ready()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return srv.Addr(), srv
}

func echoHandler(req *Request, w *ResponseWriter) {
	switch req.Path {
	case "/echo":
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(req.Body)
	case "/slow":
		time.Sleep(300 * time.Millisecond)
		w.WriteString("slept")
	default:
		w.WriteString("hello")
	}
}

// readOneResponse consumes exactly one response off the reader.
func readOneResponse(t *testing.T, br *bufio.Reader) (int, map[string]string, string) {
	t.Helper()
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading status line: %v", err)
	}
	parts := strings.SplitN(strings.TrimRight(statusLine, "\r\n"), " ", 3)
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("bad status in %q", statusLine)
	}
	headers := map[string]string{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading headers: %v", err)
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
	if cl := headers["content-length"]; cl != "" {
		n, _ := strconv.Atoi(cl)
		buf := make([]byte, n)
		if _, err := io.ReadFull(br, buf); err != nil {
			t.Fatalf("reading body: %v", err)
		}
		body = string(buf)
	}
	return status, headers, body
}

func dial(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	return conn, bufio.NewReader(conn)
}

func TestSimpleGetAndContentLength(t *testing.T) {
	addr, _ := startServer(t, echoHandler, nil)
	conn, br := dial(t, addr)
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	status, headers, body := readOneResponse(t, br)
	if status != 200 || body != "hello" {
		t.Fatalf("got %d %q", status, body)
	}
	if headers["content-length"] != "5" {
		t.Fatalf("content-length = %q, want 5", headers["content-length"])
	}
	if headers["connection"] != "keep-alive" {
		t.Fatalf("connection = %q, want keep-alive", headers["connection"])
	}
}

func TestKeepAliveServesMultipleRequests(t *testing.T) {
	addr, _ := startServer(t, echoHandler, nil)
	conn, br := dial(t, addr)
	for i := 0; i < 3; i++ {
		fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
		status, _, body := readOneResponse(t, br)
		if status != 200 || body != "hello" {
			t.Fatalf("request %d: got %d %q", i, status, body)
		}
	}
}

func TestConnectionCloseHonored(t *testing.T) {
	addr, _ := startServer(t, echoHandler, nil)
	conn, br := dial(t, addr)
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	_, headers, _ := readOneResponse(t, br)
	if headers["connection"] != "close" {
		t.Fatalf("connection = %q, want close", headers["connection"])
	}
	if _, err := br.ReadByte(); err != io.EOF {
		t.Fatalf("expected EOF after Connection: close, got %v", err)
	}
}

func TestMalformedRequestLineIs400(t *testing.T) {
	addr, _ := startServer(t, echoHandler, nil)
	conn, br := dial(t, addr)
	fmt.Fprintf(conn, "GET /\r\n\r\n") // missing protocol
	status, _, _ := readOneResponse(t, br)
	if status != 400 {
		t.Fatalf("got %d, want 400", status)
	}
}

func TestUnsupportedProtocolIs505(t *testing.T) {
	addr, _ := startServer(t, echoHandler, nil)
	conn, br := dial(t, addr)
	fmt.Fprintf(conn, "GET / HTTP/2.0\r\nHost: x\r\n\r\n")
	status, _, _ := readOneResponse(t, br)
	if status != 505 {
		t.Fatalf("got %d, want 505", status)
	}
}

func TestUnknownMethodIs501(t *testing.T) {
	addr, _ := startServer(t, echoHandler, nil)
	conn, br := dial(t, addr)
	fmt.Fprintf(conn, "BREW / HTTP/1.1\r\nHost: x\r\n\r\n")
	status, _, _ := readOneResponse(t, br)
	if status != 501 {
		t.Fatalf("got %d, want 501", status)
	}
}

func TestMissingHostIs400(t *testing.T) {
	addr, _ := startServer(t, echoHandler, nil)
	conn, br := dial(t, addr)
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\n\r\n")
	status, _, _ := readOneResponse(t, br)
	if status != 400 {
		t.Fatalf("got %d, want 400", status)
	}
}

func TestOversizedHeadersAre431(t *testing.T) {
	addr, _ := startServer(t, echoHandler, nil)
	conn, br := dial(t, addr)
	big := strings.Repeat("x", 9<<10)
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\nX-Big: %s\r\n\r\n", big)
	status, _, _ := readOneResponse(t, br)
	if status != 431 {
		t.Fatalf("got %d, want 431", status)
	}
}

func TestOversizedBodyIs413(t *testing.T) {
	addr, _ := startServer(t, echoHandler, func(c *Config) {
		c.Limits = DefaultLimits()
		c.Limits.MaxBodyBytes = 64
	})
	conn, br := dial(t, addr)
	fmt.Fprintf(conn, "POST /echo HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n%s",
		strings.Repeat("y", 100))
	status, _, _ := readOneResponse(t, br)
	if status != 413 {
		t.Fatalf("got %d, want 413", status)
	}
}

func TestChunkedBodyDecodes(t *testing.T) {
	addr, _ := startServer(t, echoHandler, nil)
	conn, br := dial(t, addr)
	fmt.Fprintf(conn, "POST /echo HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n"+
		"5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n")
	status, _, body := readOneResponse(t, br)
	if status != 200 || body != "hello world" {
		t.Fatalf("got %d %q, want 200 %q", status, body, "hello world")
	}
}

func TestHeadHasHeadersButNoBody(t *testing.T) {
	addr, _ := startServer(t, echoHandler, nil)
	conn, br := dial(t, addr)
	fmt.Fprintf(conn, "HEAD / HTTP/1.1\r\nHost: x\r\n\r\n")
	statusLine, _ := br.ReadString('\n')
	if !strings.Contains(statusLine, "200") {
		t.Fatalf("status line %q", statusLine)
	}
	sawCL := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("headers: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length: 5") {
			sawCL = true
		}
	}
	// Body must be absent: next read should time out or EOF, not return "hello".
	conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if b, err := br.ReadByte(); err == nil {
		t.Fatalf("HEAD response leaked body byte %q", b)
	}
	if !sawCL {
		t.Fatal("HEAD lost the Content-Length header")
	}
}

func TestSlowRequestIs408(t *testing.T) {
	addr, _ := startServer(t, echoHandler, func(c *Config) {
		c.ReadTimeout = 200 * time.Millisecond
	})
	conn, br := dial(t, addr)
	// Send half a request and stall — a slowloris client.
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\nX-Stall")
	status, headers, _ := readOneResponse(t, br)
	if status != 408 {
		t.Fatalf("got %d, want 408", status)
	}
	if headers["connection"] != "close" {
		t.Fatalf("408 must close the connection")
	}
}

func TestPanicIn500NotConnectionDrop(t *testing.T) {
	addr, _ := startServer(t, func(req *Request, w *ResponseWriter) {
		panic("boom")
	}, nil)
	conn, br := dial(t, addr)
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	status, _, _ := readOneResponse(t, br)
	if status != 500 {
		t.Fatalf("got %d, want 500", status)
	}
}

func TestGracefulShutdownDrainsInFlight(t *testing.T) {
	addr, srv := startServer(t, echoHandler, nil)
	conn, br := dial(t, addr)
	fmt.Fprintf(conn, "GET /slow HTTP/1.1\r\nHost: x\r\n\r\n")
	time.Sleep(50 * time.Millisecond) // let the handler start sleeping
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done <- srv.Shutdown(ctx)
	}()
	status, _, body := readOneResponse(t, br)
	if status != 200 || body != "slept" {
		t.Fatalf("in-flight request lost during drain: %d %q", status, body)
	}
	if err := <-done; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}
