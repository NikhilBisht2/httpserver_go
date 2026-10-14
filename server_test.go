package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// startServer runs a real server on a random free port ("127.0.0.1:0") and
// shuts it down when the test ends.
func startServer(t *testing.T, r *Router, configure func(*Server)) (string, *Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer("", r)
	if configure != nil {
		configure(srv)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Serve(ln)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		<-done
	})
	return ln.Addr().String(), srv
}

func testRouter() *Router {
	r := NewRouter()
	r.Use(Recover)
	r.Handle("GET", "/health", ok("ok"))
	r.Handle("GET", "/boom", func(*Request) *Response { panic("boom") })
	r.Handle("POST", "/echo", echoHandler)
	return r
}

// readResponse parses a response using Go's own net/http client code. If it
// parses, our server speaks real HTTP. Great cross-check for a from-scratch server.
func readResponse(t *testing.T, br *bufio.Reader) (*http.Response, string) {
	t.Helper()
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("could not read response: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("could not read body: %v", err)
	}
	return resp, string(body)
}

func dial(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second)) // never let a test hang forever
	return conn, bufio.NewReader(conn)
}

func TestKeepAliveServesManyRequestsOnOneConnection(t *testing.T) {
	addr, _ := startServer(t, testRouter(), nil)
	conn, br := dial(t, addr)

	for i := 0; i < 5; i++ {
		fmt.Fprint(conn, "GET /health HTTP/1.1\r\nHost: test\r\n\r\n")
		resp, body := readResponse(t, br)
		if resp.StatusCode != 200 || body != "ok" || resp.Close {
			t.Fatalf("request %d: status=%d body=%q close=%v", i, resp.StatusCode, body, resp.Close)
		}
	}
}

func TestPipelinedRequests(t *testing.T) {
	addr, _ := startServer(t, testRouter(), nil)
	conn, br := dial(t, addr)

	// Two requests sent back-to-back in ONE write. The bufio.Reader will have
	// both in its buffer: this tests that we keep leftover bytes between requests.
	fmt.Fprint(conn, "GET /health HTTP/1.1\r\nHost: t\r\n\r\nPOST /echo HTTP/1.1\r\nHost: t\r\nContent-Length: 3\r\n\r\nabc")
	_, body1 := readResponse(t, br)
	_, body2 := readResponse(t, br)
	if body1 != "ok" || body2 != "abc" {
		t.Errorf("bodies = %q, %q", body1, body2)
	}
}

func TestConnectionCloseHeader(t *testing.T) {
	addr, _ := startServer(t, testRouter(), nil)
	conn, br := dial(t, addr)

	fmt.Fprint(conn, "GET /health HTTP/1.1\r\nHost: t\r\nConnection: close\r\n\r\n")
	resp, _ := readResponse(t, br)
	if !resp.Close {
		t.Error("response should say Connection: close")
	}
	if _, err := br.ReadByte(); err != io.EOF {
		t.Errorf("server should have closed the connection, got err=%v", err)
	}
}

func TestHTTP10ClosesByDefault(t *testing.T) {
	addr, _ := startServer(t, testRouter(), nil)
	conn, br := dial(t, addr)

	fmt.Fprint(conn, "GET /health HTTP/1.0\r\n\r\n")
	resp, _ := readResponse(t, br)
	if !resp.Close {
		t.Error("HTTP/1.0 without keep-alive should close")
	}
}

