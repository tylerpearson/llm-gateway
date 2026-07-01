package proxy

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/tylerpearson/llm-gateway/internal/config"
	"github.com/tylerpearson/llm-gateway/internal/pricing"
	"github.com/tylerpearson/llm-gateway/internal/provider"
	"github.com/tylerpearson/llm-gateway/internal/provider/anthropic"
	"github.com/tylerpearson/llm-gateway/internal/router"
)

func TestRequestedMaxTokens(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"anthropic max_tokens", `{"max_tokens":1024}`, 1024},
		{"openai max_completion_tokens", `{"max_completion_tokens":512}`, 512},
		{"completion tokens win over legacy", `{"max_tokens":10,"max_completion_tokens":512}`, 512},
		{"absent", `{"model":"x"}`, 0},
		{"zero ignored", `{"max_tokens":0}`, 0},
		{"malformed json", `{not json`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestedMaxTokens([]byte(tc.body)); got != tc.want {
				t.Errorf("requestedMaxTokens(%s) = %d, want %d", tc.body, got, tc.want)
			}
		})
	}
}

func TestEstimateTokens(t *testing.T) {
	// charsPerToken 1, margin 0: estimate is len(body) plus requested max_tokens.
	body := []byte(`{"max_tokens":100}`) // 18 chars
	if got, want := estimateTokens(body, 1, 0), len(body)+100; got != want {
		t.Errorf("estimate = %d, want %d", got, want)
	}

	// Safety margin inflates the input estimate.
	plain := []byte(`aaaaaaaa`) // 8 chars, no max_tokens
	if got, want := estimateTokens(plain, 4, 0), 2; got != want {
		t.Errorf("estimate = %d, want %d (8/4)", got, want)
	}
	if got := estimateTokens(plain, 4, 0.5); got != 3 {
		t.Errorf("estimate with margin = %d, want 3 (ceil(2*1.5))", got)
	}

	// A zero divisor falls back to the default of 4 rather than dividing by zero.
	if got := estimateTokens(plain, 0, 0); got != 2 {
		t.Errorf("estimate with zero divisor = %d, want 2 (default /4)", got)
	}
}

func newCtxHandler(t *testing.T, providers provider.Registry, routing config.Routing, charsPerToken int, margin float64) *Handler {
	t.Helper()
	shapes := map[string]provider.Shape{}
	for n, p := range providers {
		shapes[n] = p.Shape()
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return New(providers, router.New(routing, shapes), log,
		WithContextCheck(pricing.DefaultTable(), charsPerToken, margin))
}

// ctxProviders wires two anthropic-shaped mock upstreams. The model names carry
// the context windows under test (gpt-4o-mini: 128k, claude-opus-4-8: 200k);
// the provider shape is irrelevant to the window lookup.
func ctxProviders(t *testing.T) (provider.Registry, *int64, *int64) {
	smallUp, smallHits := statusServer(t, func(int64) (int, string) { return http.StatusOK, `{"id":"small"}` })
	bigUp, bigHits := statusServer(t, func(int64) (int, string) { return http.StatusOK, `{"id":"big"}` })
	reg := provider.Registry{
		"small": anthropic.New("small", smallUp.URL, "k1"),
		"big":   anthropic.New("big", bigUp.URL, "k2"),
	}
	return reg, smallHits, bigHits
}

func TestContextCheck_SkipsSmallWindowFallsThrough(t *testing.T) {
	reg, smallHits, bigHits := ctxProviders(t)
	routing := failoverRouting("small", "gpt-4o-mini", "big", "claude-opus-4-8")
	h := newCtxHandler(t, reg, routing, 1, 0)

	// max_tokens between the two windows (128k < est < 200k).
	body := fmt.Sprintf(`{"model":"default","max_tokens":%d,"messages":[]}`, 150000)
	rec := post(h, "/v1/messages", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"id":"big"}` {
		t.Errorf("body = %q, want big (small window too small)", got)
	}
	if *smallHits != 0 {
		t.Errorf("small hits = %d, want 0 (skipped by context check)", *smallHits)
	}
	if *bigHits != 1 {
		t.Errorf("big hits = %d, want 1", *bigHits)
	}
}

func TestContextCheck_NoneFitRejects(t *testing.T) {
	reg, smallHits, bigHits := ctxProviders(t)
	routing := failoverRouting("small", "gpt-4o-mini", "big", "claude-opus-4-8")
	h := newCtxHandler(t, reg, routing, 1, 0)

	// Estimate exceeds both windows.
	body := fmt.Sprintf(`{"model":"default","max_tokens":%d,"messages":[]}`, 250000)
	rec := post(h, "/v1/messages", body)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if *smallHits != 0 || *bigHits != 0 {
		t.Errorf("hits: small=%d big=%d, want 0 and 0 (no upstream call)", *smallHits, *bigHits)
	}
	if body := rec.Body.String(); !strings.Contains(body, "context window") {
		t.Errorf("error body = %q, want context window message", body)
	}
}

func TestContextCheck_DisabledPassesThrough(t *testing.T) {
	reg, smallHits, bigHits := ctxProviders(t)
	routing := failoverRouting("small", "gpt-4o-mini", "big", "claude-opus-4-8")
	shapes := map[string]provider.Shape{}
	for n, p := range reg {
		shapes[n] = p.Shape()
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := New(reg, router.New(routing, shapes), log) // no WithContextCheck

	body := `{"model":"default","max_tokens":250000,"messages":[]}`
	rec := post(h, "/v1/messages", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (check disabled)", rec.Code)
	}
	if *smallHits != 1 {
		t.Errorf("small hits = %d, want 1 (primary served, no check)", *smallHits)
	}
	if *bigHits != 0 {
		t.Errorf("big hits = %d, want 0", *bigHits)
	}
}

func TestContextCheck_UnknownModelFailsOpen(t *testing.T) {
	reg, smallHits, _ := ctxProviders(t)
	// Primary model is not in the pricing table, so its window is unknown and the
	// candidate is kept regardless of size.
	routing := config.Routing{
		DefaultAlias: "default",
		Aliases:      map[string]config.Route{"default": {Provider: "small", Model: "custom-unlisted-model"}},
	}
	h := newCtxHandler(t, reg, routing, 1, 0)

	body := `{"model":"default","max_tokens":500000,"messages":[]}`
	rec := post(h, "/v1/messages", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unknown window fails open)", rec.Code)
	}
	if *smallHits != 1 {
		t.Errorf("small hits = %d, want 1", *smallHits)
	}
}
