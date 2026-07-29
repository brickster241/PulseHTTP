package httpcore

import (
	"bufio"
	"fmt"
	"strconv"
	"time"
)

// Handler is PulseHTTP's handler contract. The ResponseWriter buffers the
// full response; nothing touches the socket until the handler returns, which
// is what makes middleware like caching and metrics able to observe and
// rewrite complete responses.
type Handler func(*Request, *ResponseWriter)

// ResponseWriter accumulates status, headers, and body for one exchange.
type ResponseWriter struct {
	status  int
	headers *Headers
	body    []byte
	isHead  bool
	// hijacked marks that a middleware fully replaced the response (e.g.
	// cache 304) and downstream writes should be ignored.
	hijacked bool
}

func NewResponseWriter() *ResponseWriter {
	return &ResponseWriter{status: StatusOK, headers: NewHeaders()}
}

// Header exposes the response headers for mutation.
func (w *ResponseWriter) Header() *Headers { return w.headers }

// WriteHeader sets the status code. Last call before serialization wins.
func (w *ResponseWriter) WriteHeader(status int) {
	if w.hijacked {
		return
	}
	w.status = status
}

// Write appends to the response body.
func (w *ResponseWriter) Write(p []byte) (int, error) {
	if w.hijacked {
		return len(p), nil
	}
	w.body = append(w.body, p...)
	return len(p), nil
}

// WriteString appends a string to the response body.
func (w *ResponseWriter) WriteString(s string) { w.Write([]byte(s)) }

// JSON writes a pre-marshaled JSON payload with the right content type.
func (w *ResponseWriter) JSON(status int, payload []byte) {
	w.WriteHeader(status)
	w.headers.Set("Content-Type", "application/json")
	w.Write(payload)
}

// Error emits a minimal plain-text error response, replacing any body the
// handler had produced so far.
func (w *ResponseWriter) Error(status int, msg string) {
	if w.hijacked {
		return
	}
	w.status = status
	w.body = w.body[:0]
	w.headers.Set("Content-Type", "text/plain; charset=utf-8")
	if msg == "" {
		msg = ReasonPhrase(status)
	}
	w.body = append(w.body, []byte(fmt.Sprintf("%d %s\n", status, msg))...)
}

// Hijack freezes the response as-is; later writes are discarded.
func (w *ResponseWriter) Hijack() { w.hijacked = true }

// Status returns the status code as currently set.
func (w *ResponseWriter) Status() int { return w.status }

// BodyLen returns the current body size in bytes.
func (w *ResponseWriter) BodyLen() int { return len(w.body) }

// Body returns the buffered body. Callers must not mutate it.
func (w *ResponseWriter) Body() []byte { return w.body }

// SetBody replaces the body wholesale (used by the cache on hits).
func (w *ResponseWriter) SetBody(b []byte) {
	w.body = append(w.body[:0], b...)
}

// bodyless reports whether a status forbids a message body.
func bodyless(status int) bool {
	return status == StatusNoContent || status == StatusNotModified || (status >= 100 && status < 200)
}

// serialize writes the complete response in wire format. keepAlive decides
// the Connection header; HEAD suppresses the body but keeps Content-Length.
func (w *ResponseWriter) serialize(bw *bufio.Writer, proto string, keepAlive bool) error {
	bw.WriteString(proto)
	bw.WriteByte(' ')
	bw.WriteString(strconv.Itoa(w.status))
	bw.WriteByte(' ')
	bw.WriteString(ReasonPhrase(w.status))
	bw.WriteString("\r\n")

	if !w.headers.Has("Date") {
		w.headers.Set("Date", time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"))
	}
	if !w.headers.Has("Server") {
		w.headers.Set("Server", "PulseHTTP/1.0")
	}
	if bodyless(w.status) {
		w.headers.Del("Content-Length")
	} else {
		w.headers.Set("Content-Length", strconv.Itoa(len(w.body)))
	}
	if keepAlive {
		w.headers.Set("Connection", "keep-alive")
	} else {
		w.headers.Set("Connection", "close")
	}
	w.headers.WriteTo(bw)
	bw.WriteString("\r\n")

	if !w.isHead && !bodyless(w.status) {
		bw.Write(w.body)
	}
	return bw.Flush()
}
