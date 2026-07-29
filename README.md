# PulseHTTP

A production-shaped HTTP/1.1 stack built **from raw TCP** in Go — zero dependencies,
`net/http` deliberately absent from the serving path. One codebase, two binaries:

- **`pulsehttp`** — an API server *or* a reverse-proxy load balancer, assembled from
  composable subsystems: bounded protocol core, router, auth, rate limiting, response
  caching, structured logs, metrics, graceful shutdown.
- **`pulsebench`** — its load-testing harness: raw-TCP keep-alive workers, exact
  latency percentiles, status-code accounting.

The design goal is *diagnosability*: every failure mode a client can trigger maps to
the precise status code the RFC assigns it, because `401 vs 403` and `404 vs 405` and
`408 vs 429` are different sentences, not synonyms for "no".

| Client mistake | Answer |
|---|---|
| Malformed request line / headers, missing `Host` | `400` |
| No credentials or unknown token | `401` + `WWW-Authenticate` |
| Valid token, insufficient privilege | `403` (never 401 — identity was proven) |
| Unknown path | `404` |
| Known path, wrong verb | `405` + `Allow: GET, PUT` |
| Sent the request too slowly (slowloris) | `408` + close |
| Body over the configured cap | `413` |
| Request line too long | `414` |
| Sent requests too fast | `429` + `Retry-After` + `X-RateLimit-*` |
| Headers too large / too many | `431` |
| Handler panicked | `500` (connection survives) |
| Method the server never heard of | `501` |
| Every upstream attempt failed | `502` |
| No healthy upstream left to ask | `503` + `Retry-After` |
| `HTTP/2.0` on a 1.1 socket | `505` |

## Architecture

```
                 ┌────────────────────────── pulsehttp (server mode) ─────────────────────────┐
 client ── TCP ──┤ httpcore: accept → parse (bounded) → [metrics → logs → request-id →        │
                 │ recover → rate-limit → auth → cache] → router → handler → serialize        │
                 └──────────────────────────────────────────────────────────────────────────────┘
                 ┌────────────── pulsehttp (proxy mode) ──────────────┐
 client ── TCP ──┤ httpcore → proxy pool: health checks, round-robin/ │── TCP ──▶ upstreams
                 │ least-conn, per-try failover, X-Forwarded-For      │
                 └────────────────────────────────────────────────────┘
```

- **`httpcore`** — request parser with a limit on every dimension (line, header count,
  header bytes, body bytes) and chunked + `Content-Length` framing; buffered response
  writer; keep-alive connection loop with an idle-vs-read deadline split; graceful
  drain that finishes in-flight requests before closing.
- **`router`** — method+path routing with `:param` capture and the 404/405 distinction.
- **`middleware`** — structured JSON access logs, `X-Request-ID` correlation, panic
  recovery, bearer auth, lazy per-client token buckets (no background refill goroutine).
- **`cache`** — LRU + TTL response cache with SHA-256 ETags: fresh hits skip the handler,
  `If-None-Match` earns a bodyless `304`, mutating verbs invalidate their path, and
  `Cache-Control` is honored both ways (`no-store`/`max-age=N` on responses,
  `no-cache` revalidation on requests).
- **`metrics`** — status counters + latency histogram with interpolated p50/p90/p99,
  served as JSON at `/metrics`.
- **`proxy`** — upstream pool with active TCP health checks and passive failure
  detection, round-robin or least-connections, **keep-alive connection pooling with
  stale-connection retry** (2.7× the relay throughput of connection-per-request),
  bounded relay.
- **TLS** — pass `-tls-cert`/`-tls-key` and the identical HTTP/1.1 engine serves
  https via a `tls.NewListener` wrap; transport swaps, protocol layer untouched.

## Measured performance

Apple M5 (10 cores), loopback, keep-alive, `pulsebench` warmup excluded, **0 errors in
every run**:

| Scenario | Throughput | p50 | p99 | p99.9 |
|---|---|---|---|---|
| `GET /healthz`, c=100, 200K reqs | **178,996 req/s** | 0.49 ms | 2.20 ms | 3.37 ms |
| Routed `GET /api/users/:id`, c=100, 200K reqs | 165,280 req/s | 0.42 ms | 3.30 ms | 7.44 ms |
| CPU-bound route (20K SHA-256 rounds), uncached, c=50 | 8,198 req/s | 4.18 ms | 36.25 ms | 47.18 ms |
| Same route, LRU+ETag cache | **159,886 req/s (19.5×)** | 0.26 ms | 1.24 ms | 1.96 ms |
| Through the reverse-proxy balancer, 2 upstreams, c=100, 200K reqs | **78,341 req/s** (pooled; 2.7× over conn-per-request) | 0.97 ms | 5.00 ms | 9.67 ms |
| Rate limiter: burst 200 @ 100 rps, 2,000 blasted | — | — | — | **202×200 / 1,798×429** |

Reproduce with:

```bash
go build -o bin/pulsehttp ./cmd/pulsehttp && go build -o bin/pulsebench ./cmd/pulsebench
./bin/pulsehttp -addr 127.0.0.1:8080 -rate 0 -cache 0 -quiet
./bin/pulsebench -url 127.0.0.1:8080/healthz -c 100 -n 200000 -warmup 5000
```

## Try the status codes yourself

```bash
./bin/pulsehttp -addr 127.0.0.1:8080

curl -i localhost:8080/api/users/42                                  # 200
curl -i localhost:8080/api/users/42 -X DELETE                        # 405 + Allow
curl -i localhost:8080/nope                                          # 404
curl -i localhost:8080/admin/stats                                   # 401 + WWW-Authenticate
curl -i localhost:8080/admin/stats -H 'Authorization: Bearer user-alpha-token'   # 403
curl -i localhost:8080/admin/stats -H 'Authorization: Bearer admin-omega-token'  # 200
curl -i localhost:8080/api/heavy                                     # 200, X-Cache: MISS + ETag
curl -i localhost:8080/api/heavy                                     # 200, X-Cache: HIT
curl -i localhost:8080/api/heavy -H 'If-None-Match: <etag>'          # 304
curl -i localhost:8080/boom                                          # 500, connection survives
seq 1 2000 | xargs -P8 -I{} curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/healthz | sort | uniq -c   # 200s then 429s
```

Proxy mode:

```bash
./bin/pulsehttp -addr 127.0.0.1:9090 -mode proxy \
    -upstreams 127.0.0.1:8081,127.0.0.1:8082 -strategy round-robin
```

## Verification

34 tests, all driving the real stack (protocol tests speak raw TCP to a live listener):

```bash
go test ./...
```

Covered: keep-alive reuse, `Connection: close`, HEAD body suppression, chunked
decoding, every 4xx/5xx edge in the table above, panic recovery, graceful-drain
in-flight completion, LRU eviction order, TTL expiry, ETag revalidation, per-client
bucket isolation, round-robin distribution, upstream-death failover, and
all-upstreams-down 503.

## Scope decisions

- **HTTP/1.1 by design.** The project's goal is owning one protocol completely —
  parsing, framing, and every status-code boundary — on a raw socket. HTTP/2's
  binary framing/HPACK is a different protocol, deliberately out of scope rather
  than half-implemented. TLS is supported (`-tls-cert`/`-tls-key`).
- **Buffered bodies, not streamed — deliberately.** Responses materialize in
  memory (bounded by limits) because full-response buffering is what lets the
  cache, ETag computation, metrics, and Content-Length framing observe complete
  exchanges. Streaming is a different design point with different middleware
  semantics, not a missing feature.
