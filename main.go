// A small HTTP/1.1 server built from raw TCP sockets (no net/http).
//
// Life of a request:
//
//	Accept loop -> goroutine per connection -> parseRequest -> middleware chain
//	-> router -> handler -> Write -> (keep-alive loop or close)
//
// Files:
//
//	main.go        wiring: flags, routes, graceful shutdown
//	server.go      listener, accept loop, connection lifecycle, timeouts
//	request.go     HTTP request parsing (with limits)
//	response.go    HTTP response building and writing
//	router.go      exact + prefix routing, 404/405, middleware chain
//	middleware.go  logging, panic recovery, request counting
//	static.go      static files with path-traversal protection
//	handlers.go    the application endpoints
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// buildRouter registers all routes and middleware.
// It lives in its own function so tests can build the same router.
func buildRouter(staticDir string) *Router {
	router := NewRouter()

	// Middleware order: Logger is outermost, so it sees the final status code
	// (including the 500 produced by Recover).
	router.Use(Logger, Recover, CountRequests)

	router.Handle("GET", "/", homeHandler)
	router.Handle("GET", "/health", healthHandler)
	router.Handle("GET", "/time", timeHandler)
	router.Handle("GET", "/greet", greetHandler)
	router.Handle("GET", "/about", aboutHandler)
	router.Handle("GET", "/api/stats", statsHandler)
	router.Handle("GET", "/panic", panicHandler)
	router.Handle("POST", "/echo", echoHandler)

	router.HandlePrefix("GET", "/static/", StaticHandler("/static/", staticDir))
	return router
}

func main() {
	// Command-line flags: ./httpserver -addr :9000 -static public
	addr := flag.String("addr", ":8080", "address to listen on")
	staticDir := flag.String("static", "public", "directory served under /static/")
	maxConns := flag.Int("max-conns", 1000, "maximum simultaneous connections")
	flag.Parse()

	srv := NewServer(*addr, buildRouter(*staticDir))
	srv.MaxConns = *maxConns

	// ctx is cancelled when the user presses Ctrl+C (or the OS sends SIGTERM).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Run the server in a goroutine so main can wait for either a startup
	// error or a shutdown signal.
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	absStatic, _ := filepath.Abs(*staticDir)
	log.Printf("listening on %s (static files from %s)", *addr, absStatic)

	select {
	case err := <-errCh:
		// Server failed to start or crashed (e.g. the port is already in use).
		log.Fatalf("server error: %v", err)
	case <-ctx.Done():
		// Ctrl+C pressed.
	}
	stop() // restore default Ctrl+C behaviour: pressing it again force-quits

	log.Println("shutting down: finishing in-flight requests...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown did not finish cleanly: %v", err)
	}
	log.Println("bye")
}
