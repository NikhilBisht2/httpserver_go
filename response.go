// response.go — everything about building and sending HTTP responses.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"
)

// Response is what a handler returns. Write() later turns it into bytes.
type Response struct {
	StatusCode int               // 200, 404, ...
	StatusText string            // "OK", "Not Found", ... (human-readable part of the status line)
	Header     map[string]string // headers to send, e.g. "Content-Type": "text/plain"
	Body       []byte            // []byte (not string) because bodies can be any bytes: images, files...
}

// statusTexts maps a status code to its reason phrase.
var statusTexts = map[int]string{
	200: "OK",
	201: "Created",
	204: "No Content",
	400: "Bad Request",
	403: "Forbidden",
	404: "Not Found",
	405: "Method Not Allowed",
	408: "Request Timeout",
	413: "Payload Too Large",
	431: "Request Header Fields Too Large",
	500: "Internal Server Error",
	501: "Not Implemented",
	505: "HTTP Version Not Supported",
}

// statusText is a safe lookup. A plain map lookup of a missing key returns ""
// which would produce a broken status line ("HTTP/1.1 418 \r\n"), so we
// fall back to "Unknown" instead.
func statusText(code int) string {
	if t, ok := statusTexts[code]; ok {
		return t
	}
	return "Unknown"
}

// Bytes builds a response with any content type. All other helpers use it.
func Bytes(status int, contentType string, body []byte) *Response {
	return &Response{
		StatusCode: status,
		StatusText: statusText(status),
		Header:     map[string]string{"Content-Type": contentType},
		Body:       body,
	}
}

// Text builds a plain-text response so handlers don't fill the struct by hand.
func Text(status int, body string) *Response {
	return Bytes(status, "text/plain; charset=utf-8", []byte(body))
}

// JSON encodes any Go value (struct, map, slice...) as a JSON response.
// `v any` accepts a value of any type ("any" is an alias for interface{}).
func JSON(status int, v any) *Response {
	data, err := json.Marshal(v)
	if err != nil {
		// Marshal can fail for things like channels or functions.
		return Text(500, "could not encode JSON")
	}
	return Bytes(status, "application/json", data)
}

// Write serializes a response onto w (usually the network connection).
//
// On the wire:
//
//	HTTP/1.1 200 OK\r\n
//	Content-Length: 5\r\n
//	Content-Type: text/plain\r\n
//	\r\n
//	hello
//
// keepAlive decides whether we announce "Connection: keep-alive" (the client
// may send another request on this connection) or "Connection: close".
func Write(r *Response, w io.Writer, keepAlive bool) error {
	if r.Header == nil {
		r.Header = make(map[string]string)
	}

	// Some statuses must not have a body at all (204 No Content, 304, 1xx).
	bodyAllowed := !(r.StatusCode == 204 || r.StatusCode == 304 || r.StatusCode < 200)
	if bodyAllowed {
		// Content-Length MUST equal the number of body BYTES, otherwise the
		// client hangs (too big) or truncates (too small).
		r.Header["Content-Length"] = strconv.Itoa(len(r.Body))
	} else {
		r.Body = nil
	}

	if keepAlive {
		r.Header["Connection"] = "keep-alive"
	} else {
		r.Header["Connection"] = "close"
	}

	// Standard informational headers (only if a handler didn't set them).
	if _, ok := r.Header["Date"]; !ok {
		r.Header["Date"] = time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
	}
	if _, ok := r.Header["Server"]; !ok {
		r.Header["Server"] = "go-http-server"
	}

	// bufio.Writer collects everything in memory and sends it in (ideally) one
	// network write on Flush. Without it every Fprintf would be its own tiny packet.
	bw := bufio.NewWriter(w)

	if _, err := fmt.Fprintf(bw, "HTTP/1.1 %d %s\r\n", r.StatusCode, r.StatusText); err != nil {
		return err
	}

	// Map iteration order is random in Go. Sorting the keys gives deterministic
	// output, which makes the server much easier to test and debug.
	keys := make([]string, 0, len(r.Header))
	for k := range r.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, err := fmt.Fprintf(bw, "%s: %s\r\n", k, r.Header[k]); err != nil {
			return err
		}
	}

	// A blank line ends the headers.
	if _, err := bw.WriteString("\r\n"); err != nil {
		return err
	}
	if len(r.Body) > 0 {
		if _, err := bw.Write(r.Body); err != nil {
			return err
		}
	}

	// Nothing is really sent until Flush. Forgetting it = client waits forever.
	return bw.Flush()
}
