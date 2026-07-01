package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tylerpearson/llm-gateway/internal/config"
	"github.com/tylerpearson/llm-gateway/internal/guard"
	"github.com/tylerpearson/llm-gateway/internal/provider"
	"github.com/tylerpearson/llm-gateway/internal/provider/anthropic"
	"github.com/tylerpearson/llm-gateway/internal/router"
)

// blockGuard always blocks, for testing the block path.
type blockGuard struct {
	category string
	reason   string
}

func (b blockGuard) Inspect(context.Context, guard.Request) guard.Decision {
	return guard.Decision{Action: guard.Block, Category: b.category, Reason: b.reason}
}

// capturingUpstream records the last request body it received.
func capturingUpstream(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var lastBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() string {
		mu.Lock()
		defer mu.Unlock()
		return lastBody
	}
}

func newGuardHandler(t *testing.T, providers provider.Registry, routing config.Routing, g guard.Guard) *Handler {
	t.Helper()
	shapes := map[string]provider.Shape{}
	for n, p := range providers {
		shapes[n] = p.Shape()
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return New(providers, router.New(routing, shapes), log, WithGuard(g))
}

func TestGuard_BlockRejectsAndSkipsUpstream(t *testing.T) {
	up, lastBody := capturingUpstream(t)
	reg := provider.Registry{"anthropic": anthropic.New("anthropic", up.URL, "k")}
	h := newGuardHandler(t, reg, routingTo("anthropic", "claude"), blockGuard{category: "pii", reason: "contains prohibited content"})

	rec := post(h, "/v1/messages", `{"model":"default","messages":[{"role":"user","content":"secret"}]}`)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "contains prohibited content") {
		t.Errorf("body = %q, want the guard reason", rec.Body.String())
	}
	if got := lastBody(); got != "" {
		t.Errorf("upstream received a body %q, want none (request blocked)", got)
	}
}

func TestGuard_MaskRewritesBodySentUpstream(t *testing.T) {
	up, lastBody := capturingUpstream(t)
	reg := provider.Registry{"anthropic": anthropic.New("anthropic", up.URL, "k")}
	h := newGuardHandler(t, reg, routingTo("anthropic", "claude"), guard.NewRegexMasker())

	rec := post(h, "/v1/messages", `{"model":"default","messages":[{"role":"user","content":"email me at jane@example.com"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := lastBody()
	if strings.Contains(got, "jane@example.com") {
		t.Errorf("upstream body still contains the email: %s", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("upstream body missing mask token: %s", got)
	}
}

func TestGuard_NopPassesThroughUnchanged(t *testing.T) {
	up, lastBody := capturingUpstream(t)
	reg := provider.Registry{"anthropic": anthropic.New("anthropic", up.URL, "k")}
	shapes := map[string]provider.Shape{"anthropic": provider.ShapeAnthropic}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := New(reg, router.New(routingTo("anthropic", "claude"), shapes), log) // default NopGuard

	body := `{"model":"default","messages":[{"role":"user","content":"email me at jane@example.com"}]}`
	rec := post(h, "/v1/messages", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// The body is forwarded with the resolved model set, so the content is intact.
	if got := lastBody(); !strings.Contains(got, "jane@example.com") {
		t.Errorf("upstream body should be unchanged by the nop guard, got %s", got)
	}
}
