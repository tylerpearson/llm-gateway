// Package translate is the bounded cross-shape translation module. It converts
// requests and responses between the Anthropic Messages wire format and the
// OpenAI Chat Completions wire format so a request received in one shape can be
// served by a provider that speaks the other (for example an Anthropic
// /v1/messages request routed to GLM).
//
// Scope is intentionally bounded to text conversations: string or text-block
// message content, system prompts, and the common sampling parameters. Tool
// calls, images, and other modalities are out of scope for v1 and pass through
// only as far as their text content allows. The module is pure and tested in
// isolation.
package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// defaultMaxTokens is used when translating an OpenAI request (where max_tokens
// is optional) to Anthropic (where it is required).
const defaultMaxTokens = 4096

// anthropicRequest is the subset of the Anthropic Messages request we translate.
type anthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	MaxTokens     int                `json:"max_tokens,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// openAIRequest is the subset of the OpenAI Chat Completions request we handle.
type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stop        json.RawMessage `json:"stop,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// AnthropicRequestToOpenAI converts an Anthropic Messages request body into an
// OpenAI Chat Completions request body targeting targetModel.
func AnthropicRequestToOpenAI(body []byte, targetModel string) ([]byte, error) {
	var in anthropicRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("translate: parse anthropic request: %w", err)
	}

	out := openAIRequest{
		Model:       targetModel,
		MaxTokens:   in.MaxTokens,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		Stream:      in.Stream,
	}
	if sys := textFromRaw(in.System); sys != "" {
		out.Messages = append(out.Messages, openAIMessage{Role: "system", Content: sys})
	}
	for _, m := range in.Messages {
		out.Messages = append(out.Messages, openAIMessage{
			Role:    m.Role,
			Content: textFromRaw(m.Content),
		})
	}
	if len(in.StopSequences) > 0 {
		stop, _ := json.Marshal(in.StopSequences)
		out.Stop = stop
	}
	return json.Marshal(out)
}

// OpenAIRequestToAnthropic converts an OpenAI Chat Completions request body into
// an Anthropic Messages request body targeting targetModel.
func OpenAIRequestToAnthropic(body []byte, targetModel string) ([]byte, error) {
	var in openAIRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("translate: parse openai request: %w", err)
	}

	out := anthropicRequest{
		Model:       targetModel,
		MaxTokens:   in.MaxTokens,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		Stream:      in.Stream,
	}
	if out.MaxTokens == 0 {
		out.MaxTokens = defaultMaxTokens
	}

	var systemParts []string
	for _, m := range in.Messages {
		if m.Role == "system" {
			systemParts = append(systemParts, m.Content)
			continue
		}
		content, _ := json.Marshal(m.Content)
		out.Messages = append(out.Messages, anthropicMessage{Role: m.Role, Content: content})
	}
	if len(systemParts) > 0 {
		sys, _ := json.Marshal(strings.Join(systemParts, "\n\n"))
		out.System = sys
	}
	if len(in.Stop) > 0 {
		out.StopSequences = stopToSequences(in.Stop)
	}
	return json.Marshal(out)
}

// textFromRaw extracts plain text from an Anthropic content value that may be a
// JSON string or an array of content blocks. Only text blocks contribute.
func textFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

// stopToSequences normalizes an OpenAI stop value (string or array) into a
// string slice for Anthropic stop_sequences.
func stopToSequences(raw json.RawMessage) []string {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}
	return nil
}

// mapFinishToStop converts an OpenAI finish_reason to an Anthropic stop_reason.
func mapFinishToStop(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "stop_sequence"
	default:
		return "end_turn"
	}
}

// mapStopToFinish converts an Anthropic stop_reason to an OpenAI finish_reason.
func mapStopToFinish(stop string) string {
	switch stop {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}
