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
type sseUsageScanner struct {
	pending []byte
	usage   provider.Usage
}

func newSSEUsageScanner() *sseUsageScanner { return &sseUsageScanner{} }

func (s *sseUsageScanner) Write(p []byte) (int, error) {
	s.pending = append(s.pending, p...)
	for {
		i := bytes.IndexByte(s.pending, '\n')
		if i < 0 {
			break
		}
		line := s.pending[:i]
		s.pending = s.pending[i+1:]
		s.processLine(line)
	}
	return len(p), nil
}

func (s *sseUsageScanner) processLine(line []byte) {
	line = bytes.TrimRight(line, "\r")
	const prefix = "data:"
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return
	}
	payload := bytes.TrimSpace(line[len(prefix):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
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
