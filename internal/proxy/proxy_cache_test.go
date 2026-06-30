package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/tylerpearson/llm-gateway/internal/cache"
	"github.com/tylerpearson/llm-gateway/internal/provider"
	"github.com/tylerpearson/llm-gateway/internal/provider/anthropic"
	"github.com/tylerpearson/llm-gateway/internal/router"
)

type fakeCache struct {
	store map[string]*cache.Entry
	sets  int
}

func (f *fakeCache) Get(_ context.Context, key string) (*cache.Entry, bool) {
	e, ok := f.store[key]
	return e, ok
}
func (f *fakeCache) Set(_ context.Context, key string, e *cache.Entry) {
	f.sets++
	f.store[key] = e
}
func (f *fakeCache) MaxBytes() int { return 1 << 20 }

func TestCache_MissThenHit(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicSSE)
	}))
	defer upstream.Close()

	fc := &fakeCache{store: map[string]*cache.Entry{}}
	reg := provider.Registry{"anthropic": anthropic.New("anthropic", upstream.URL, "k")}
	shapes := map[string]provider.Shape{"anthropic": provider.ShapeAnthropic}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(reg, router.New(routingTo("anthropic", "claude-haiku-4-5-20251001"), shapes), log, WithCache(fc))

	body := `{"model":"claude-haiku-4-5-20251001","stream":true}`

	// First request: miss, upstream called, response cached.
	rec1 := post(h, "/v1/messages", body)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first status = %d", rec1.Code)
	}
	if rec1.Header().Get("x-llm-cache") != "miss" {
		t.Errorf("first x-llm-cache = %q, want miss", rec1.Header().Get("x-llm-cache"))
	}
	if fc.sets != 1 {
		t.Fatalf("cache sets = %d, want 1", fc.sets)
	}

	// Second identical request: hit, upstream NOT called again.
	rec2 := post(h, "/v1/messages", body)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second status = %d", rec2.Code)
	}
	if rec2.Header().Get("x-llm-cache") != "hit" {
		t.Errorf("second x-llm-cache = %q, want hit", rec2.Header().Get("x-llm-cache"))
	}
	if rec2.Body.String() != anthropicSSE {
		t.Errorf("cached body mismatch: %q", rec2.Body.String())
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Errorf("upstream calls = %d, want 1 (second served from cache)", got)
	}
}
