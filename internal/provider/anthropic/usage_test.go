package anthropic

import (
	"testing"

	"github.com/tylerpearson/llm-gateway/internal/provider"
)

const sampleSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"cache_creation_input_tokens":3,"cache_read_input_tokens":2,"output_tokens":1}}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":25}}

event: message_stop
data: {"type":"message_stop"}

`

func wantUsage() provider.Usage {
	return provider.Usage{InputTokens: 10, OutputTokens: 25, CacheReadTokens: 2, CacheWriteTokens: 3}
}

func TestSSEUsageScanner_WholeWrite(t *testing.T) {
	s := newSSEUsageScanner()
	if _, err := s.Write([]byte(sampleSSE)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := s.Usage(); got != wantUsage() {
		t.Errorf("usage = %+v, want %+v", got, wantUsage())
	}
}

// TestSSEUsageScanner_ChunkedWrites feeds the stream one byte at a time to
// exercise partial-line buffering across Write calls.
func TestSSEUsageScanner_ChunkedWrites(t *testing.T) {
	s := newSSEUsageScanner()
	for i := 0; i < len(sampleSSE); i++ {
		if _, err := s.Write([]byte{sampleSSE[i]}); err != nil {
			t.Fatalf("Write byte %d: %v", i, err)
		}
	}
	if got := s.Usage(); got != wantUsage() {
		t.Errorf("usage = %+v, want %+v", got, wantUsage())
	}
}

func TestSSEUsageScanner_IgnoresGarbage(t *testing.T) {
	s := newSSEUsageScanner()
	_, _ = s.Write([]byte("data: not-json\n\ndata: [DONE]\n\n: comment line\n"))
	if got := s.Usage(); got != (provider.Usage{}) {
		t.Errorf("usage = %+v, want zero", got)
	}
}

func TestJSONUsageScanner(t *testing.T) {
	s := newJSONUsageScanner()
	doc := `{"id":"msg_1","type":"message","usage":{"input_tokens":5,"output_tokens":7,"cache_read_input_tokens":1,"cache_creation_input_tokens":4}}`
	_, _ = s.Write([]byte(doc))
	want := provider.Usage{InputTokens: 5, OutputTokens: 7, CacheReadTokens: 1, CacheWriteTokens: 4}
	if got := s.Usage(); got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

func TestJSONUsageScanner_Invalid(t *testing.T) {
	s := newJSONUsageScanner()
	_, _ = s.Write([]byte("not json"))
	if got := s.Usage(); got != (provider.Usage{}) {
		t.Errorf("usage = %+v, want zero", got)
	}
}
