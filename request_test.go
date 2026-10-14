package main

import (
	"bufio"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"testing"
)

// TestMain runs before all tests in the package: silence the server's logging
// so test output stays readable.
func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

func parse(raw string) (*Request, error) {
	return parseRequest(bufio.NewReader(strings.NewReader(raw)))
}

func TestParseRequestValid(t *testing.T) {
	req, err := parse("GET /search?q=go%20lang&x=1 HTTP/1.1\r\nHost: test\r\nX-Thing: a\r\nX-Thing: b\r\n\r\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Method != "GET" || req.Path != "/search" || req.Version != "HTTP/1.1" {
		t.Errorf("bad request line: %+v", req)
	}
	if got := req.Query.Get("q"); got != "go lang" {
		t.Errorf("query q = %q, want %q", got, "go lang")
	}
	if got := req.Header["x-thing"]; got != "a, b" {
		t.Errorf("duplicate headers = %q, want %q", got, "a, b")
	}
}

func TestParseRequestBody(t *testing.T) {
	req, err := parse("POST /echo HTTP/1.1\r\nHost: test\r\nContent-Length: 5\r\n\r\nhello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(req.Body) != "hello" {
		t.Errorf("body = %q, want hello", req.Body)
	}
}

func TestParseRequestDecodesPath(t *testing.T) {
	req, err := parse("GET /a%20b/%2e%2e/c HTTP/1.1\r\nHost: test\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if req.Path != "/a b/../c" {
		t.Errorf("path = %q, want /a b/../c", req.Path)
	}
}

// Table-driven test: one slice of cases, one loop. The idiomatic Go style.
func TestParseRequestErrors(t *testing.T) {
	manyHeaders := "GET / HTTP/1.1\r\nHost: test\r\n" + strings.Repeat("X: y\r\n", maxHeaderCount+1) + "\r\n"
	longLine := "GET /" + strings.Repeat("a", maxLineSize+10) + " HTTP/1.1\r\nHost: test\r\n\r\n"

	tests := []struct {
		name   string
		raw    string
		status int
	}{
		{"garbage request line", "FOO\r\n\r\n", 400},
		{"empty request line", "\r\n\r\n", 400},
		{"target without slash", "GET foo HTTP/1.1\r\nHost: t\r\n\r\n", 400},
		{"missing host", "GET / HTTP/1.1\r\n\r\n", 400},
		{"header without colon", "GET / HTTP/1.1\r\nHost: t\r\nBroken\r\n\r\n", 400},
		{"space before colon", "GET / HTTP/1.1\r\nHost : t\r\n\r\n", 400},
		{"bad percent-encoding", "GET /%zz HTTP/1.1\r\nHost: t\r\n\r\n", 400},
		{"non-numeric content-length", "POST / HTTP/1.1\r\nHost: t\r\nContent-Length: abc\r\n\r\n", 400},
		{"negative content-length", "POST / HTTP/1.1\r\nHost: t\r\nContent-Length: -5\r\n\r\n", 400},
		{"body too large", "POST / HTTP/1.1\r\nHost: t\r\nContent-Length: 99999999\r\n\r\n", 413},
		{"huge content-length", "POST / HTTP/1.1\r\nHost: t\r\nContent-Length: 99999999999999999999999\r\n\r\n", 413},
		{"chunked unsupported", "POST / HTTP/1.1\r\nHost: t\r\nTransfer-Encoding: chunked\r\n\r\n", 501},
		{"http/2 version", "GET / HTTP/2.0\r\nHost: t\r\n\r\n", 505},
		{"too many headers", manyHeaders, 431},
		{"line too long", longLine, 431},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(tc.raw)
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) {
				t.Fatalf("expected *HTTPError, got %v", err)
			}
			if httpErr.Status != tc.status {
				t.Errorf("status = %d, want %d (%v)", httpErr.Status, tc.status, err)
			}
		})
	}
}

func TestParseRequestTruncatedBody(t *testing.T) {
	_, err := parse("POST / HTTP/1.1\r\nHost: t\r\nContent-Length: 10\r\n\r\nabc")
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestParseRequestEOF(t *testing.T) {
	if _, err := parse(""); !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

func TestWantsKeepAlive(t *testing.T) {
	tests := []struct {
		version, conn string
		want          bool
	}{
		{"HTTP/1.1", "", true},
		{"HTTP/1.1", "close", false},
		{"HTTP/1.1", "Close", false},
		{"HTTP/1.0", "", false},
		{"HTTP/1.0", "keep-alive", true},
	}
	for _, tc := range tests {
		req := &Request{Version: tc.version, Header: map[string]string{"connection": tc.conn}}
		if got := req.WantsKeepAlive(); got != tc.want {
			t.Errorf("%s %q: got %v, want %v", tc.version, tc.conn, got, tc.want)
		}
	}
}

func BenchmarkParseRequest(b *testing.B) {
	raw := "POST /echo?x=1 HTTP/1.1\r\nHost: localhost\r\nUser-Agent: bench\r\nAccept: */*\r\nContent-Length: 5\r\n\r\nhello"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := parse(raw); err != nil {
			b.Fatal(err)
		}
	}
}

func TestWriteFormat(t *testing.T) {
	var sb strings.Builder
	resp := Text(200, "hi")
	if err := Write(resp, &sb, true); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"HTTP/1.1 200 OK\r\n",
		"Content-Length: 2\r\n",
		"Connection: keep-alive\r\n",
		"Content-Type: text/plain; charset=utf-8\r\n",
		"\r\n\r\nhi",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestWriteNoBodyFor204(t *testing.T) {
	var sb strings.Builder
	resp := &Response{StatusCode: 204, StatusText: statusText(204), Body: []byte("ignored")}
	if err := Write(resp, &sb, false); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if strings.Contains(out, "Content-Length") || strings.Contains(out, "ignored") {
		t.Errorf("204 must have no body or Content-Length:\n%s", out)
	}
}

func TestStatusTextUnknown(t *testing.T) {
	if got := statusText(799); got != "Unknown" {
		t.Errorf("statusText(799) = %q", got)
	}
}
