package openai

import (
	"encoding/json"
	"testing"

	"github.com/tylerpearson/llm-gateway/internal/provider"
)

func TestSSEUsageScanner(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":4}}\n\n" +
		"data: [DONE]\n\n"
	s := newSSEUsageScanner()
	// Feed in two arbitrary chunks to exercise partial line buffering.
	mid := len(stream) / 2
	_, _ = s.Write([]byte(stream[:mid]))
	_, _ = s.Write([]byte(stream[mid:]))
	if got := s.Usage(); got != (provider.Usage{InputTokens: 11, OutputTokens: 4}) {
		t.Errorf("usage = %+v, want 11/4", got)
	}
}

func TestJSONUsageScanner(t *testing.T) {
	s := newJSONUsageScanner()
	_, _ = s.Write([]byte(`{"usage":{"prompt_tokens":2,"completion_tokens":9}}`))
	if got := s.Usage(); got != (provider.Usage{InputTokens: 2, OutputTokens: 9}) {
		t.Errorf("usage = %+v, want 2/9", got)
	}
}

func TestEnsureIncludeUsage(t *testing.T) {
	out := ensureIncludeUsage([]byte(`{"model":"x","stream":true}`))
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	so, ok := m["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Errorf("stream_options.include_usage not set: %v", m["stream_options"])
	}
	// Invalid JSON is returned unchanged.
	bad := []byte(`not json`)
	if string(ensureIncludeUsage(bad)) != "not json" {
		t.Error("invalid body should be returned unchanged")
	}
}
