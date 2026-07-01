package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/tylerpearson/llm-gateway/internal/config"
	"github.com/tylerpearson/llm-gateway/internal/provider"
	"github.com/tylerpearson/llm-gateway/internal/provider/anthropic"
	"github.com/tylerpearson/llm-gateway/internal/router"
)

// statusServer returns an httptest server whose handler responds according to
// next(), which yields the (status, body) for each successive request.
func statusServer(t *testing.T, next func(n int64) (int, string)) (*httptest.Server, *int64) {
	t.Helper()
	var count int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&count, 1)
		status, body := next(n)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &count
}

// failoverRouting builds a two-target alias: primary provider/model with a single
// fallback.
func failoverRouting(primaryProv, primaryModel, fbProv, fbModel string) config.Routing {
	return config.Routing{
		DefaultAlias: "default",
		Aliases: map[string]config.Route{
			"default": {
				Provider:  primaryProv,
				Model:     primaryModel,
				Fallbacks: []config.Route{{Provider: fbProv, Model: fbModel}},
			},
		},
	}
}

func newFailoverHandler(t *testing.T, providers provider.Registry, routing config.Routing, breaker Breaker, policy ResiliencePolicy) *Handler {
	t.Helper()
	shapes := map[string]provider.Shape{}
	for n, p := range providers {
		shapes[n] = p.Shape()
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return New(providers, router.New(routing, shapes), log, WithFailover(breaker, policy))
}

// fakeBreaker gates targets from an allowlist and records calls, so tests can
// assert breaker interaction without Redis.
type fakeBreaker struct {
	blocked   map[string]bool
	successes []string
	failures  []string
}

func (f *fakeBreaker) Allow(_ context.Context, target string) bool { return !f.blocked[target] }
func (f *fakeBreaker) RecordSuccess(_ context.Context, target string) {
	f.successes = append(f.successes, target)
}
func (f *fakeBreaker) RecordFailure(_ context.Context, target string) {
	f.failures = append(f.failures, target)
}

func TestDispatch_FailsOverToFallbackOnRetryableStatus(t *testing.T) {
	primaryUp, primaryHits := statusServer(t, func(int64) (int, string) { return http.StatusTooManyRequests, `{"error":"rate"}` })
	fbUp, fbHits := statusServer(t, func(int64) (int, string) { return http.StatusOK, `{"id":"from_fallback"}` })

	reg := provider.Registry{
		"primary":  anthropic.New("primary", primaryUp.URL, "k1"),
		"fallback": anthropic.New("fallback", fbUp.URL, "k2"),
	}
	policy := NewResiliencePolicy(0, time.Millisecond, 0, []int{http.StatusTooManyRequests})
	br := &fakeBreaker{}
	h := newFailoverHandler(t, reg, failoverRouting("primary", "m1", "fallback", "m2"), br, policy)

	rec := post(h, "/v1/messages", `{"model":"default","messages":[]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"id":"from_fallback"}` {
		t.Errorf("body = %q, want fallback body", got)
	}
	if *primaryHits != 1 || *fbHits != 1 {
		t.Errorf("hits: primary=%d fallback=%d, want 1 and 1", *primaryHits, *fbHits)
	}
	if len(br.failures) != 1 || br.failures[0] != "primary/m1" {
		t.Errorf("breaker failures = %v, want [primary/m1]", br.failures)
	}
	if len(br.successes) != 1 || br.successes[0] != "fallback/m2" {
		t.Errorf("breaker successes = %v, want [fallback/m2]", br.successes)
	}
}

func TestDispatch_RetriesSameCandidateThenSucceeds(t *testing.T) {
	up, hits := statusServer(t, func(n int64) (int, string) {
		if n == 1 {
			return http.StatusServiceUnavailable, `{"error":"down"}`
		}
		return http.StatusOK, `{"id":"ok"}`
	})
	reg := provider.Registry{"primary": anthropic.New("primary", up.URL, "k")}
	routing := config.Routing{
		DefaultAlias: "default",
		Aliases:      map[string]config.Route{"default": {Provider: "primary", Model: "m1"}},
	}
	policy := NewResiliencePolicy(2, time.Millisecond, 0, []int{http.StatusServiceUnavailable})
	h := newFailoverHandler(t, reg, routing, NoopBreaker{}, policy)

	rec := post(h, "/v1/messages", `{"model":"default","messages":[]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if *hits != 2 {
		t.Errorf("upstream hits = %d, want 2 (one retry)", *hits)
	}
}

func TestDispatch_AllCandidatesFailSurfacesLastStatus(t *testing.T) {
	primaryUp, _ := statusServer(t, func(int64) (int, string) { return http.StatusBadGateway, `{}` })
	fbUp, _ := statusServer(t, func(int64) (int, string) { return http.StatusServiceUnavailable, `{}` })
	reg := provider.Registry{
		"primary":  anthropic.New("primary", primaryUp.URL, "k1"),
		"fallback": anthropic.New("fallback", fbUp.URL, "k2"),
	}
	policy := NewResiliencePolicy(0, time.Millisecond, 0, []int{http.StatusBadGateway, http.StatusServiceUnavailable})
	h := newFailoverHandler(t, reg, failoverRouting("primary", "m1", "fallback", "m2"), NoopBreaker{}, policy)

	rec := post(h, "/v1/messages", `{"model":"default","messages":[]}`)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (last upstream status surfaced)", rec.Code)
	}
}

func TestDispatch_BreakerOpenSkipsTarget(t *testing.T) {
	primaryUp, primaryHits := statusServer(t, func(int64) (int, string) { return http.StatusOK, `{"id":"primary"}` })
	fbUp, fbHits := statusServer(t, func(int64) (int, string) { return http.StatusOK, `{"id":"fallback"}` })
	reg := provider.Registry{
		"primary":  anthropic.New("primary", primaryUp.URL, "k1"),
		"fallback": anthropic.New("fallback", fbUp.URL, "k2"),
	}
	policy := NewResiliencePolicy(0, time.Millisecond, 0, []int{http.StatusServiceUnavailable})
	br := &fakeBreaker{blocked: map[string]bool{"primary/m1": true}}
	h := newFailoverHandler(t, reg, failoverRouting("primary", "m1", "fallback", "m2"), br, policy)

	rec := post(h, "/v1/messages", `{"model":"default","messages":[]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"id":"fallback"}` {
		t.Errorf("body = %q, want fallback (primary breaker open)", got)
	}
	if *primaryHits != 0 {
		t.Errorf("primary hits = %d, want 0 (breaker open)", *primaryHits)
	}
	if *fbHits != 1 {
		t.Errorf("fallback hits = %d, want 1", *fbHits)
	}
}

// TestDispatch_NonRetryableStatusRelayedVerbatim confirms a client error is
// relayed as is and not failed over, even with a fallback available.
func TestDispatch_NonRetryableStatusRelayedVerbatim(t *testing.T) {
	primaryUp, primaryHits := statusServer(t, func(int64) (int, string) { return http.StatusBadRequest, `{"error":"bad input"}` })
	fbUp, fbHits := statusServer(t, func(int64) (int, string) { return http.StatusOK, `{"id":"fallback"}` })
	reg := provider.Registry{
		"primary":  anthropic.New("primary", primaryUp.URL, "k1"),
		"fallback": anthropic.New("fallback", fbUp.URL, "k2"),
	}
	policy := NewResiliencePolicy(2, time.Millisecond, 0, []int{http.StatusServiceUnavailable})
	h := newFailoverHandler(t, reg, failoverRouting("primary", "m1", "fallback", "m2"), NoopBreaker{}, policy)

	rec := post(h, "/v1/messages", `{"model":"default","messages":[]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 relayed verbatim", rec.Code)
	}
	if got := rec.Body.String(); got != `{"error":"bad input"}` {
		t.Errorf("body = %q, want verbatim upstream error", got)
	}
	if *primaryHits != 1 || *fbHits != 0 {
		t.Errorf("hits: primary=%d fallback=%d, want 1 and 0 (no failover on 4xx)", *primaryHits, *fbHits)
	}
}

func newRedisBreaker(t *testing.T, threshold int, cooldown time.Duration) (*RedisBreaker, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewRedisBreakerWithClient(client, threshold, cooldown, log), mr
}

func TestRedisBreaker_OpensAfterThresholdAndRecovers(t *testing.T) {
	br, mr := newRedisBreaker(t, 3, 30*time.Second)
	ctx := context.Background()
	const target = "anthropic/claude"

	if !br.Allow(ctx, target) {
		t.Fatal("expected fresh target to be allowed")
	}
	// Below threshold: still allowed.
	br.RecordFailure(ctx, target)
	br.RecordFailure(ctx, target)
	if !br.Allow(ctx, target) {
		t.Fatal("expected target allowed below threshold")
	}
	// Reaching the threshold opens the breaker.
	br.RecordFailure(ctx, target)
	if br.Allow(ctx, target) {
		t.Fatal("expected breaker open at threshold")
	}
	// After the cooldown elapses the target recovers.
	mr.FastForward(31 * time.Second)
	if !br.Allow(ctx, target) {
		t.Fatal("expected breaker closed after cooldown")
	}
}

func TestRedisBreaker_SuccessClearsFailureCount(t *testing.T) {
	br, _ := newRedisBreaker(t, 2, 30*time.Second)
	ctx := context.Background()
	const target = "openai/gpt-4o"

	br.RecordFailure(ctx, target)
	br.RecordSuccess(ctx, target) // resets the streak
	br.RecordFailure(ctx, target) // count is 1, not 2
	if !br.Allow(ctx, target) {
		t.Fatal("expected target still allowed: success reset the failure streak")
	}
}
