// Package router maps method+path patterns to handlers with the 404/405
// distinction done right: an unknown path is 404, a known path with the
// wrong verb is 405 and names the verbs that work in an Allow header.
package router

import (
	"sort"
	"strings"

	"github.com/brickster241/PulseHTTP/httpcore"
)

type segment struct {
	literal string
	param   string // non-empty for :name segments
}

type route struct {
	method   string
	segments []segment
	handler  httpcore.Handler
}

type Router struct {
	routes   []route
	NotFound httpcore.Handler
}

func New() *Router {
	return &Router{
		NotFound: func(req *httpcore.Request, w *httpcore.ResponseWriter) {
			w.Error(httpcore.StatusNotFound, "no such resource: "+req.Path)
		},
	}
}

// Handle registers a handler for method + pattern. Pattern segments starting
// with ':' capture path parameters: /users/:id matches /users/42 with id=42.
func (r *Router) Handle(method, pattern string, h httpcore.Handler) {
	segs := []segment{}
	for _, part := range strings.Split(strings.Trim(pattern, "/"), "/") {
		if strings.HasPrefix(part, ":") {
			segs = append(segs, segment{param: part[1:]})
		} else {
			segs = append(segs, segment{literal: part})
		}
	}
	r.routes = append(r.routes, route{method: method, segments: segs, handler: h})
}

func (r *Router) GET(p string, h httpcore.Handler)    { r.Handle("GET", p, h) }
func (r *Router) HEAD(p string, h httpcore.Handler)   { r.Handle("HEAD", p, h) }
func (r *Router) POST(p string, h httpcore.Handler)   { r.Handle("POST", p, h) }
func (r *Router) PUT(p string, h httpcore.Handler)    { r.Handle("PUT", p, h) }
func (r *Router) DELETE(p string, h httpcore.Handler) { r.Handle("DELETE", p, h) }

func match(segs []segment, parts []string) (map[string]string, bool) {
	if len(segs) != len(parts) {
		return nil, false
	}
	params := map[string]string{}
	for i, s := range segs {
		if s.param != "" {
			params[s.param] = parts[i]
			continue
		}
		if s.literal != parts[i] {
			return nil, false
		}
	}
	return params, true
}

// Serve is the router's httpcore.Handler.
func (r *Router) Serve(req *httpcore.Request, w *httpcore.ResponseWriter) {
	parts := strings.Split(strings.Trim(req.Path, "/"), "/")
	allowed := map[string]bool{}
	for _, rt := range r.routes {
		params, ok := match(rt.segments, parts)
		if !ok {
			continue
		}
		if rt.method == req.Method || (req.Method == "HEAD" && rt.method == "GET") {
			req.PathParams = params
			rt.handler(req, w)
			return
		}
		allowed[rt.method] = true
	}
	if len(allowed) > 0 {
		verbs := make([]string, 0, len(allowed))
		for m := range allowed {
			verbs = append(verbs, m)
		}
		sort.Strings(verbs)
		w.Header().Set("Allow", strings.Join(verbs, ", "))
		w.Error(httpcore.StatusMethodNotAllowed, req.Method+" not allowed here")
		return
	}
	r.NotFound(req, w)
}
