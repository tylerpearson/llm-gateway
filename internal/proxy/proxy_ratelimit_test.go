package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tylerpearson/llm-gateway/internal/auth"
	"github.com/tylerpearson/llm-gateway/internal/cache"
	"github.com/tylerpearson/llm-gateway/internal/provider"
	"github.com/tylerpearson/llm-gateway/internal/provider/anthropic"
	"github.com/tylerpearson/llm-gateway/internal/ratelimit"
	"github.com/tylerpearson/llm-gateway/internal/router"
)

type fakeLimiter struct {
	decision ratelimit.Decision
	recorded atomic.Bool
}

func (f *fakeLimiter) Check(context.Context, ratelimit.Identity) ratelimit.Decision {
	return f.decision
}
func (f *fakeLimiter) RecordUsage(context.Context, ratelimit.Identity, int, float64) {
	f.recorded.Store(true)
}

func handlerWithLimiter(t *testing.T, upstreamURL string, fl *fakeLimiter) *Handler {
	t.Helper()
	reg := provider.Registry{"anthropic": anthropic.New("anthropic", upstreamURL, "k")}
	shapes := map[string]provider.Shape{"anthropic": provider.ShapeAnthropic}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(reg, router.New(routingTo("anthropic", "claude-haiku-4-5-20251001"), shapes), log, WithRateLimit(fl))
}

// withPrincipal attaches an authenticated identity so the limiter path runs.
func postAuthed(h *Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	ctx := auth.WithPrincipal(req.Context(), &auth.Principal{KeyID: "k1", TeamID: "t1"})
	rec := httptest.NewRecorder()
	h.Messages(rec, req.WithContext(ctx))
	return rec
}

func TestRateLimit_HardRejects(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, anthropicSSE)
	}))
	defer upstream.Close()

	fl := &fakeLimiter{decision: ratelimit.Decision{Allowed: false, Exceeded: []string{"key:requests_per_min"}}}
	h := handlerWithLimiter(t, upstream.URL, fl)
	rec := postAuthed(h, `{"model":"claude-haiku-4-5-20251001","stream":true}`)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("x-llm-limit") != "key:requests_per_min" {
		t.Errorf("x-llm-limit = %q", rec.Header().Get("x-llm-limit"))
	}
	if calls.Load() != 0 {
		t.Errorf("upstream called %d times, want 0 (rejected)", calls.Load())
	}
}

// TestRateLimit_HardRejects_EvenOnCacheHit proves that a request which would
// otherwise be served from the cache is still rejected when the limiter
// denies in hard mode. The limiter check must run before the cache lookup so
// a cache hit cannot be used to bypass rate or budget enforcement.
func TestRateLimit_HardRejects_EvenOnCacheHit(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, anthropicSSE)
	}))
	defer upstream.Close()

	reg := provider.Registry{"anthropic": anthropic.New("anthropic", upstream.URL, "k")}
	shapes := map[string]provider.Shape{"anthropic": provider.ShapeAnthropic}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	body := `{"model":"claude-haiku-4-5-20251001","stream":true}`
	fc := &fakeCache{store: map[string]*cache.Entry{}}
	// Pre-populate the cache as if a prior request from this tenant had
	// already been cached, so a hit would be served if the limiter check were
	// skipped or ran after the cache lookup.
	key := cache.Key("t1", provider.ShapeAnthropic, "anthropic", "claude-haiku-4-5-20251001", []byte(body))
	fc.store[key] = &cache.Entry{Status: http.StatusOK, ContentType: "text/event-stream", Body: []byte(anthropicSSE)}

	fl := &fakeLimiter{decision: ratelimit.Decision{Allowed: false, Exceeded: []string{"key:requests_per_min"}}}
	h := New(reg, router.New(routingTo("anthropic", "claude-haiku-4-5-20251001"), shapes), log, WithCache(fc), WithRateLimit(fl))

	rec := postAuthed(h, body)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 even though a cache entry existed", rec.Code)
	}
	if rec.Header().Get("x-llm-cache") != "" {
		t.Errorf("x-llm-cache = %q, want unset (cache lookup must not run after a hard rejection)", rec.Header().Get("x-llm-cache"))
	}
	if calls.Load() != 0 {
		t.Errorf("upstream called %d times, want 0 (rejected)", calls.Load())
	}
}

func TestRateLimit_SoftAllowsAndFlags(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicSSE)
	}))
	defer upstream.Close()

	fl := &fakeLimiter{decision: ratelimit.Decision{Allowed: true, Exceeded: []string{"team:monthly_usd"}}}
	h := handlerWithLimiter(t, upstream.URL, fl)
	rec := postAuthed(h, `{"model":"claude-haiku-4-5-20251001","stream":true}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (soft allows)", rec.Code)
	}
	if rec.Header().Get("x-llm-limit") != "team:monthly_usd" {
		t.Errorf("x-llm-limit = %q, want team:monthly_usd", rec.Header().Get("x-llm-limit"))
	}
	if !fl.recorded.Load() {
		t.Error("RecordUsage was not called after a served request")
	}
}
