package cache

import (
	"testing"

	"github.com/tylerpearson/llm-gateway/internal/provider"
)

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
