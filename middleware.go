// middleware.go — wrappers that add behaviour around every handler.
package main

import (
	"log"
	"runtime/debug"
	"sync/atomic"
	"time"
)

// totalRequests counts every request. atomic.Int64 is safe to use from many
// goroutines at once without a mutex.
var totalRequests atomic.Int64

// Logger prints one line per request: client, method, target, status, duration.
//
// This is a CLOSURE: the returned function remembers `next` from the call that
// created it.
func Logger(next HandlerFunc) HandlerFunc {
	return func(req *Request) *Response {
		start := time.Now()
		resp := next(req) // run the rest of the chain
		log.Printf("%s %s %s -> %d (%s)",
			req.RemoteAddr, req.Method, req.Target,
			resp.StatusCode, time.Since(start).Round(time.Microsecond))
		return resp
	}
}

// Recover turns a panic inside a handler into a 500 response.
//
// Without it, a panic in ANY goroutine crashes the whole process, so one bad
// request would take the entire server down.
//
// Notice the NAMED result `(resp *Response)`: the deferred function runs after
// a panic, when the normal `return` never happens, so it must set the return
// value by assigning to the named variable.
func Recover(next HandlerFunc) HandlerFunc {
	return func(req *Request) (resp *Response) {
		defer func() {
			// recover() returns the panic value, or nil if there was no panic.
			// It only works when called directly inside a deferred function.
			if rec := recover(); rec != nil {
				log.Printf("panic serving %s %s: %v\n%s", req.Method, req.Target, rec, debug.Stack())
				resp = Text(500, "Internal Server Error")
			}
		}()
		return next(req)
	}
}

// CountRequests increments the global request counter.
func CountRequests(next HandlerFunc) HandlerFunc {
	return func(req *Request) *Response {
		totalRequests.Add(1)
		return next(req)
	}
}
