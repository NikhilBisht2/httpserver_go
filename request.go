// request.go — parsing raw bytes from a connection into a Request.
package main

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
)

// Limits protect the server from clients that send absurd amounts of data.
const (
	maxLineSize    = 8 << 10 // longest allowed request/header line: 8 KB
	maxHeaderCount = 100     // most headers allowed in one request
	maxBodySize    = 1 << 20 // largest allowed body: 1 MB
)

// Request is the parsed form of what the client sent.
type Request struct {
	Method     string            // "GET", "POST", ...
	Target     string            // raw request target exactly as sent: "/search?q=go"
	Path       string            // decoded path without the query: "/search"
	Query      url.Values        // parsed query string: {"q": ["go"]}
	Version    string            // "HTTP/1.1"
	Header     map[string]string // header names are stored in lowercase
	Body       []byte            // empty if the request had no body
	RemoteAddr string            // client address, filled in by the server
}

// HTTPError is an error that knows which HTTP status code it should produce.
// The parser returns it so the server can answer with a proper 400/413/431...
// instead of silently hanging up.
type HTTPError struct {
	Status int
	Msg    string
}

// Error makes *HTTPError satisfy Go's built-in `error` interface.
func (e *HTTPError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, statusText(e.Status), e.Msg)
}

func badRequest(msg string) *HTTPError {
	return &HTTPError{Status: 400, Msg: msg}
}

// WantsKeepAlive reports whether the connection should stay open afterwards.
//   - HTTP/1.1: persistent by default, unless the client sends "Connection: close".
//   - HTTP/1.0: closes by default, unless the client sends "Connection: keep-alive".
func (r *Request) WantsKeepAlive() bool {
	conn := strings.ToLower(r.Header["connection"])
	if r.Version == "HTTP/1.0" {
		return strings.Contains(conn, "keep-alive")
	}
	return !strings.Contains(conn, "close")
}

// readLine reads one line (without the trailing \r\n) but refuses lines longer
// than maxLineSize. Plain ReadString would buffer an endless line forever,
// which is a memory attack.
func readLine(r *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		// ReadLine returns the line in chunks: isPrefix=true means "the line
		// was longer than the buffer, call me again for the rest".
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return "", err
		}
		sb.Write(chunk)
		if sb.Len() > maxLineSize {
			return "", &HTTPError{Status: 431, Msg: "line too long"}
		}
		if !isPrefix {
			return sb.String(), nil
		}
	}
}

// isDigits reports whether s is a non-empty string of only 0-9.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// parseRequest turns the bytes arriving on r into a *Request.
//
//	POST /echo?x=1 HTTP/1.1\r\n   <- request line
//	Host: localhost:8080\r\n      <- headers
//	Content-Length: 5\r\n
//	\r\n                          <- blank line = end of headers
//	hello                         <- body (exactly Content-Length bytes)
//
// TCP is a byte STREAM with no message boundaries, so we find the boundaries
// ourselves: newlines for lines, the blank line for the headers, and
// Content-Length for the body.
func parseRequest(r *bufio.Reader) (*Request, error) {
	// ---- 1. request line ----
	line, err := readLine(r)
	if err != nil {
		return nil, err // io.EOF, timeout, ... — passed up unchanged
	}
	parts := strings.Fields(line)
	if len(parts) != 3 {
		return nil, badRequest("malformed request line")
	}
	method, target, version := parts[0], parts[1], parts[2]

	if version != "HTTP/1.1" && version != "HTTP/1.0" {
		return nil, &HTTPError{Status: 505, Msg: "only HTTP/1.0 and HTTP/1.1 are supported"}
	}
	if !strings.HasPrefix(target, "/") {
		return nil, badRequest("request target must start with /")
	}

	// Split "/search?q=go%20lang" into path and query, then decode both.
	rawPath, rawQuery, _ := strings.Cut(target, "?")
	path, err := url.PathUnescape(rawPath) // "%20" -> " ", "%2e" -> "."
	if err != nil {
		return nil, badRequest("invalid percent-encoding in path")
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, badRequest("invalid query string")
	}

	req := &Request{
		Method:  method,
		Target:  target,
		Path:    path,
		Query:   query,
		Version: version,
		Header:  make(map[string]string),
	}

	// ---- 2. headers ----
	for count := 0; ; count++ {
		line, err := readLine(r)
		if err != nil {
			return nil, err
		}
		if line == "" { // blank line: end of headers
			break
		}
		if count >= maxHeaderCount {
			return nil, &HTTPError{Status: 431, Msg: "too many headers"}
		}

		// Split at the FIRST colon only, since values may contain colons
		// ("Host: localhost:8080").
		key, value, found := strings.Cut(line, ":")
		if !found || key == "" || key != strings.TrimSpace(key) {
			return nil, badRequest("malformed header")
		}
		key = strings.ToLower(key) // header names are case-insensitive
		value = strings.TrimSpace(value)

		// If a header appears twice, HTTP says to join the values with ", ".
		if old, exists := req.Header[key]; exists {
			req.Header[key] = old + ", " + value
		} else {
			req.Header[key] = value
		}
	}

	// HTTP/1.1 requires a Host header.
	if version == "HTTP/1.1" {
		if _, ok := req.Header["host"]; !ok {
			return nil, badRequest("missing Host header")
		}
	}

	// Chunked bodies are not supported (listed as a limitation in the README).
	if _, ok := req.Header["transfer-encoding"]; ok {
		return nil, &HTTPError{Status: 501, Msg: "Transfer-Encoding is not supported"}
	}

	// ---- 3. body ----
	if cl, ok := req.Header["content-length"]; ok {
		if !isDigits(cl) {
			return nil, badRequest("invalid Content-Length")
		}
		n, err := strconv.Atoi(cl)
		// After isDigits the only way Atoi fails is overflow (a gigantic number).
		if err != nil || n > maxBodySize {
			return nil, &HTTPError{Status: 413, Msg: "body too large"}
		}
		req.Body = make([]byte, n)
		// ReadFull blocks until EXACTLY n bytes have arrived. A plain Read may
		// return fewer because TCP can deliver the body in several pieces.
		if _, err := io.ReadFull(r, req.Body); err != nil {
			return nil, err
		}
	}

	return req, nil
}
