package eval

import (
	"context"
	"testing"
)

// TestNopHookIsInert verifies the default hook accepts a request and does
// nothing observable (no panic, no mutation).
func TestNopHookIsInert(t *testing.T) {
	var hook MirrorHook = NopHook{}
	hook.Mirror(context.Background(), MirrorRequest{
		RequestID:      "r1",
		ServedModel:    "claude-haiku-4-5-20251001",
		Provider:       "anthropic",
		Body:           []byte(`{"model":"default"}`),
	})
}
