// server.go — accepting connections and serving requests on each one.
package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"runtime/debug"
	"sync"
	"time"
)

// ErrServerClosed is returned by Serve/ListenAndServe after Shutdown.
var ErrServerClosed = errors.New("server closed")

// Server owns the listener, the connections and the timeouts.
type Server struct {
	Addr    string
	Handler *Router

	ReadTimeout  time.Duration // max time to receive one complete request
	WriteTimeout time.Duration // max time to send one response
	IdleTimeout  time.Duration // max time a keep-alive connection may sit unused
	MaxConns     int           // max simultaneous connections (0 = unlimited)

	mu           sync.Mutex            // protects the fields below
	ln           net.Listener          // the listening socket
	idle         map[net.Conn]struct{} // connections currently waiting for a request
	shuttingDown bool
	wg           sync.WaitGroup // counts active connections, so Shutdown can wait for them
}

// NewServer returns a Server with sensible defaults.
func NewServer(addr string, handler *Router) *Server {
	return &Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
		MaxConns:     1000,
		idle:         make(map[net.Conn]struct{}),
	}
}

// ListenAndServe opens the port and serves until Shutdown is called.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Serve is the ACCEPT LOOP: wait for a client, hand the connection to a new
// goroutine, go back to waiting.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		ln.Close()
		return ErrServerClosed
	}
	s.ln = ln
	s.mu.Unlock()

	// A buffered channel used as a SEMAPHORE: it holds at most MaxConns items,
	// so sending blocks once that many connections are active. This limits
	// resource use under heavy load.
	var sem chan struct{}
	if s.MaxConns > 0 {
		sem = make(chan struct{}, s.MaxConns)
	}

	for {
		if sem != nil {
			sem <- struct{}{} // take a slot (blocks when full)
		}

		conn, err := ln.Accept() // blocks until a client connects
		if err != nil {
			if sem != nil {
				<-sem // give the slot back
			}
			if s.isShuttingDown() || errors.Is(err, net.ErrClosed) {
				return ErrServerClosed
			}
			// A failed accept (e.g. too many open files) must not kill the server.
			log.Printf("accept error: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// Register the connection with the WaitGroup under the lock, so it
		// can't race with Shutdown calling Wait().
		s.mu.Lock()
		if s.shuttingDown {
			s.mu.Unlock()
			conn.Close()
			if sem != nil {
				<-sem
			}
			return ErrServerClosed
		}
		s.wg.Add(1)
		s.mu.Unlock()

		// `go` starts a goroutine (a very cheap lightweight thread): the loop
		// immediately continues accepting while this connection is served.
		go func() {
			defer s.wg.Done()
			if sem != nil {
				defer func() { <-sem }()
			}
			s.serveConn(conn)
		}()
	}
}

// serveConn handles ONE client connection: it loops, serving as many requests
// as the client sends (HTTP keep-alive), until something ends the connection.
func (s *Server) serveConn(conn net.Conn) {
	defer conn.Close() // always release the socket

	// Last line of defence: a panic outside the handlers (the Recover
	// middleware covers those) must not crash the whole server.
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("panic on connection %s: %v\n%s", conn.RemoteAddr(), rec, debug.Stack())
		}
	}()

	// ONE bufio.Reader per connection, reused for every request. It may have
	// read ahead into the next request, and those bytes must stay in it.
	reader := bufio.NewReader(conn)

	for {
		// 1. Wait for the next request (idle timeout applies here).
		if !s.waitForRequest(conn, reader) {
			return
		}

		// 2. A request has started arriving: it must complete within ReadTimeout.
		s.setReadDeadline(conn, s.ReadTimeout)
		req, err := parseRequest(reader)
		if err != nil {
			s.writeParseError(conn, err)
			return // after a bad request the stream is out of sync: close
		}
		req.RemoteAddr = conn.RemoteAddr().String()

		// 3. Route it (middleware + handler).
		resp := s.Handler.Route(req)

		// 4. Decide whether to keep the connection, then send the response.
		keepAlive := req.WantsKeepAlive() && !s.isShuttingDown()
		if s.WriteTimeout > 0 {
			conn.SetWriteDeadline(time.Now().Add(s.WriteTimeout))
		}
		if err := Write(resp, conn, keepAlive); err != nil {
			log.Printf("write error to %s: %v", req.RemoteAddr, err)
			return
		}
		if !keepAlive {
			return
		}
	}
}

// waitForRequest blocks until the first byte of the next request arrives.
// It returns false if the connection should be closed instead (client left,
// idle timeout hit, or the server is shutting down).
func (s *Server) waitForRequest(conn net.Conn, r *bufio.Reader) bool {
	s.setReadDeadline(conn, s.IdleTimeout)

	// Register as idle so Shutdown can wake us up. This also tells us if a
	// shutdown has already begun.
	if !s.markIdle(conn) {
		return false
	}
	// Peek(1) waits for one byte WITHOUT consuming it. If the buffer already
	// holds data (pipelined requests), it returns immediately.
	_, err := r.Peek(1)
	s.unmarkIdle(conn)
	return err == nil
}

// setReadDeadline sets (or clears, if d <= 0) the deadline for the next read.
// A deadline is an absolute time after which reads fail with a timeout error.
func (s *Server) setReadDeadline(conn net.Conn, d time.Duration) {
	if d > 0 {
		conn.SetReadDeadline(time.Now().Add(d))
	} else {
		conn.SetReadDeadline(time.Time{}) // zero time = no deadline
	}
}

// writeParseError answers a request we couldn't parse with a proper error
// response (400, 413, 431, 408...) instead of silently dropping the client.
func (s *Server) writeParseError(conn net.Conn, err error) {
	var resp *Response

	var httpErr *HTTPError
	var netErr net.Error
	switch {
	case errors.As(err, &httpErr): // errors.As finds an *HTTPError inside err
		resp = Text(httpErr.Status, httpErr.Msg+"\n")
	case errors.As(err, &netErr) && netErr.Timeout():
		resp = Text(408, "request timed out\n")
	default:
		// Client closed the connection mid-request (EOF) or reset it:
		// nobody is listening for a response.
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			log.Printf("read error from %s: %v", conn.RemoteAddr(), err)
		}
		return
	}

	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	Write(resp, conn, false)
}

func (s *Server) isShuttingDown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shuttingDown
}

// markIdle records conn as waiting for a request. It returns false if the
// server is already shutting down (the caller should close the connection).
func (s *Server) markIdle(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shuttingDown {
		return false
	}
	s.idle[conn] = struct{}{}
	return true
}

func (s *Server) unmarkIdle(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.idle, conn)
}

// Shutdown stops the server gracefully:
//  1. stop accepting new connections,
//  2. wake idle keep-alive connections so they close,
//  3. let in-flight requests finish,
//  4. wait for everything to end, or until ctx expires.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.shuttingDown = true
	ln := s.ln
	for conn := range s.idle {
		// A deadline in the past makes the blocked Peek() fail right away.
		conn.SetReadDeadline(time.Now())
	}
	s.mu.Unlock()

	if ln != nil {
		ln.Close()
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait() // returns when every connection goroutine has finished
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err() // gave up waiting
	}
}
