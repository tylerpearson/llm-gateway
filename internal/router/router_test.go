package router

import (
	"testing"

	"github.com/tylerpearson/llm-gateway/internal/config"
	"github.com/tylerpearson/llm-gateway/internal/provider"
)

func testRouter() *Router {
	routing := config.Routing{
		DefaultAlias: "default",
		Aliases: map[string]config.Route{
			"default":  {Provider: "anthropic", Model: "claude-haiku-4-5-20251001"},
			"fast":     {Provider: "openai", Model: "gpt-4o-mini"},
			"frontier": {Provider: "anthropic", Model: "claude-opus-4-8"},
		},
	}
	shapes := map[string]provider.Shape{
		"anthropic": provider.ShapeAnthropic,
		"openai":    provider.ShapeOpenAI,
	}
	return New(routing, shapes)
}

func TestResolve(t *testing.T) {
	r := testRouter()
	tests := []struct {
		name                          string
		reqModel, tier, keyDefault    string
		wantProvider, wantModel       string
		wantShape                     provider.Shape
	}{
		{"alias by request model", "fast", "", "", "openai", "gpt-4o-mini", provider.ShapeOpenAI},
		{"tier header overrides model", "default", "frontier", "", "anthropic", "claude-opus-4-8", provider.ShapeAnthropic},
		{"key default when model empty", "", "", "fast", "openai", "gpt-4o-mini", provider.ShapeOpenAI},
		{"config default fallback", "", "", "", "anthropic", "claude-haiku-4-5-20251001", provider.ShapeAnthropic},
		{"concrete model passes through to default provider", "claude-3-5-haiku", "", "", "anthropic", "claude-3-5-haiku", provider.ShapeAnthropic},
		{"tier beats key default", "", "frontier", "fast", "anthropic", "claude-opus-4-8", provider.ShapeAnthropic},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.Resolve(tc.reqModel, tc.tier, tc.keyDefault)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d candidates, want 1 (no fallbacks configured)", len(got))
			}
			if got[0].Provider != tc.wantProvider || got[0].Model != tc.wantModel || got[0].Shape != tc.wantShape {
				t.Errorf("got %+v, want provider=%s model=%s shape=%s", got[0], tc.wantProvider, tc.wantModel, tc.wantShape)
			}
		})
	}
}

// TestResolveFallbacks verifies that an alias's fallbacks are appended in order
// after the primary, that unknown-provider fallbacks are dropped, and that
// concrete-model passthrough applies only to the primary.
func TestResolveFallbacks(t *testing.T) {
	routing := config.Routing{
		DefaultAlias: "default",
		Aliases: map[string]config.Route{
			"default": {
				Provider: "anthropic",
				Model:    "claude-haiku-4-5-20251001",
				Fallbacks: []config.Route{
					{Provider: "openai", Model: "gpt-4o-mini"},
					{Provider: "ghost", Model: "does-not-exist"}, // dropped: unknown provider
				},
			},
		},
	}
	shapes := map[string]provider.Shape{
		"anthropic": provider.ShapeAnthropic,
		"openai":    provider.ShapeOpenAI,
	}
	r := New(routing, shapes)

	t.Run("alias resolves primary plus known fallback", func(t *testing.T) {
		got, err := r.Resolve("default", "", "")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		want := []Target{
			{Provider: "anthropic", Model: "claude-haiku-4-5-20251001", Shape: provider.ShapeAnthropic},
			{Provider: "openai", Model: "gpt-4o-mini", Shape: provider.ShapeOpenAI},
		}
		if len(got) != len(want) {
			t.Fatalf("got %d candidates, want %d: %+v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("candidate %d = %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	t.Run("concrete model overrides only the primary", func(t *testing.T) {
		got, err := r.Resolve("claude-3-5-haiku", "", "")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got[0].Model != "claude-3-5-haiku" {
			t.Errorf("primary model = %q, want concrete passthrough claude-3-5-haiku", got[0].Model)
		}
		if got[1].Model != "gpt-4o-mini" {
			t.Errorf("fallback model = %q, want configured gpt-4o-mini", got[1].Model)
		}
	})
}

func TestResolveNoRoute(t *testing.T) {
	// Router with no default alias and a concrete, unknown model cannot route.
	r := New(config.Routing{Aliases: map[string]config.Route{}}, map[string]provider.Shape{})
	if _, err := r.Resolve("some-model", "", ""); err == nil {
		t.Fatal("expected error when no route resolves, got nil")
	}
}
