package httpcore

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
)

// Limits bound every dimension of an incoming request. Each limit maps to the
// specific status code the RFC assigns to that failure mode — a client should
// always learn *which* limit it hit.
type Limits struct {
	MaxRequestLine int   // exceeded -> 414 (target) / 400
	MaxHeaderLine  int   // single header line, exceeded -> 431
	MaxHeaderBytes int   // all headers combined -> 431
	MaxHeaderCount int   // number of header fields -> 431
	MaxBodyBytes   int64 // Content-Length or decoded chunked size -> 413
}

func DefaultLimits() Limits {
	return Limits{
		MaxRequestLine: 8 << 10,
		MaxHeaderLine:  8 << 10,
		MaxHeaderBytes: 64 << 10,
		MaxHeaderCount: 100,
		MaxBodyBytes:   1 << 20,
	}
}

// ParseError carries the HTTP status the connection loop should answer with
// before closing. Reason is for logs, never leaked into the response body
// beyond the standard phrase. The cause chain is preserved so the server can
// distinguish a network timeout (408) from a malformed request (400).
type ParseError struct {
	Status int
	Reason string
	cause  error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, ReasonPhrase(e.Status), e.Reason)
}

func (e *ParseError) Unwrap() error { return e.cause }

func perr(status int, format string, args ...any) *ParseError {
	return &ParseError{Status: status, Reason: fmt.Sprintf(format, args...)}
}

// perrc is perr with the underlying I/O error preserved for errors.As.
func perrc(status int, cause error, format string, args ...any) *ParseError {
	return &ParseError{Status: status, Reason: fmt.Sprintf(format, args...), cause: cause}
}

// Request is a fully-read HTTP/1.x request. The body is pre-read and bounded
// by Limits.MaxBodyBytes before the handler ever runs, so handlers never
// perform network reads.
type Request struct {
	Method     string
	RawTarget  string // exactly as received, e.g. /users/7?full=1
	Path       string // decoded path, e.g. /users/7
	Proto      string // HTTP/1.0 or HTTP/1.1
	Query      url.Values
	Headers    *Headers
	Body       []byte
	RemoteAddr string
	PathParams map[string]string // filled by the router
	Meta       map[string]any    // cross-middleware scratch space (auth principal, request id, ...)
}

// wantClose reports whether the connection must close after this exchange.
func (r *Request) wantClose() bool {
	if r.Headers.ContainsToken("Connection", "close") {
		return true
	}
	if r.Proto == "HTTP/1.0" && !r.Headers.ContainsToken("Connection", "keep-alive") {
		return true
	}
	return false
}

var knownMethods = map[string]bool{
	"GET": true, "HEAD": true, "POST": true, "PUT": true,
	"DELETE": true, "PATCH": true, "OPTIONS": true,
}

// errCleanClose signals EOF before the first byte of a request — the peer
// simply closed an idle connection, which is not an error.
var errCleanClose = errors.New("connection closed before request")

// readBoundedLine reads a CRLF- (or LF-) terminated line of at most max bytes.
// Exceeding max returns errLineTooLong; the caller maps that to 414/431.
var errLineTooLong = errors.New("line exceeds limit")

func readBoundedLine(r *bufio.Reader, max int) (string, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > max {
			return "", errLineTooLong
		}
		if err == nil {
			break
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return "", err
	}
	s := strings.TrimSuffix(string(buf), "\n")
	s = strings.TrimSuffix(s, "\r")
	return s, nil
}

