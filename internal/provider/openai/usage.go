package openai

import (
	"bytes"
	"encoding/json"

	"github.com/tylerpearson/llm-gateway/internal/provider"
)

// openaiUsage mirrors the usage object OpenAI reports. Streamed responses carry
// it in the final chunk when stream_options.include_usage is set.
type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func toUsage(u *openaiUsage) provider.Usage {
	if u == nil {
		return provider.Usage{}
	}
	return provider.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
}

// sseUsageScanner reads an OpenAI SSE stream and captures the usage chunk.
// The line reassembly and SSE framing (chunk buffering, data: prefix, [DONE]
// skip) live in the shared provider.SSEPayloadScanner; this type only
// interprets payloads.
type sseUsageScanner struct {
	*provider.SSEPayloadScanner
	usage provider.Usage
}

func newSSEUsageScanner() *sseUsageScanner {
	s := &sseUsageScanner{}
	s.SSEPayloadScanner = provider.NewSSEPayloadScanner(s.processPayload)
	return s
}

func (s *sseUsageScanner) processPayload(payload []byte) {
	var chunk struct {
		Usage *openaiUsage `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return
	}
	if chunk.Usage != nil {
		s.usage = toUsage(chunk.Usage)
	}
}

func (s *sseUsageScanner) Usage() provider.Usage { return s.usage }

// jsonUsageScanner buffers a single JSON response and parses its usage object.
type jsonUsageScanner struct {
	buf bytes.Buffer
}

func newJSONUsageScanner() *jsonUsageScanner { return &jsonUsageScanner{} }

func (s *jsonUsageScanner) Write(p []byte) (int, error) { return s.buf.Write(p) }

func (s *jsonUsageScanner) Usage() provider.Usage {
	var doc struct {
		Usage *openaiUsage `json:"usage"`
	}
	if err := json.Unmarshal(s.buf.Bytes(), &doc); err != nil {
		return provider.Usage{}
	}
	return toUsage(doc.Usage)
}

var (
	_ provider.UsageScanner = (*sseUsageScanner)(nil)
	_ provider.UsageScanner = (*jsonUsageScanner)(nil)
)
