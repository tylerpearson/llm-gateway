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
			if got.Provider != tc.wantProvider || got.Model != tc.wantModel || got.Shape != tc.wantShape {
				t.Errorf("got %+v, want provider=%s model=%s shape=%s", got, tc.wantProvider, tc.wantModel, tc.wantShape)
			}
		})
	}
}

func TestResolveNoRoute(t *testing.T) {
	// Router with no default alias and a concrete, unknown model cannot route.
	r := New(config.Routing{Aliases: map[string]config.Route{}}, map[string]provider.Shape{})
	if _, err := r.Resolve("some-model", "", ""); err == nil {
		t.Fatal("expected error when no route resolves, got nil")
	}
}