// ParseRequest reads one complete request off the wire. On protocol errors it
// returns a *ParseError whose Status is ready to be written back.
func ParseRequest(br *bufio.Reader, lim Limits, remoteAddr string) (*Request, error) {
	// --- request line ---------------------------------------------------
	line, err := readBoundedLine(br, lim.MaxRequestLine)
	if err != nil {
		if err == errLineTooLong {
			return nil, perr(StatusURITooLong, "request line exceeds %d bytes", lim.MaxRequestLine)
		}
		if err == io.EOF && line == "" {
			return nil, errCleanClose
		}
		return nil, perrc(StatusBadRequest, err, "reading request line: %v", err)
	}
	parts := strings.Split(line, " ")
	if len(parts) != 3 {
		return nil, perr(StatusBadRequest, "malformed request line %q", line)
	}
	method, target, proto := parts[0], parts[1], parts[2]

	if !knownMethods[method] {
		if method != strings.ToUpper(method) || method == "" {
			return nil, perr(StatusBadRequest, "malformed method %q", method)
		}
		return nil, perr(StatusNotImplemented, "method %q not implemented", method)
	}
	if proto != "HTTP/1.1" && proto != "HTTP/1.0" {
		return nil, perr(StatusHTTPVersionNotSupported, "unsupported protocol %q", proto)
	}
	if !strings.HasPrefix(target, "/") {
		return nil, perr(StatusBadRequest, "target %q must be origin-form", target)
	}

	// --- headers --------------------------------------------------------
	headers := NewHeaders()
	totalHeaderBytes := 0
	for {
		hline, err := readBoundedLine(br, lim.MaxHeaderLine)
		if err != nil {
			if err == errLineTooLong {
				return nil, perr(StatusHeaderFieldsTooLarge, "header line exceeds %d bytes", lim.MaxHeaderLine)
			}
			return nil, perrc(StatusBadRequest, err, "reading headers: %v", err)
		}
		if hline == "" {
			break
		}
		totalHeaderBytes += len(hline)
		if totalHeaderBytes > lim.MaxHeaderBytes {
			return nil, perr(StatusHeaderFieldsTooLarge, "headers exceed %d bytes", lim.MaxHeaderBytes)
		}
		name, value, found := strings.Cut(hline, ":")
		if !found || name == "" || strings.ContainsAny(name, " \t") {
			return nil, perr(StatusBadRequest, "malformed header line %q", hline)
		}
		headers.Add(name, strings.TrimSpace(value))
		if headers.Len() > lim.MaxHeaderCount {
			return nil, perr(StatusHeaderFieldsTooLarge, "more than %d header fields", lim.MaxHeaderCount)
		}
	}
	if proto == "HTTP/1.1" && !headers.Has("Host") {
		return nil, perr(StatusBadRequest, "HTTP/1.1 request without Host header")
	}

	// --- body -----------------------------------------------------------
	var body []byte
	switch {
	case headers.ContainsToken("Transfer-Encoding", "chunked"):
		body, err = readChunkedBody(br, lim)
		if err != nil {
			var pe *ParseError
			if errors.As(err, &pe) {
				return nil, pe
			}
			return nil, perrc(StatusBadRequest, err, "reading chunked body: %v", err)
		}
	case headers.Has("Transfer-Encoding"):
		return nil, perr(StatusNotImplemented, "transfer-encoding %q not supported", headers.Get("Transfer-Encoding"))
	case headers.Has("Content-Length"):
		n, convErr := strconv.ParseInt(headers.Get("Content-Length"), 10, 64)
		if convErr != nil || n < 0 {
			return nil, perr(StatusBadRequest, "invalid Content-Length %q", headers.Get("Content-Length"))
		}
		if n > lim.MaxBodyBytes {
			return nil, perr(StatusPayloadTooLarge, "declared body of %d bytes exceeds limit %d", n, lim.MaxBodyBytes)
		}
		body = make([]byte, n)
		if _, err := io.ReadFull(br, body); err != nil {
			return nil, perrc(StatusBadRequest, err, "short body read: %v", err)
		}
	default:
		// No framing headers: the request has no body (RFC 9112 §6).
	}

	// --- target decomposition -------------------------------------------
	rawPath, rawQuery, _ := strings.Cut(target, "?")
	path, decErr := url.PathUnescape(rawPath)
	if decErr != nil {
		return nil, perr(StatusBadRequest, "undecodable path %q", rawPath)
	}
	query, qErr := url.ParseQuery(rawQuery)
	if qErr != nil {
		return nil, perr(StatusBadRequest, "undecodable query %q", rawQuery)
	}

	return &Request{
		Method:     method,
		RawTarget:  target,
		Path:       path,
		Proto:      proto,
		Query:      query,
		Headers:    headers,
		Body:       body,
		RemoteAddr: remoteAddr,
		PathParams: map[string]string{},
		Meta:       map[string]any{},
	}, nil
}

// readChunkedBody decodes a chunked transfer coding, enforcing the total
// decoded-size limit. Trailer fields are read and discarded.
func readChunkedBody(br *bufio.Reader, lim Limits) ([]byte, error) {
	var body []byte
	for {
		sizeLine, err := readBoundedLine(br, lim.MaxHeaderLine)
		if err != nil {
			return nil, fmt.Errorf("reading chunk size: %w", err)
		}
		// Chunk extensions (";ext=val") are legal; ignore them.
		sizeHex, _, _ := strings.Cut(sizeLine, ";")
		size, err := strconv.ParseInt(strings.TrimSpace(sizeHex), 16, 64)
		if err != nil || size < 0 {
			return nil, perr(StatusBadRequest, "invalid chunk size %q", sizeLine)
		}
		if size == 0 {
			// Trailer section: lines until the terminating blank line.
			for {
				t, err := readBoundedLine(br, lim.MaxHeaderLine)
				if err != nil {
					return nil, fmt.Errorf("reading trailers: %w", err)
				}
				if t == "" {
					return body, nil
				}
			}
		}
		if int64(len(body))+size > lim.MaxBodyBytes {
			return nil, perr(StatusPayloadTooLarge, "chunked body exceeds limit %d", lim.MaxBodyBytes)
		}
		chunk := make([]byte, size)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, fmt.Errorf("short chunk read: %w", err)
		}
		body = append(body, chunk...)
		crlf := make([]byte, 2)
		if _, err := io.ReadFull(br, crlf); err != nil || string(crlf) != "\r\n" {
			return nil, perr(StatusBadRequest, "chunk data not terminated by CRLF")
		}
	}
}