func TestMalformedRequestGets400(t *testing.T) {
	addr, _ := startServer(t, testRouter(), nil)
	conn, br := dial(t, addr)

	fmt.Fprint(conn, "THIS IS NOT HTTP\r\n\r\n")
	resp, _ := readResponse(t, br)
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestOversizedBodyGets413(t *testing.T) {
	addr, _ := startServer(t, testRouter(), nil)
	conn, br := dial(t, addr)

	fmt.Fprint(conn, "POST /echo HTTP/1.1\r\nHost: t\r\nContent-Length: 999999999\r\n\r\n")
	resp, _ := readResponse(t, br)
	if resp.StatusCode != 413 {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

func TestPanicDoesNotKillServer(t *testing.T) {
	addr, _ := startServer(t, testRouter(), nil)

	conn, br := dial(t, addr)
	fmt.Fprint(conn, "GET /boom HTTP/1.1\r\nHost: t\r\n\r\n")
	if resp, _ := readResponse(t, br); resp.StatusCode != 500 {
		t.Fatalf("panic status = %d, want 500", resp.StatusCode)
	}

	// Same connection AND a brand-new one must still work afterwards.
	fmt.Fprint(conn, "GET /health HTTP/1.1\r\nHost: t\r\n\r\n")
	if resp, body := readResponse(t, br); resp.StatusCode != 200 || body != "ok" {
		t.Errorf("after panic, same conn: %d %q", resp.StatusCode, body)
	}
	conn2, br2 := dial(t, addr)
	fmt.Fprint(conn2, "GET /health HTTP/1.1\r\nHost: t\r\n\r\n")
	if resp, _ := readResponse(t, br2); resp.StatusCode != 200 {
		t.Errorf("after panic, new conn: %d", resp.StatusCode)
	}
}

func TestNetHTTPClientInterop(t *testing.T) {
	addr, _ := startServer(t, testRouter(), nil)
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()

	resp, err := client.Get("http://" + addr + "/health")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Errorf("GET: %d %q", resp.StatusCode, body)
	}

	resp, err = client.Post("http://"+addr+"/echo", "text/plain", strings.NewReader("ping"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ping" {
		t.Errorf("POST echo = %q", body)
	}
}

func TestConcurrentClients(t *testing.T) {
	addr, _ := startServer(t, testRouter(), nil)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 100}}
	defer client.CloseIdleConnections()

	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get("http://" + addr + "/health")
			if err != nil {
				errs <- err
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				errs <- fmt.Errorf("status %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestIdleTimeoutClosesConnection(t *testing.T) {
	addr, _ := startServer(t, testRouter(), func(s *Server) { s.IdleTimeout = 100 * time.Millisecond })
	_, br := dial(t, addr)

	// Send nothing. The server should hang up on its own after ~100ms.
	start := time.Now()
	if _, err := br.ReadByte(); err != io.EOF {
		t.Fatalf("expected EOF after idle timeout, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("idle timeout took far too long")
	}
}

func TestReadTimeoutSends408(t *testing.T) {
	addr, _ := startServer(t, testRouter(), func(s *Server) { s.ReadTimeout = 150 * time.Millisecond })
	conn, br := dial(t, addr)

	// Start a request but never finish the headers.
	fmt.Fprint(conn, "GET /health HTTP/1.1\r\nHost: t\r\n")
	resp, _ := readResponse(t, br)
	if resp.StatusCode != 408 {
		t.Errorf("status = %d, want 408", resp.StatusCode)
	}
}

func TestGracefulShutdownLetsInFlightRequestFinish(t *testing.T) {
	started := make(chan struct{})
	r := NewRouter()
	r.Handle("GET", "/slow", func(*Request) *Response {
		close(started)
		time.Sleep(300 * time.Millisecond)
		return Text(200, "finished")
	})
	addr, srv := startServer(t, r, nil)

	type result struct {
		status int
		body   string
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		client := &http.Client{Transport: &http.Transport{}}
		resp, err := client.Get("http://" + addr + "/slow")
		if err != nil {
			resCh <- result{err: err}
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		resCh <- result{status: resp.StatusCode, body: string(body)}
	}()

	<-started // the request is now being processed
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	res := <-resCh
	if res.err != nil || res.status != 200 || res.body != "finished" {
		t.Errorf("in-flight request was cut off: %+v", res)
	}

	// New connections must now be refused.
	if c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		c.Close()
		t.Error("server still accepting connections after Shutdown")
	}
}

func TestShutdownClosesIdleKeepAliveConnections(t *testing.T) {
	addr, srv := startServer(t, testRouter(), nil) // default IdleTimeout is 60s
	conn, br := dial(t, addr)

	fmt.Fprint(conn, "GET /health HTTP/1.1\r\nHost: t\r\n\r\n")
	readResponse(t, br) // connection is now idle but open

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown should not wait for the 60s idle timeout: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Error("shutdown was too slow")
	}
	if _, err := br.ReadByte(); err != io.EOF {
		t.Errorf("idle connection should be closed, got %v", err)
	}
}

func TestMaxConnsLimitsConcurrency(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	active, peak := 0, 0

	r := NewRouter()
	r.Handle("GET", "/hold", func(*Request) *Response {
		mu.Lock()
		active++
		if active > peak {
			peak = active
		}
		mu.Unlock()
		<-release
		mu.Lock()
		active--
		mu.Unlock()
		return Text(200, "done")
	})
	addr, _ := startServer(t, r, func(s *Server) { s.MaxConns = 2 })

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			fmt.Fprint(conn, "GET /hold HTTP/1.1\r\nHost: t\r\nConnection: close\r\n\r\n")
			io.Copy(io.Discard, conn)
		}()
	}

	time.Sleep(300 * time.Millisecond) // let the handlers pile up
	mu.Lock()
	p := peak
	mu.Unlock()
	close(release)
	wg.Wait()

	if p > 2 {
		t.Errorf("peak concurrent handlers = %d, want <= 2", p)
	}
}

// TestRealRouter exercises the actual application routes end to end.
func TestRealRouterEndToEnd(t *testing.T) {
	addr, _ := startServer(t, buildRouter(t.TempDir()), nil)
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()

	tests := []struct {
		method, path string
		status       int
		contains     string
	}{
		{"GET", "/health", 200, "ok"},
		{"GET", "/greet?name=Nikhil", 200, "Hello, Nikhil!"},
		{"GET", "/greet", 200, "Hello, stranger!"},
		{"GET", "/api/stats", 200, "requests_served"},
		{"GET", "/nope", 404, ""},
		{"POST", "/health", 405, ""},
		{"GET", "/panic", 500, ""},
		{"GET", "/static/missing.txt", 404, ""},
	}
	for _, tc := range tests {
		req, _ := http.NewRequest(tc.method, "http://"+addr+tc.path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status || !strings.Contains(string(body), tc.contains) {
			t.Errorf("%s %s: got %d %q, want %d containing %q", tc.method, tc.path, resp.StatusCode, body, tc.status, tc.contains)
		}
	}
}
