// handlers.go — the application's endpoints. Each is a HandlerFunc:
// "request in, response out", knowing nothing about sockets or bytes.
package main

import (
	"log"
	"os"
	"runtime"
	"time"
)

var startTime = time.Now()

func homeHandler(req *Request) *Response {
	return Text(200, "hello from my Go HTTP server\ntry /static/index.html")
}

func healthHandler(req *Request) *Response {
	// Monitoring tools hit this to check that the server is alive.
	return Text(200, "ok")
}

// echoHandler sends back whatever body the client posted.
func echoHandler(req *Request) *Response {
	return Text(200, string(req.Body))
}

// timeHandler: Go formats times using the REFERENCE DATE "Mon Jan 2 15:04:05
// MST 2006": you write that date in the layout you want.
func timeHandler(req *Request) *Response {
	return Text(200, time.Now().Format("2006-01-02 15:04:05"))
}

// greetHandler demonstrates query strings: GET /greet?name=Nikhil
func greetHandler(req *Request) *Response {
	name := req.Query.Get("name") // "" if the parameter is missing
	if name == "" {
		name = "stranger"
	}
	return Text(200, "Hello, "+name+"!")
}

// aboutHandler serves the project README.
func aboutHandler(req *Request) *Response {
	data, err := os.ReadFile("README.md")
	if err != nil {
		// Log the real reason on the server, send the client a generic message.
		log.Printf("error reading README.md: %v", err)
		return Text(500, "Internal Server Error")
	}
	return Text(200, string(data))
}

// statsHandler returns server statistics as JSON.
func statsHandler(req *Request) *Response {
	return JSON(200, map[string]any{
		"uptime_seconds":  int(time.Since(startTime).Seconds()),
		"requests_served": totalRequests.Load(),
		"goroutines":      runtime.NumGoroutine(),
		"go_version":      runtime.Version(),
	})
}

// panicHandler exists to DEMONSTRATE panic recovery: visit /panic and watch the
// server answer 500 instead of crashing.
func panicHandler(req *Request) *Response {
	panic("deliberate panic to test the Recover middleware")
}
