# Plan 002: Refuse to expose cache admin endpoints when auth is disabled

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report, do not improvise. When done, update the status row for this plan
> in `plans/README.md`.
>
> **Drift check (run first)**: `git diff --stat 1825c44..HEAD -- cmd/gateway/`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: security
- **Planned at**: commit `1825c44`, 2026-07-01

## Why this matters

The gateway deliberately runs unauthenticated when no MySQL DSN is configured
(a documented development mode, logged loudly at startup). But the cache admin
endpoints inherit that mode silently: if Redis is configured and MySQL is not,
`POST /cache/delete` is reachable by anyone and can evict any tenant's cache
entries. The proxy endpoints being open in dev mode is a stated tradeoff; the
admin endpoints being open is an accident, and the code comment ("Protected by
the same auth as the proxy when configured") shows the intent was protection.
The fix: when auth is not configured, do not mount the cache admin endpoints
at all, and say so in the log.

## Current state

- `cmd/gateway/main.go` wires everything. Auth middleware is nil without MySQL
  (`cmd/gateway/main.go:98-109`):

```go
var authMW func(http.Handler) http.Handler
if cfg.Storage.MySQLDSN != "" {
    ...
    authMW = auth.New(st, log, cfg.Security.AuthCacheTTL).Middleware
    ...
} else {
    log.Warn("AUTH DISABLED: MYSQL_DSN not configured, /v1/messages is unauthenticated (development only)")
}
```

The cache admin mount at `cmd/gateway/main.go:207-221` applies auth only when
present:

```go
// Operational cache endpoints (health probe and delete-by-key) when the
// cache is enabled. Protected by the same auth as the proxy when configured.
if respCache != nil {
    admin := proxy.NewCacheAdmin(respCache, log)
    routeFns = append(routeFns, func(r chi.Router) {
        r.Group(func(gr chi.Router) {
            if authMW != nil {
                gr.Use(authMW)
            }
            gr.Get("/cache/ping", admin.Ping)
            gr.Post("/cache/delete", admin.Delete)
        })
    })
    log.Info("mounted cache admin endpoints", slog.String("paths", "/cache/ping, /cache/delete"))
}
```

- The admin handlers live in `internal/proxy/cache_admin.go` (`NewCacheAdmin`,
  `Ping`, `Delete`). They do no auth of their own.
- Test conventions: `cmd/gateway/main_test.go` already starts the full server
  via `run(cfgPath)` with a temp YAML config, waits on `/readyz` with
  `waitForReady`, and shuts down via SIGTERM (see
  `TestRun_StartAndGracefulShutdown` at `cmd/gateway/main_test.go:132-162`,
  and `freePort` below it). Model the new test on it.
- Config YAML shape (from `configs/config.example.yaml`): `storage.redis_addr_env`
  names an env var holding the Redis address, e.g.

```yaml
storage:
  redis_addr_env: REDIS_ADDR
```

- `github.com/alicebob/miniredis/v2` is already a dependency; use
  `miniredis.RunT(t)` to get a real-enough Redis address for the test.

## Commands you will need

| Purpose | Command | Expected on success |
|---------|---------|---------------------|
| Build   | `make build` | exit 0 |
| Focused tests | `go test ./cmd/gateway/ -race` | ok, all pass |
| Full tests | `make test` | exit 0 |
| Lint    | `make lint` | exit 0 |
| Em dash check | `grep -rPn "\xe2\x80\x94" cmd/gateway/` | no output |

## Scope

**In scope** (the only files you should modify):

- `cmd/gateway/main.go` (the cache admin mount block only)
- `cmd/gateway/main_test.go` (add one test)

**Out of scope** (do NOT touch, even though they look related):

- The unauthenticated proxy endpoints in dev mode: documented tradeoff, keep.
- `internal/proxy/cache_admin.go`: handlers stay auth-agnostic; enforcement
  belongs at the mount.
- Adding a separate admin credential or config flag: a bigger design change,
  not this plan.

## Git workflow

- Branch: `advisor/002-cache-admin-auth`
- Commit style: imperative, under 72 chars, e.g.
  `security: mount cache admin endpoints only when auth is enabled`
- All work lands via PR into `main`.

## Steps

### Step 1: Gate the mount on authMW

In `cmd/gateway/main.go`, change the cache admin block so the endpoints are
mounted only when `authMW != nil`. When auth is disabled, log a warning
instead, for example:

```go
if respCache != nil {
    if authMW == nil {
        log.Warn("cache admin endpoints not mounted: auth disabled (MYSQL_DSN not configured)")
    } else {
        admin := proxy.NewCacheAdmin(respCache, log)
        routeFns = append(routeFns, func(r chi.Router) {
            r.Group(func(gr chi.Router) {
                gr.Use(authMW)
                gr.Get("/cache/ping", admin.Ping)
                gr.Post("/cache/delete", admin.Delete)
            })
        })
        log.Info("mounted cache admin endpoints", slog.String("paths", "/cache/ping, /cache/delete"))
    }
}
```

Update the comment above the block: the endpoints require auth and are not
available in the unauthenticated development mode.

**Verify**: `make build` exits 0.

### Step 2: Add a regression test

In `cmd/gateway/main_test.go`, add `TestRun_CacheAdminNotMountedWithoutAuth`:

1. Start miniredis: `mr := miniredis.RunT(t)`.
2. `t.Setenv("REDIS_ADDR", mr.Addr())`.
3. Write a temp config like `TestRun_StartAndGracefulShutdown` does, but with
   a `storage` section: `storage:\n  redis_addr_env: REDIS_ADDR\n` (no MySQL).
4. Start `run(cfgPath)` in a goroutine, wait for `/readyz`.
5. `GET /cache/ping` and assert the status is 404 (endpoint not mounted).
6. SIGTERM and wait for clean shutdown, exactly like the existing test.

**Verify**: `go test ./cmd/gateway/ -race` passes, including the new test.
Reverting Step 1 should make the new test fail with a 200 from `/cache/ping`
(confirm once, then re-apply).

### Step 3: Full gates

**Verify**: `make build && make test && make lint` all exit 0, and the em dash
grep prints nothing.

## Test plan

- New test: with Redis configured and MySQL absent, `/cache/ping` returns 404.
- Existing tests must stay green; in particular any test that exercises the
  authenticated mount path.
- Pattern: `TestRun_StartAndGracefulShutdown` in `cmd/gateway/main_test.go`.

## Done criteria

- [ ] `make build`, `make test`, `make lint` exit 0
- [ ] With MySQL unset and Redis set, the gateway logs the warning and serves
      404 on `/cache/ping` and `/cache/delete`
- [ ] `git status` shows changes only to the two in-scope files
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- The mount block in `cmd/gateway/main.go` does not match the excerpt above.
- Any existing test asserts that cache admin endpoints work without auth
  (that would mean the open behavior is intentional somewhere; needs review).
- The `run()`-based test cannot reach `/readyz` within the timeout on CI.

## Maintenance notes

- If a dedicated admin credential or role system is added later, this mount
  gate is the place to swap in the stronger check.
- Reviewer should confirm docs do not promise cache admin endpoints in the
  unauthenticated dev mode (check `docs/local-development.md` and README; if
  they do, the docs need a one-line update in the same PR).
