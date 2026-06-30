//go:build integration

// Needs a real Redis instance. Runs only under the integration build tag and
// skips unless REDIS_ADDR is set.
package cache

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/tylerpearson/llm-gateway/internal/provider"
)

func TestCacheRoundTrip(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set; skipping Redis integration test")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := New(addr, time.Minute, 32, log)
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	nonce := fmt.Sprintf(`{"n":%d}`, time.Now().UnixNano())
	key := Key(provider.ShapeAnthropic, "glm", "m", []byte(nonce))

	if _, ok := c.Get(ctx, key); ok {
		t.Fatal("expected miss for a fresh key")
	}

	entry := &Entry{Status: 200, ContentType: "text/event-stream", Body: []byte("hello"),
		Usage: provider.Usage{InputTokens: 3, OutputTokens: 4}}
	c.Set(ctx, key, entry)

	got, ok := c.Get(ctx, key)
	if !ok {
		t.Fatal("expected hit after set")
	}
	if got.Status != 200 || string(got.Body) != "hello" || got.Usage.OutputTokens != 4 {
		t.Errorf("got %+v, unexpected", got)
	}

	// Oversized bodies are not cached (maxBytes is 32 above).
	bigKey := Key(provider.ShapeAnthropic, "glm", "big", []byte(nonce))
	c.Set(ctx, bigKey, &Entry{Status: 200, Body: make([]byte, 1024)})
	if _, ok := c.Get(ctx, bigKey); ok {
		t.Error("oversized entry should not be cached")
	}
}
