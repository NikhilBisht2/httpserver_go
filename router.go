// router.go — maps (method, path) to handlers, with middleware support.
package main

import (
	"sort"
	"strings"
)

// HandlerFunc is any function that takes a request and returns a response.
// Functions are "first-class values" in Go: we can store them in maps and pass
// them around, which is how the router stores "which code runs for which URL".
type HandlerFunc func(*Request) *Response

// Middleware wraps a handler to add behaviour before/after it (logging,
// panic recovery, ...). It takes a handler and returns a new, wrapped handler.
type Middleware func(HandlerFunc) HandlerFunc

type prefixRoute struct {
	method  string
	prefix  string
	handler HandlerFunc
}

// Router decides which handler serves a request.
type Router struct {
	Routes     map[string]map[string]HandlerFunc // Routes[path][method] -> handler
	Prefixes   []prefixRoute                     // "everything under /static/"
	middleware []Middleware
}

// NewRouter creates a router ready to use. (A nil map would panic on write.)
func NewRouter() *Router {
	return &Router{Routes: make(map[string]map[string]HandlerFunc)}
}

// Use registers middleware. The FIRST one given is the OUTERMOST wrapper:
// Use(A, B) means a request passes A -> B -> handler -> B -> A.
func (rt *Router) Use(mws ...Middleware) {
	rt.middleware = append(rt.middleware, mws...)
}

// Handle registers a handler for an exact path and method.
func (rt *Router) Handle(method, path string, h HandlerFunc) {
	if rt.Routes[path] == nil {
		rt.Routes[path] = make(map[string]HandlerFunc)
	}
	rt.Routes[path][method] = h
}

// HandlePrefix registers a handler for every path starting with prefix.
// Use a trailing slash ("/static/") so "/staticfoo" doesn't match.
func (rt *Router) HandlePrefix(method, prefix string, h HandlerFunc) {
	rt.Prefixes = append(rt.Prefixes, prefixRoute{method, prefix, h})
}

// Route runs the request through the middleware chain and then the router.
func (rt *Router) Route(req *Request) *Response {
	var h HandlerFunc = rt.dispatch
	// Wrap from the inside out, so the first middleware ends up outermost.
	for i := len(rt.middleware) - 1; i >= 0; i-- {
		h = rt.middleware[i](h)
	}
	return h(req)
}

// dispatch is the actual routing logic: exact routes, then prefixes, then 404.
func (rt *Router) dispatch(req *Request) *Response {
	// 1. Exact path match wins.
	if methods, ok := rt.Routes[req.Path]; ok {
		if h, ok := methods[req.Method]; ok {
			return callHandler(h, req)
		}
		allowed := make([]string, 0, len(methods))
		for m := range methods {
			allowed = append(allowed, m)
		}
		return methodNotAllowed(allowed)
	}

	// 2. Prefix routes. Remember which methods exist for matching prefixes
	// so a wrong method gives 405 instead of 404.
	var allowed []string
	for _, p := range rt.Prefixes {
		if strings.HasPrefix(req.Path, p.prefix) {
			if p.method == req.Method {
				return callHandler(p.handler, req)
			}
			allowed = append(allowed, p.method)
		}
	}
	if len(allowed) > 0 {
		return methodNotAllowed(allowed)
	}

	// 3. Nothing matched.
	return Text(404, "not found")
}

// callHandler runs a handler and guards against it returning nil.
func callHandler(h HandlerFunc, req *Request) *Response {
	resp := h(req)
	if resp == nil {
		return Text(500, "handler returned nothing")
	}
	return resp
}

// methodNotAllowed builds a 405 with the mandatory Allow header.
func methodNotAllowed(methods []string) *Response {
	sort.Strings(methods)
	// Remove duplicates (sorted, so duplicates are neighbours).
	unique := methods[:0]
	for i, m := range methods {
		if i == 0 || m != methods[i-1] {
			unique = append(unique, m)
		}
	}
	resp := Text(405, "method not allowed")
	resp.Header["Allow"] = strings.Join(unique, ", ")
	return resp
}
