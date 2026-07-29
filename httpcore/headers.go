package httpcore

import (
	"bufio"
	"net/textproto"
	"strings"
)

// Headers is an insertion-ordered, case-insensitive header map. Order is
// preserved so responses serialize deterministically (stable output makes
// golden tests and diffing against other servers possible).
type Headers struct {
	keys []string            // canonical form, insertion order
	vals map[string][]string // canonical key -> values
}

func NewHeaders() *Headers {
	return &Headers{vals: make(map[string][]string)}
}

func canonical(key string) string { return textproto.CanonicalMIMEHeaderKey(key) }

// Set replaces all values for key.
func (h *Headers) Set(key, value string) {
	ck := canonical(key)
	if _, exists := h.vals[ck]; !exists {
		h.keys = append(h.keys, ck)
	}
	h.vals[ck] = []string{value}
}

// Add appends a value for key.
func (h *Headers) Add(key, value string) {
	ck := canonical(key)
	if _, exists := h.vals[ck]; !exists {
		h.keys = append(h.keys, ck)
	}
	h.vals[ck] = append(h.vals[ck], value)
}

// Get returns the first value for key, or "".
func (h *Headers) Get(key string) string {
	if vs := h.vals[canonical(key)]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// Values returns all values for key.
func (h *Headers) Values(key string) []string { return h.vals[canonical(key)] }

// Has reports whether key is present.
func (h *Headers) Has(key string) bool {
	_, ok := h.vals[canonical(key)]
	return ok
}

// Del removes key.
func (h *Headers) Del(key string) {
	ck := canonical(key)
	if _, ok := h.vals[ck]; !ok {
		return
	}
	delete(h.vals, ck)
	for i, k := range h.keys {
		if k == ck {
			h.keys = append(h.keys[:i], h.keys[i+1:]...)
			break
		}
	}
}

// Range calls fn for every header in insertion order (one call per value).
func (h *Headers) Range(fn func(key, value string)) {
	for _, k := range h.keys {
		for _, v := range h.vals[k] {
			fn(k, v)
		}
	}
}

// Len returns the number of distinct header keys.
func (h *Headers) Len() int { return len(h.keys) }

// WriteTo serializes headers in wire format (no terminating blank line).
func (h *Headers) WriteTo(bw *bufio.Writer) {
	h.Range(func(k, v string) {
		bw.WriteString(k)
		bw.WriteString(": ")
		bw.WriteString(v)
		bw.WriteString("\r\n")
	})
}

// ContainsToken reports whether the comma-separated value of key contains
// token (case-insensitive). Used for Connection and Transfer-Encoding.
func (h *Headers) ContainsToken(key, token string) bool {
	for _, v := range h.vals[canonical(key)] {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}
