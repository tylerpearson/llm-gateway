package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tylerpearson/llm-gateway/internal/cache"
	"github.com/tylerpearson/llm-gateway/internal/provider"
	"github.com/tylerpearson/llm-gateway/internal/provider/anthropic"
	"github.com/tylerpearson/llm-gateway/internal/router"
)

type fakeCache struct {
	store   map[string]*cache.Entry
	sets    int
	lastTTL time.Duration
}

func (f *fakeCache) Get(_ context.Context, key string) (*cache.Entry, bool) {
	e, ok := f.store[key]
	return e, ok
}
func (f *fakeCache) Set(_ context.Context, key string, e *cache.Entry, ttl time.Duration) {
	f.sets++
	f.lastTTL = ttl
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

// failAfterWriter wraps an httptest.ResponseRecorder and fails every Write
// call after the first allow calls succeed, simulating a client that
// disconnects partway through a streamed response.
type failAfterWriter struct {
	*httptest.ResponseRecorder
	allow int
	calls int
}

func (f *failAfterWriter) Write(p []byte) (int, error) {
	f.calls++
	if f.calls > f.allow {
		return 0, errors.New("simulated client disconnect")
	}
	return f.ResponseRecorder.Write(p)
}

// TestCache_InterruptedRelayNotStored covers the bug this plan fixes: when the
// write to the client fails partway through a streamed response, relay stops
// copying immediately, so the cache capture buffer holds only a prefix of the
// upstream response. That partial response must not be written to the cache,
// or every later identical request in the same tenant would replay a
// truncated body until the entry expires.
func TestCache_InterruptedRelayNotStored(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// Write and flush each SSE event separately so the client-side relay
		// sees the body across multiple reads, not as a single chunk.
		for _, chunk := range strings.SplitAfter(anthropicSSE, "\n\n") {
			if chunk == "" {
				continue
			}
			_, _ = io.WriteString(w, chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	fc := &fakeCache{store: map[string]*cache.Entry{}}
	reg := provider.Registry{"anthropic": anthropic.New("anthropic", upstream.URL, "k")}
	shapes := map[string]provider.Shape{"anthropic": provider.ShapeAnthropic}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(reg, router.New(routingTo("anthropic", "claude-haiku-4-5-20251001"), shapes), log, WithCache(fc))

	body := `{"model":"claude-haiku-4-5-20251001","stream":true}`

	// First request: the client writer fails after the first chunk. The relay
	// is interrupted, so the response the "client" received is a strict
	// prefix of the full upstream body.
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	w1 := &failAfterWriter{ResponseRecorder: httptest.NewRecorder(), allow: 1}
	h.Messages(w1, req1)

	if w1.Body.String() == anthropicSSE || w1.Body.Len() == 0 {
		t.Fatalf("expected a non-empty, truncated body from the interrupted relay, got %q", w1.Body.String())
	}
	if fc.sets != 0 {
		t.Fatalf("cache sets after interrupted relay = %d, want 0", fc.sets)
	}

	// Second, identical request with a normal writer: must still be a cache
	// miss, proving the partial response from the first request was not
	// stored, and the upstream must be called again to serve it.
	rec2 := post(h, "/v1/messages", body)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second status = %d", rec2.Code)
	}
	if rec2.Header().Get("x-llm-cache") != "miss" {
		t.Errorf("second x-llm-cache = %q, want miss", rec2.Header().Get("x-llm-cache"))
	}
	if rec2.Body.String() != anthropicSSE {
		t.Errorf("second body mismatch: %q", rec2.Body.String())
	}
	if got := upstreamCalls.Load(); got != 2 {
		t.Errorf("upstream calls = %d, want 2 (no cache hit after interrupted relay)", got)
	}
}
