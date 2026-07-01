# Plan 001: Stop caching responses whose relay to the client was interrupted

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report, do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**: `git diff --stat 1825c44..HEAD -- internal/proxy/`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: bug
- **Planned at**: commit `1825c44`, 2026-07-01

## Why this matters

When a client disconnects mid-stream, `relay` stops copying as soon as the
write to the client fails, so the cache capture buffer holds only a prefix of
the upstream response. The cache store condition in `serve` checks the
truncation flag (which only trips on the size limit) and the upstream status,
but never the relay error. The partial body is therefore written to the Redis
cache and replayed as a complete response to every identical request in the
same tenant until the TTL expires. One flaky client can poison the cache for
its whole team. The fix is a one-line condition change plus tests that
simulate a client disconnect, a scenario that currently has zero coverage.

## Current state

- `internal/proxy/proxy.go` is the request pipeline. The relevant pieces:

The store condition at `internal/proxy/proxy.go:403-414` (note: no check of
`relayErr`, which was returned on line 394):

```go
usage, written, relayErr := h.relayResponse(w, resp, clientShape, served, meta.Stream, capture)
...
if h.cache != nil && capture != nil && !capture.truncated && resp.StatusCode >= 200 && resp.StatusCode < 300 {
    h.cache.Set(bgCtx, cacheKey, &cache.Entry{
        Status:      resp.StatusCode,
        ContentType: contentType(meta.Stream),
        Body:        capture.Bytes(),
        Usage:       usage,
    }, cc.ttl)
}
```

`relay` at `internal/proxy/proxy.go:636-659` returns immediately when the
client write fails, leaving the upstream body partially consumed:

```go
n, readErr := body.Read(buf)
if n > 0 {
    if _, writeErr := w.Write(buf[:n]); writeErr != nil {
        return total, writeErr
    }
    _, _ = scanner.Write(buf[:n])
    ...
```

The cross-shape path has the same defect: `relayResponse` returns the
translation error as the same third return value, and `flushWriter` has been
teeing partial events into `capture` all along.

`boundedBuffer.truncated` (bottom of `proxy.go`) only becomes true when the
size limit is exceeded, so it does not protect against partial capture.

- Conventions: no em dashes in comments, comments are complete sentences,
  table-driven tests with `httptest`. See `internal/proxy/proxy_cache_test.go`
  for the existing cache test setup (read it before step 2; it shows how the
  proxy handler is constructed with a cache for tests).

## Commands you will need

| Purpose | Command | Expected on success |
|---------|---------|---------------------|
| Build   | `make build` | exit 0 |
| Focused tests | `go test ./internal/proxy/ -race` | ok, all pass |
| Full tests | `make test` | exit 0, all pass |
| Lint    | `make lint` | exit 0 |
| Em dash check | `grep -rPn "\xe2\x80\x94" internal/proxy/` | no output |

## Scope

**In scope** (the only files you should modify):

- `internal/proxy/proxy.go` (the store condition only)
- `internal/proxy/proxy_cache_test.go` (add tests)

**Out of scope** (do NOT touch, even though they look related):

- `internal/proxy/relayResponse`, `relay`, `flushWriter`, `boundedBuffer`:
  their behavior is correct; only the store condition is wrong.
- `internal/provider/translate/response.go`: the discarded final-event write
  errors there were reviewed and judged acceptable (the last write error is
  returned).
- Post-response bookkeeping (limiter, attribution, logging): recording partial
  usage on disconnect is intentional; partial usage is better than none for
  attribution.

## Git workflow

- Branch: `advisor/001-relay-cache-poisoning`
- Commit style: imperative mood, under 72 chars, e.g.
  `proxy: skip cache store when response relay is interrupted`
- All work lands via PR into `main`; do not push unless the operator says so.

## Steps

### Step 1: Gate the cache store on a clean relay

In `internal/proxy/proxy.go`, change the store condition to require
`relayErr == nil`:

```go
if h.cache != nil && capture != nil && relayErr == nil && !capture.truncated && resp.StatusCode >= 200 && resp.StatusCode < 300 {
```

Extend the comment above it to state that an interrupted relay (client
disconnect or upstream read failure) must not be cached because the capture
buffer holds only a prefix of the response.

**Verify**: `make build` exits 0.

### Step 2: Add a client-disconnect cache test

In `internal/proxy/proxy_cache_test.go`, add a test that:

1. Sets up a handler with a cache exactly like the existing tests in that file.
2. Configures a mock upstream that streams a multi-chunk SSE response (reuse
   the `anthropicSSE` fixture pattern from `internal/proxy/proxy_test.go`).
3. Invokes the handler with a `http.ResponseWriter` wrapper that returns an
   error from `Write` after the first successful write. The proxy tests call
   `h.Messages(rec, req)` directly, so a wrapper struct around
   `*httptest.ResponseRecorder` implementing `Header`, `Write` (failing after
   N bytes), and `WriteHeader` is sufficient.
4. Asserts the response relay was interrupted and then sends the identical
   request again with a normal recorder, asserting `x-llm-cache: miss` (the
   partial response was not stored).

Also add the inverse regression case if not already covered: an uninterrupted
streamed request followed by an identical request that gets `x-llm-cache: hit`.

**Verify**: `go test ./internal/proxy/ -race` passes, including the new tests.
Temporarily reverting the Step 1 condition should make the new disconnect test
fail (run it once to confirm the test actually bites, then re-apply the fix).

### Step 3: Full gates

**Verify**: `make build && make test && make lint` all exit 0, and
`grep -rPn "\xe2\x80\x94" internal/proxy/` prints nothing.

## Test plan

- New test: interrupted relay does not populate the cache (the bug this plan
  fixes).
- New or confirmed test: successful relay still populates the cache (guard
  against over-tightening the condition).
- Pattern: model after the existing tests in
  `internal/proxy/proxy_cache_test.go`.

## Done criteria

- [ ] `make build`, `make test`, `make lint` exit 0
- [ ] The store condition in `internal/proxy/proxy.go` includes `relayErr == nil`
- [ ] A test exists that fails when the `relayErr == nil` clause is removed
- [ ] `git status` shows changes only to the two in-scope files
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- The store condition at `internal/proxy/proxy.go` does not match the excerpt
  above (drift since planning).
- The handler cannot be driven with a failing writer because the signature of
  `h.Messages` or the test harness differs from what `proxy_test.go` shows.
- Fixing the condition breaks any existing cache test (that would mean an
  existing test depends on caching partial bodies, which needs human review).

## Maintenance notes

- If a future change makes `relay` drain the upstream body after a client
  write failure (to keep the connection reusable), the capture buffer would
  then hold the full body and this condition could be revisited.
- Reviewer should scrutinize that the cross-shape (translated) path is covered
  by the same gate; it is, because `relayResponse` returns translation write
  errors through the same third return value.
