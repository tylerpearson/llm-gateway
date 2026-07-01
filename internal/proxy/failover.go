// This file implements upstream failover for the proxy: bounded retries with
// backoff across an ordered candidate list, and a Redis-shared circuit breaker
// that ejects a repeatedly failing target for a cooldown. Failover happens
// strictly before the response is relayed to the client (the commit point);
// once relaying begins a streamed response cannot be retried, so a mid-stream
// upstream drop surfaces to the client as a truncated response, unchanged from
// the non-failover path.
package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/tylerpearson/llm-gateway/internal/provider"
	"github.com/tylerpearson/llm-gateway/internal/router"
)

// ResiliencePolicy governs retry and failover behavior. The zero value makes a
// single attempt against the primary target with no retries and no failover on
// upstream status, matching the pre-failover behavior.
type ResiliencePolicy struct {
	// MaxRetries is the number of extra attempts per candidate on a retryable
	// failure, on top of the first attempt.
	MaxRetries int
	// RetryBackoff is the base delay before a retry; it grows exponentially with
	// jitter across attempts.
	RetryBackoff time.Duration
	// RequestTimeout bounds a single attempt's wait for the upstream response.
	// It never cuts off an in-progress streamed body: the timer is stopped as
	// soon as the response headers arrive.
	RequestTimeout time.Duration
	// retryable is the set of upstream status codes worth retrying or failing
	// over on.
	retryable map[int]bool
}

// NewResiliencePolicy builds a policy, turning the retryable status slice into a
// set for O(1) lookup.
func NewResiliencePolicy(maxRetries int, backoff, requestTimeout time.Duration, retryableStatus []int) ResiliencePolicy {
	set := make(map[int]bool, len(retryableStatus))
	for _, s := range retryableStatus {
		set[s] = true
	}
	return ResiliencePolicy{
		MaxRetries:     maxRetries,
		RetryBackoff:   backoff,
		RequestTimeout: requestTimeout,
		retryable:      set,
	}
}

// isRetryable reports whether an upstream status should trigger a retry or
// failover rather than being relayed to the caller.
func (p ResiliencePolicy) isRetryable(status int) bool {
	return p.retryable[status]
}

// Breaker gates traffic to a target that has been failing, giving it a cooldown
// to recover. Implementations must be safe for concurrent use and must fail open
// (allow) on their own errors so a breaker outage never blocks live traffic.
type Breaker interface {
	// Allow reports whether requests to target may proceed.
	Allow(ctx context.Context, target string) bool
	// RecordSuccess clears the failure state for target.
	RecordSuccess(ctx context.Context, target string)
	// RecordFailure records a failure for target and may open the breaker.
	RecordFailure(ctx context.Context, target string)
}

// NoopBreaker always allows and records nothing. It is the default when failover
// is not wired.
type NoopBreaker struct{}

// Allow always returns true.
func (NoopBreaker) Allow(context.Context, string) bool { return true }

// RecordSuccess does nothing.
func (NoopBreaker) RecordSuccess(context.Context, string) {}

// RecordFailure does nothing.
func (NoopBreaker) RecordFailure(context.Context, string) {}

var _ Breaker = NoopBreaker{}

// RedisBreaker is a Redis-backed circuit breaker shared across gateway replicas.
// It counts consecutive failures per target and, once the threshold is reached,
// sets a cooldown marker that blocks the target until it expires. State lives in
// Redis so every replica sees the same open/closed decision.
type RedisBreaker struct {
	rdb       redis.Cmdable
	threshold int
	cooldown  time.Duration
	log       *slog.Logger
}

// NewRedisBreaker connects to Redis and returns a breaker. It mirrors the
// connection handling of the ratelimit package.
func NewRedisBreaker(addr string, threshold int, cooldown time.Duration, log *slog.Logger) (*RedisBreaker, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, err
	}
	return NewRedisBreakerWithClient(rdb, threshold, cooldown, log), nil
}

