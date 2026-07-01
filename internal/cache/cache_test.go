package cache

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/tylerpearson/llm-gateway/internal/provider"
)

func newTestCache(t *testing.T, ttl time.Duration) (*Cache, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := New(mr.Addr(), ttl, DefaultMaxBytes, log)
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

func TestSetRecordsCreatedAt(t *testing.T) {
	c, _ := newTestCache(t, time.Minute)
	ctx := context.Background()
	key := Key("t", provider.ShapeAnthropic, "glm", "m", []byte(`{"a":1}`))

	e := &Entry{Status: 200, Body: []byte("hi")}
	c.Set(ctx, key, e, 0)
	if e.CreatedAt == 0 {
		t.Error("Set should stamp CreatedAt on the entry")
	}
	got, ok := c.Get(ctx, key)
	if !ok || got.CreatedAt == 0 {
		t.Errorf("stored entry missing CreatedAt: %+v ok=%v", got, ok)
	}
}

func TestSetTTLOverride(t *testing.T) {
	c, mr := newTestCache(t, time.Hour)
	ctx := context.Background()
	key := Key("t", provider.ShapeAnthropic, "glm", "m", []byte(`{"a":1}`))

	// A per-request ttl of 10s overrides the 1h default.
	c.Set(ctx, key, &Entry{Status: 200, Body: []byte("hi")}, 10*time.Second)
	if _, ok := c.Get(ctx, key); !ok {
		t.Fatal("expected hit right after set")
	}
	mr.FastForward(11 * time.Second)
	if _, ok := c.Get(ctx, key); ok {
		t.Error("entry should have expired after its per-request ttl")
	}
}

func TestDeleteAndPing(t *testing.T) {
	c, mr := newTestCache(t, time.Minute)
	ctx := context.Background()
	key := Key("t", provider.ShapeAnthropic, "glm", "m", []byte(`{"a":1}`))

	c.Set(ctx, key, &Entry{Status: 200, Body: []byte("hi")}, 0)
	if err := c.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := c.Get(ctx, key); ok {
		t.Error("entry should be gone after delete")
	}

	if err := c.Ping(ctx); err != nil {
		t.Errorf("ping should succeed: %v", err)
	}
	mr.Close()
	if err := c.Ping(ctx); err == nil {
		t.Error("ping should fail when the backend is down")
	}
}

func TestKeyCanonical(t *testing.T) {
	base := Key("team-a", provider.ShapeAnthropic, "glm", "m", []byte(`{"a":1,"b":2}`))

	// Reordered keys and extra whitespace produce the same key.
	same := Key("team-a", provider.ShapeAnthropic, "glm", "m", []byte(`{"b":2,   "a":1}`))
	if base != same {
		t.Error("canonicalization should make whitespace and key order irrelevant")
	}

	// Different model, provider, or shape produce different keys.
	if base == Key("team-a", provider.ShapeAnthropic, "glm", "other", []byte(`{"a":1,"b":2}`)) {
		t.Error("different model should change the key")
	}
	if base == Key("team-a", provider.ShapeAnthropic, "openai", "m", []byte(`{"a":1,"b":2}`)) {
		t.Error("different provider should change the key")
	}
	if base == Key("team-a", provider.ShapeOpenAI, "glm", "m", []byte(`{"a":1,"b":2}`)) {
		t.Error("different shape should change the key")
	}
	// Different body content changes the key.
	if base == Key("team-a", provider.ShapeAnthropic, "glm", "m", []byte(`{"a":1,"b":3}`)) {
		t.Error("different body should change the key")
	}
}

func TestKeyTenantScoping(t *testing.T) {
	// Two different tenants issuing the identical request must not collide.
	a := Key("team-a", provider.ShapeAnthropic, "glm", "m", []byte(`{"a":1,"b":2}`))
	b := Key("team-b", provider.ShapeAnthropic, "glm", "m", []byte(`{"a":1,"b":2}`))
	if a == b {
		t.Error("different tenants should produce different keys for the same request")
	}

	// The same tenant and the same body produce the same key.
	again := Key("team-a", provider.ShapeAnthropic, "glm", "m", []byte(`{"a":1,"b":2}`))
	if a != again {
		t.Error("same tenant and same body should produce the same key")
	}
}
