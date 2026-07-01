# Plan 005: Extract the duplicated SSE usage scanner into a shared helper

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report, do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**: `git diff --stat 1825c44..HEAD -- internal/provider/`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: tech-debt
- **Planned at**: commit `1825c44`, 2026-07-01

## Why this matters

`internal/provider/anthropic/usage.go` and `internal/provider/openai/usage.go`
contain byte-for-byte identical SSE plumbing: the same `pending []byte`
buffering, the same `Write` loop splitting on newlines, the same `processLine`
prefix handling for `data:` lines, `[DONE]`, and blank payloads. Only the JSON
payload interpretation differs. Any SSE-level fix (chunk boundary handling,
CRLF trimming, a new provider) must currently be made twice and can drift.
Extracting the shared scaffolding into one helper in `internal/provider`
removes the duplication while keeping provider-specific usage extraction where
it belongs.

## Current state

- `internal/provider/anthropic/usage.go`: `sseUsageScanner` with fields
  `pending []byte; usage provider.Usage`. Its `Write` (lines ~39-52):

```go
func (s *sseUsageScanner) Write(p []byte) (int, error) {
    s.pending = append(s.pending, p...)
    for {
        i := bytes.IndexByte(s.pending, '\n')
        if i < 0 {
            break
        }
        line := s.pending[:i]
        s.pending = s.pending[i+1:]
        s.processLine(line)
    }
    return len(p), nil
}
```

Its `processLine` trims `\r`, requires the `data:` prefix, trims the payload,
skips empty and `[DONE]`, then unmarshals into an Anthropic-specific event
struct (message_start carries input and cache tokens, message_delta carries
the running output count).

- `internal/provider/openai/usage.go`: identical `Write` and identical
  `processLine` scaffolding; the payload handling unmarshals
  `{"usage": {...}}` and keeps the last non-nil usage chunk.
- Both scanners are created by their package's `NewUsageScanner` and consumed
  as `io.Writer` by the proxy relay tee. There is a `provider.UsageScanner`
  interface (or equivalent contract) in `internal/provider/provider.go`;
  check its exact shape before starting.
- Note: `internal/provider/translate/response.go` has its own `sseScanner`,
  a pull-based reader used during shape translation. It is a different
  mechanism and is explicitly out of scope.
- Tests: `internal/provider/anthropic/usage_test.go` and
  `internal/provider/openai/usage_test.go` cover both scanners, including
  chunk-boundary behavior. They must pass unchanged.
- Conventions: comments are complete sentences, no em dashes.

## Commands you will need

| Purpose | Command | Expected on success |
|---------|---------|---------------------|
| Build   | `make build` | exit 0 |
| Focused tests | `go test ./internal/provider/... -race` | ok, all pass |
| Full tests | `make test` | exit 0 |
| Lint    | `make lint` | exit 0 |
| Em dash check | `grep -rPn "\xe2\x80\x94" internal/provider/` | no output |

## Scope

**In scope** (the only files you should create or modify):

- `internal/provider/sse.go` (create)
- `internal/provider/sse_test.go` (create)
- `internal/provider/anthropic/usage.go`
- `internal/provider/openai/usage.go`

**Out of scope** (do NOT touch, even though they look related):

- `internal/provider/translate/response.go` and its `sseScanner`: pull-based
  reader with different semantics; consolidating it is a separate decision.
- The `NewUsageScanner` constructors' signatures and the scanners' public
  behavior as `io.Writer`s: the proxy depends on them.
- Usage extraction logic (which fields, which events): behavior-preserving
  refactor only.

## Git workflow

- Branch: `advisor/005-shared-sse-scanner`
- Commit style: imperative, under 72 chars, e.g.
  `provider: extract shared SSE line scanner from usage scanners`
- All work lands via PR into `main`.

## Steps

### Step 1: Create the shared scanner

Add `internal/provider/sse.go` (package `provider`) with an SSE line scanner
that owns the shared scaffolding and delegates payloads to a callback:

```go
// SSEPayloadScanner is an io.Writer that reassembles SSE lines from arbitrary
// chunk boundaries and invokes onPayload for each non-empty data payload,
// skipping the [DONE] sentinel.
type SSEPayloadScanner struct {
    pending   []byte
    onPayload func(payload []byte)
}

func NewSSEPayloadScanner(onPayload func([]byte)) *SSEPayloadScanner { ... }

func (s *SSEPayloadScanner) Write(p []byte) (int, error) { ... }
```

Move the `Write` loop and the shared parts of `processLine` (CR trim, `data:`
prefix check, payload trim, empty and `[DONE]` skip) here verbatim.

Add `internal/provider/sse_test.go` covering: payload split across multiple
`Write` calls, CRLF line endings, non-data lines ignored, `[DONE]` and empty
payloads skipped, multiple events in one chunk. Port the relevant cases from
the two existing usage tests rather than inventing new fixtures.

**Verify**: `go test ./internal/provider/ -race` passes with the new tests.

### Step 2: Rewrite the anthropic scanner on top of it

In `internal/provider/anthropic/usage.go`, replace the `pending` buffer,
`Write`, and the scaffolding half of `processLine` with an embedded
`*provider.SSEPayloadScanner` whose callback contains the existing
Anthropic-specific unmarshal and usage bookkeeping, unchanged. Keep
`NewUsageScanner`'s signature and the `Usage()` accessor identical.

**Verify**: `go test ./internal/provider/anthropic/ -race` passes unchanged.

### Step 3: Rewrite the openai scanner the same way

Same change in `internal/provider/openai/usage.go`.

**Verify**: `go test ./internal/provider/openai/ -race` passes unchanged.

### Step 4: Full gates

**Verify**: `make build && make test && make lint` exit 0; em dash grep
prints nothing; `grep -n "pending \[\]byte" internal/provider/anthropic/usage.go internal/provider/openai/usage.go`
returns no matches (the duplicated buffer is gone from both).

## Test plan

- New: `internal/provider/sse_test.go` (cases listed in Step 1).
- Existing: both `usage_test.go` files pass without modification; they are
  the behavioral contract for this refactor.
- Pattern: match the table-driven style of the existing usage tests.

## Done criteria

- [ ] `make build`, `make test`, `make lint` exit 0
- [ ] `internal/provider/sse.go` exists and both usage scanners delegate to it
- [ ] Neither usage.go contains its own line-splitting loop
- [ ] Both existing usage test files unchanged (`git diff --stat` confirms)
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- The two `processLine` implementations turn out to differ in any way beyond
  the payload unmarshal (diff them first; if the scaffolding differs, the
  "identical" premise is wrong and needs review).
- Keeping `NewUsageScanner` signatures intact proves impossible without
  touching `internal/proxy/`.
- Any existing usage test fails.

## Maintenance notes

- A future provider adapter (or the translate module, if consolidation is
  ever wanted) should build on `SSEPayloadScanner` instead of hand-rolling
  line handling.
- Reviewer should confirm chunk-boundary behavior is covered in the new
  shared test, since that is the subtle part the duplication protected.
