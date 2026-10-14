# Go HTTP Server From Scratch

An HTTP/1.1 server written in Go on top of **raw TCP sockets**. It uses the `net` package only for the socket, and implements request parsing, routing, response writing, keep-alive, timeouts and graceful shutdown by hand. Go's `net/http` is **not** used by the server (only by the tests, as an independent client to check that the server speaks real HTTP).

Built as a learning project to understand what actually happens between `curl` and a handler function.

## Features

| Area | What's implemented |
|---|---|
| **Protocol** | HTTP/1.1 and HTTP/1.0, request line + headers + `Content-Length` bodies, query strings, percent-decoding, duplicate-header merging |
| **Connections** | Goroutine per connection, **persistent connections (keep-alive)**, pipelined requests, `Connection: close` handling |
| **Routing** | Exact routes + prefix routes, automatic **404** and **405** (with `Allow` header), exact-beats-prefix |
| **Middleware** | Composable `func(Handler) Handler` chain: request logging, **panic recovery**, request counting |
| **Static files** | Content-Type detection, directory `index.html`, **path-traversal protection**, dotfile blocking, `nosniff` header |
| **Robustness** | Read / write / idle **timeouts**, header-size, header-count and body-size limits, proper `400` / `408` / `413` / `431` / `501` / `505` responses, max-connections limit |
| **Operations** | **Graceful shutdown** on Ctrl+C / SIGTERM (finishes in-flight requests, closes idle connections), command-line flags |
| **Quality** | Table-driven unit tests, end-to-end tests over real sockets, race detector clean, ~80% statement coverage |

## Architecture

```
                 ┌──────────────────────────────────────────────┐
  client ──TCP──▶│ Accept loop (server.go)                      │
                 │   semaphore limits concurrent connections    │
                 └───────────────┬──────────────────────────────┘
                                 │ one goroutine per connection
                                 ▼
                 ┌──────────────────────────────────────────────┐
                 │ serveConn loop (keep-alive)                  │
                 │  1. wait for next request   (idle timeout)   │
                 │  2. parseRequest            (read timeout)   │
                 │        └─ error? -> 400/408/413/431/501/505  │
                 │  3. Router.Route                              │
                 │  4. Write response          (write timeout)  │
                 │  5. keep-alive? loop : close                 │
                 └───────────────┬──────────────────────────────┘
                                 ▼
        Logger ─▶ Recover ─▶ CountRequests ─▶ Router ─▶ Handler
        (middleware chain, outermost first)    exact → prefix → 404/405
```

### Project layout

```
main.go        flags, route registration, signal handling, graceful shutdown
server.go      listener, accept loop, connection lifecycle, timeouts, Shutdown
request.go     request parsing with size limits and strict validation
response.go    response building and serialization
router.go      exact + prefix routing, 405 handling, middleware chain
middleware.go  Logger, Recover, CountRequests
static.go      safe static file serving
handlers.go    application endpoints
*_test.go      unit and end-to-end tests
public/        sample static site used to exercise the file handler
```

## Run it

Requires Go 1.21+.

```bash
go run .                       # listens on :8080, serves ./public under /static/
go run . -addr :9000 -static public -max-conns 500
```

Then open <http://localhost:8080/static/index.html>.

### Endpoints

| Method | Path | Description |
|---|---|---|
| GET | `/` | greeting |
| GET | `/health` | liveness check |
| GET | `/time` | server time |
| GET | `/greet?name=Ada` | query-string demo |
| GET | `/about` | serves this README |
| GET | `/api/stats` | JSON: uptime, request count, goroutines |
| POST | `/echo` | returns the request body |
| GET | `/panic` | deliberately panics, to demonstrate recovery (returns 500, server survives) |
| GET | `/static/*` | files from the static directory |

```bash
curl -i localhost:8080/health
curl -X POST -d 'hello' localhost:8080/echo
curl -s localhost:8080/api/stats
curl -v localhost:8080/health localhost:8080/time    # note "Re-using existing connection"
```

## Tests

```bash
go test ./...                  # all tests
go test -race ./...            # with the data-race detector
go test -cover ./...           # coverage
go test -bench . -benchmem     # parser benchmark
```

What's covered:

- **Parser**: valid requests, every error class (table-driven), truncated bodies, oversized lines/headers/bodies, percent-decoding.
- **Router**: 404, 405 + `Allow`, prefix matching, precedence, nil handlers, middleware ordering, panic recovery.
- **Static files**: content types, binary files byte-for-byte intact, directory index, **path-traversal attempts** (including a sibling directory sharing the prefix), dotfiles, null bytes.
- **Server (over real sockets)**: keep-alive across many requests, pipelining, `Connection: close`, HTTP/1.0, 400 / 408 / 413 responses, panic survival, idle timeout, concurrent clients, max-connections limit, **graceful shutdown** (in-flight requests complete, idle connections close immediately), and interoperability with Go's own `net/http` client.

### Manual security checks

```bash
curl --path-as-is -i localhost:8080/static/../main.go        # 403
curl --path-as-is -i localhost:8080/static/%2e%2e/main.go    # 403
curl -i localhost:8080/static/.env                           # 404
printf 'NOT HTTP\r\n\r\n' | nc localhost 8080                # 400
```

## Benchmarking

Install a load generator such as [`hey`](https://github.com/rakyll/hey) or `wrk`, then run the server and:

```bash
hey -n 100000 -c 100 http://localhost:8080/health
```

To compare against Go's standard library, write a 10-line `net/http` server with the same `/health` route, run the same command, and fill in the table below with **your own measurements** (results depend heavily on your machine):

| Server | Requests/sec | p99 latency |
|---|---|---|
| This project | _measure_ | _measure_ |
| `net/http` | _measure_ | _measure_ |

## Design decisions

- **One goroutine per connection.** Blocking I/O is cheap in Go, so the code stays straightforward (read, handle, write) instead of using callbacks or an event loop.
- **Single `bufio.Reader` per connection.** It may read ahead past one request into the next, so it must be reused across the keep-alive loop. Pipelining is tested.
- **Parser returns typed `HTTPError`s.** The server turns them into the right status code instead of silently dropping the client.
- **Path-traversal check happens after joining and cleaning**, on the final absolute path, with a trailing separator in the prefix comparison (so `/site/public-secrets` isn't mistaken for being inside `/site/public`).
- **Idle connections are tracked** so `Shutdown` can wake them immediately instead of waiting for the idle timeout.
- **Semaphore (buffered channel)** caps concurrent connections for backpressure.
- **Middleware as closures**, composed so the logger sees the final status, including the 500 created by the recovery middleware.

## Known limitations

Deliberate scope cuts, listed so nobody assumes otherwise:

- No TLS / HTTPS.
- No chunked request bodies (`Transfer-Encoding` returns 501).
- No `HEAD`, `OPTIONS`, `Expect: 100-continue`, range requests, compression or caching headers (ETag / Last-Modified).
- Static files are read fully into memory (no streaming) and symlinks are not specially handled.
- Request headers are stored in a `map[string]string` (repeated headers are joined with `", "`).
- HTTP/2 is not supported.
