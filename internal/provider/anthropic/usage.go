package anthropic

import (
	"bytes"
	"encoding/json"

	"github.com/tylerpearson/llm-gateway/internal/provider"
)

// anthropicUsage mirrors the usage object Anthropic reports in both streamed
// events and single JSON responses. Fields are optional and default to zero.
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// sseUsageScanner extracts usage from a streamed Messages response. Anthropic
// reports input and cache tokens in the message_start event and the running
// output token count in message_delta events, so we set input/cache once and
// keep overwriting output with the latest value. The line reassembly and SSE
// framing (chunk buffering, data: prefix, [DONE] skip) live in the shared
// provider.SSEPayloadScanner; this type only interprets payloads.
type sseUsageScanner struct {
	*provider.SSEPayloadScanner
	usage provider.Usage
}

func newSSEUsageScanner() *sseUsageScanner {
	s := &sseUsageScanner{}
	s.SSEPayloadScanner = provider.NewSSEPayloadScanner(s.processPayload)
	return s
}

type sseEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`
	Usage *anthropicUsage `json:"usage"`
}

func (s *sseUsageScanner) processPayload(payload []byte) {
	var ev sseEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		// Tolerate non-JSON or partial data lines; usage events are well formed.
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil && ev.Message.Usage != nil {
			u := ev.Message.Usage
			s.usage.InputTokens = u.InputTokens
			s.usage.CacheWriteTokens = u.CacheCreationInputTokens
			s.usage.CacheReadTokens = u.CacheReadInputTokens
			if u.OutputTokens > 0 {
				s.usage.OutputTokens = u.OutputTokens
			}
		}
	case "message_delta":
		if ev.Usage != nil && ev.Usage.OutputTokens > 0 {
			s.usage.OutputTokens = ev.Usage.OutputTokens
		}
	}
}

// Usage returns the captured usage. Call after the stream is fully written.
func (s *sseUsageScanner) Usage() provider.Usage { return s.usage }

// jsonUsageScanner buffers a single JSON response and parses its usage object.
type jsonUsageScanner struct {
	buf bytes.Buffer
}

func newJSONUsageScanner() *jsonUsageScanner { return &jsonUsageScanner{} }

func (s *jsonUsageScanner) Write(p []byte) (int, error) { return s.buf.Write(p) }

func (s *jsonUsageScanner) Usage() provider.Usage {
	var doc struct {
		Usage *anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(s.buf.Bytes(), &doc); err != nil || doc.Usage == nil {
		return provider.Usage{}
	}
	return provider.Usage{
		InputTokens:      doc.Usage.InputTokens,
		OutputTokens:     doc.Usage.OutputTokens,
		CacheReadTokens:  doc.Usage.CacheReadInputTokens,
		CacheWriteTokens: doc.Usage.CacheCreationInputTokens,
	}
}

var (
	_ provider.UsageScanner = (*sseUsageScanner)(nil)
	_ provider.UsageScanner = (*jsonUsageScanner)(nil)
)