// NewRedisBreakerWithClient builds a breaker over an existing client (used in
// tests).
func NewRedisBreakerWithClient(rdb redis.Cmdable, threshold int, cooldown time.Duration, log *slog.Logger) *RedisBreaker {
	if threshold <= 0 {
		threshold = 5
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &RedisBreaker{rdb: rdb, threshold: threshold, cooldown: cooldown, log: log}
}

// Close releases the client if it owns one.
func (b *RedisBreaker) Close() error {
	if c, ok := b.rdb.(*redis.Client); ok {
		return c.Close()
	}
	return nil
}

func breakerFailKey(target string) string { return "brk:fail:" + target }
func breakerOpenKey(target string) string { return "brk:open:" + target }

// Allow returns false while the target's cooldown marker exists. On a Redis
// error it fails open.
func (b *RedisBreaker) Allow(ctx context.Context, target string) bool {
	n, err := b.rdb.Exists(ctx, breakerOpenKey(target)).Result()
	if err != nil {
		b.log.Warn("breaker exists check failed, failing open", slog.String("target", target), slog.Any("error", err))
		return true
	}
	return n == 0
}

// RecordSuccess clears the consecutive-failure counter.
func (b *RedisBreaker) RecordSuccess(ctx context.Context, target string) {
	if err := b.rdb.Del(ctx, breakerFailKey(target)).Err(); err != nil {
		b.log.Warn("breaker clear failed", slog.String("target", target), slog.Any("error", err))
	}
}

// RecordFailure increments the consecutive-failure counter and opens the breaker
// for the cooldown once it reaches the threshold, resetting the counter.
func (b *RedisBreaker) RecordFailure(ctx context.Context, target string) {
	failKey := breakerFailKey(target)
	n, err := b.rdb.Incr(ctx, failKey).Result()
	if err != nil {
		b.log.Warn("breaker incr failed", slog.String("target", target), slog.Any("error", err))
		return
	}
	// Bound the failure counter's lifetime so isolated, spread-out failures do
	// not accumulate into an ejection.
	_ = b.rdb.Expire(ctx, failKey, b.cooldown).Err()
	if n >= int64(b.threshold) {
		if err := b.rdb.Set(ctx, breakerOpenKey(target), "1", b.cooldown).Err(); err != nil {
			b.log.Warn("breaker open failed", slog.String("target", target), slog.Any("error", err))
			return
		}
		_ = b.rdb.Del(ctx, failKey).Err()
		b.log.Warn("breaker opened", slog.String("target", target), slog.Duration("cooldown", b.cooldown))
	}
}

// dispatch tries the candidate targets in order and returns the first response
// that should be relayed to the client (a success or a non-retryable upstream
// status). For each candidate it makes up to MaxRetries+1 attempts on retryable
// failures. It returns the served target, the response to relay (nil when no
// candidate produced one), a cleanup func the caller must defer alongside
// closing the body, the last upstream status seen (0 if none), and an error
// describing why nothing could be relayed.
//
// The returned response's attempt context is left live so its streamed body can
// be read; cleanup cancels it once the caller is done. Failed attempts have
// their contexts and bodies cleaned up before the next attempt.
func (h *Handler) dispatch(ctx context.Context, reqID string, clientShape provider.Shape, stream bool, candidates []router.Target, body []byte, reqHeader http.Header) (router.Target, *provider.Response, func(), int, error) {
	noop := func() {}
	primary := candidates[0]
	var lastStatus int
	var lastErr error

	for i, target := range candidates {
		prov, ok := h.registry.Get(target.Provider)
		if !ok {
			lastErr = fmt.Errorf("provider %q is not available", target.Provider)
			continue
		}
		targetKey := target.Provider + "/" + target.Model
		if !h.breaker.Allow(ctx, targetKey) {
			h.setBreakerMetric(target, true)
			h.log.Warn("breaker open, skipping target",
				slog.String("request_id", reqID), slog.String("target", targetKey))
			lastErr = fmt.Errorf("breaker open for %s", targetKey)
			continue
		}
		h.setBreakerMetric(target, false)

		sendBody, err := buildBody(body, clientShape, target)
		if err != nil {
			lastErr = fmt.Errorf("translate request for %s: %w", targetKey, err)
			continue
		}

		attempts := 1 + h.policy.MaxRetries
		for a := 0; a < attempts; a++ {
			if a > 0 {
				if h.metrics != nil {
					h.metrics.IncUpstreamRetry(target.Provider)
				}
				if !h.backoff(ctx, a) {
					return primary, nil, noop, lastStatus, ctx.Err()
				}
			}

			resp, cancel, cerr := h.attempt(ctx, prov, &provider.Request{
				Model:  target.Model,
				Stream: stream,
				Raw:    sendBody,
				Header: passthroughHeaders(reqHeader),
			})

			if cerr != nil {
				cancel()
				if ctx.Err() != nil {
					// The client's context is done (disconnect or shutdown);
					// stop rather than churn through fallbacks.
					return primary, nil, noop, lastStatus, cerr
				}
				lastErr = cerr
				if h.metrics != nil {
					h.metrics.IncUpstreamError(target.Provider)
				}
				h.breaker.RecordFailure(ctx, targetKey)
				continue
			}

			if h.policy.isRetryable(resp.StatusCode) {
				_ = resp.Body.Close()
				cancel()
				lastStatus = resp.StatusCode
				lastErr = fmt.Errorf("upstream %s returned %d", targetKey, resp.StatusCode)
				h.breaker.RecordFailure(ctx, targetKey)
				continue
			}

			// Relayable response (success or a non-retryable upstream status).
			h.breaker.RecordSuccess(ctx, targetKey)
			if i > 0 && h.metrics != nil {
				h.metrics.IncFailover(primary.Provider, target.Provider)
			}
			return target, resp, cancel, resp.StatusCode, nil
		}
	}
	return primary, nil, noop, lastStatus, lastErr
}

// attempt makes a single upstream call under an attempt-scoped context. When
// RequestTimeout is set a timer cancels the context if the response headers do
// not arrive in time; the timer is stopped as soon as Complete returns so a
// streamed body is never cut off. The returned cancel func must be called: on a
// failed attempt immediately, on a relayed response after the body is drained.
func (h *Handler) attempt(ctx context.Context, prov provider.Provider, req *provider.Request) (*provider.Response, context.CancelFunc, error) {
	attemptCtx, cancel := context.WithCancel(ctx)
	var timer *time.Timer
	if h.policy.RequestTimeout > 0 {
		timer = time.AfterFunc(h.policy.RequestTimeout, cancel)
	}
	resp, err := prov.Complete(attemptCtx, req)
	if timer != nil {
		timer.Stop()
	}
	return resp, cancel, err
}

// maxBackoff caps a single retry delay so a large max_retries cannot overflow
// the exponential shift into a negative or zero duration (which would panic
// rand.Int64N) or into an absurdly long sleep.
const maxBackoff = 30 * time.Second

// backoff sleeps before a retry, growing the base delay exponentially with up to
// full jitter. It returns false if the context is canceled while waiting.
func (h *Handler) backoff(ctx context.Context, attempt int) bool {
	base := h.policy.RetryBackoff
	if base <= 0 {
		return ctx.Err() == nil
	}
	// Exponential growth: base * 2^(attempt-1), capped. The shift count is bounded
	// so it cannot exceed the width of time.Duration and wrap to a bad value.
	shift := attempt - 1
	if shift > 20 {
		shift = 20
	}
	d := base << shift
	if d <= 0 || d > maxBackoff {
		d = maxBackoff
	}
	// Full jitter in [0, d).
	d += time.Duration(rand.Int64N(int64(d)))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// setBreakerMetric records the breaker gauge for a target when metrics are on.
func (h *Handler) setBreakerMetric(target router.Target, open bool) {
	if h.metrics != nil {
		h.metrics.SetBreakerOpen(target.Provider, target.Model, open)
	}
}
