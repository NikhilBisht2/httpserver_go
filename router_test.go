package main

import (
	"strings"
	"testing"
)

func ok(body string) HandlerFunc {
	return func(*Request) *Response { return Text(200, body) }
}

func get(path string) *Request {
	return &Request{Method: "GET", Path: path, Target: path, Header: map[string]string{}}
}

func TestRouterExactMatch(t *testing.T) {
	r := NewRouter()
	r.Handle("GET", "/health", ok("ok"))
	resp := r.Route(get("/health"))
	if resp.StatusCode != 200 || string(resp.Body) != "ok" {
		t.Errorf("got %d %q", resp.StatusCode, resp.Body)
	}
}

func TestRouter404(t *testing.T) {
	r := NewRouter()
	r.Handle("GET", "/health", ok("ok"))
	if resp := r.Route(get("/nope")); resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestRouter405AllowHeader(t *testing.T) {
	r := NewRouter()
	r.Handle("GET", "/x", ok("g"))
	r.Handle("POST", "/x", ok("p"))
	req := get("/x")
	req.Method = "DELETE"
	resp := r.Route(req)
	if resp.StatusCode != 405 {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header["Allow"]; got != "GET, POST" {
		t.Errorf("Allow = %q, want %q", got, "GET, POST")
	}
}

func TestRouterPrefix(t *testing.T) {
	r := NewRouter()
	r.HandlePrefix("GET", "/static/", ok("file"))

	if resp := r.Route(get("/static/a/b.css")); string(resp.Body) != "file" {
		t.Errorf("prefix did not match: %d %q", resp.StatusCode, resp.Body)
	}
	if resp := r.Route(get("/staticfoo")); resp.StatusCode != 404 {
		t.Errorf("/staticfoo should not match /static/, got %d", resp.StatusCode)
	}

	post := get("/static/a.css")
	post.Method = "POST"
	resp := r.Route(post)
	if resp.StatusCode != 405 || resp.Header["Allow"] != "GET" {
		t.Errorf("wrong method on prefix: %d Allow=%q", resp.StatusCode, resp.Header["Allow"])
	}
}

func TestRouterExactBeatsPrefix(t *testing.T) {
	r := NewRouter()
	r.HandlePrefix("GET", "/static/", ok("prefix"))
	r.Handle("GET", "/static/special", ok("exact"))
	if resp := r.Route(get("/static/special")); string(resp.Body) != "exact" {
		t.Errorf("exact route should win, got %q", resp.Body)
	}
}

func TestRouterNilHandlerResponse(t *testing.T) {
	r := NewRouter()
	r.Handle("GET", "/nil", func(*Request) *Response { return nil })
	if resp := r.Route(get("/nil")); resp.StatusCode != 500 {
		t.Errorf("nil response should become 500, got %d", resp.StatusCode)
	}
}

func TestMiddlewareOrder(t *testing.T) {
	var trace []string
	mk := func(name string) Middleware {
		return func(next HandlerFunc) HandlerFunc {
			return func(req *Request) *Response {
				trace = append(trace, name+":before")
				resp := next(req)
				trace = append(trace, name+":after")
				return resp
			}
		}
	}
	r := NewRouter()
	r.Use(mk("A"), mk("B"))
	r.Handle("GET", "/", func(*Request) *Response {
		trace = append(trace, "handler")
		return Text(200, "")
	})
	r.Route(get("/"))

	got := strings.Join(trace, " ")
	want := "A:before B:before handler B:after A:after"
	if got != want {
		t.Errorf("order = %q, want %q", got, want)
	}
}

func TestRecoverMiddleware(t *testing.T) {
	r := NewRouter()
	r.Use(Recover)
	r.Handle("GET", "/boom", func(*Request) *Response { panic("boom") })
	resp := r.Route(get("/boom"))
	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

func TestCountRequests(t *testing.T) {
	before := totalRequests.Load()
	r := NewRouter()
	r.Use(CountRequests)
	r.Handle("GET", "/", ok(""))
	r.Route(get("/"))
	r.Route(get("/missing"))
	if got := totalRequests.Load() - before; got != 2 {
		t.Errorf("counted %d requests, want 2", got)
	}
}
