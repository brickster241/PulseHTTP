package router

import (
	"net/url"
	"testing"

	"github.com/brickster241/PulseHTTP/httpcore"
)

func makeReq(method, path string) *httpcore.Request {
	return &httpcore.Request{
		Method: method, Path: path, RawTarget: path,
		Query: url.Values{}, Headers: httpcore.NewHeaders(),
		PathParams: map[string]string{}, Meta: map[string]any{},
	}
}

func TestPathParams(t *testing.T) {
	r := New()
	var got string
	r.GET("/users/:id/posts/:post", func(req *httpcore.Request, w *httpcore.ResponseWriter) {
		got = req.PathParams["id"] + "/" + req.PathParams["post"]
		w.WriteString("ok")
	})
	w := httpcore.NewResponseWriter()
	r.Serve(makeReq("GET", "/users/42/posts/7"), w)
	if w.Status() != 200 || got != "42/7" {
		t.Fatalf("status %d params %q", w.Status(), got)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	r := New()
	r.GET("/known", func(req *httpcore.Request, w *httpcore.ResponseWriter) {})
	w := httpcore.NewResponseWriter()
	r.Serve(makeReq("GET", "/unknown"), w)
	if w.Status() != 404 {
		t.Fatalf("got %d, want 404", w.Status())
	}
}

func TestWrongMethodIs405WithAllow(t *testing.T) {
	r := New()
	r.GET("/thing", func(req *httpcore.Request, w *httpcore.ResponseWriter) {})
	r.PUT("/thing", func(req *httpcore.Request, w *httpcore.ResponseWriter) {})
	w := httpcore.NewResponseWriter()
	r.Serve(makeReq("POST", "/thing"), w)
	if w.Status() != 405 {
		t.Fatalf("got %d, want 405", w.Status())
	}
	if allow := w.Header().Get("Allow"); allow != "GET, PUT" {
		t.Fatalf("Allow = %q, want \"GET, PUT\"", allow)
	}
}

func TestHeadFallsBackToGet(t *testing.T) {
	r := New()
	r.GET("/page", func(req *httpcore.Request, w *httpcore.ResponseWriter) {
		w.WriteString("content")
	})
	w := httpcore.NewResponseWriter()
	r.Serve(makeReq("HEAD", "/page"), w)
	if w.Status() != 200 {
		t.Fatalf("got %d, want 200", w.Status())
	}
}
