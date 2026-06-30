package cache

import (
	"testing"

	"github.com/tylerpearson/llm-gateway/internal/provider"
)

func TestKeyCanonical(t *testing.T) {
	base := Key(provider.ShapeAnthropic, "glm", "m", []byte(`{"a":1,"b":2}`))

	// Reordered keys and extra whitespace produce the same key.
	same := Key(provider.ShapeAnthropic, "glm", "m", []byte(`{"b":2,   "a":1}`))
	if base != same {
		t.Error("canonicalization should make whitespace and key order irrelevant")
	}

	// Different model, provider, or shape produce different keys.
	if base == Key(provider.ShapeAnthropic, "glm", "other", []byte(`{"a":1,"b":2}`)) {
		t.Error("different model should change the key")
	}
	if base == Key(provider.ShapeAnthropic, "openai", "m", []byte(`{"a":1,"b":2}`)) {
		t.Error("different provider should change the key")
	}
	if base == Key(provider.ShapeOpenAI, "glm", "m", []byte(`{"a":1,"b":2}`)) {
		t.Error("different shape should change the key")
	}
	// Different body content changes the key.
	if base == Key(provider.ShapeAnthropic, "glm", "m", []byte(`{"a":1,"b":3}`)) {
		t.Error("different body should change the key")
	}
}
