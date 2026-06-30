package translate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/tylerpearson/llm-gateway/internal/provider"
)

// usageJSON is the token usage shape shared by both wire formats here.
type oaUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// OpenAIResponseToAnthropic reads an OpenAI Chat Completions response from body
// and writes the equivalent Anthropic Messages response to w, returning the
// captured usage. stream selects SSE event translation versus a single JSON
// document.
func OpenAIResponseToAnthropic(w io.Writer, body io.Reader, stream bool, model string) (provider.Usage, error) {
	if stream {
		return openAIStreamToAnthropic(w, body, model)
	}
	return openAIJSONToAnthropic(w, body, model)
}

func openAIJSONToAnthropic(w io.Writer, body io.Reader, model string) (provider.Usage, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return provider.Usage{}, err
	}
	var in struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *oaUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return provider.Usage{}, fmt.Errorf("translate: parse openai response: %w", err)
	}

	content, finish := "", "stop"
	if len(in.Choices) > 0 {
		content = in.Choices[0].Message.Content
		finish = in.Choices[0].FinishReason
	}
	usage := provider.Usage{}
	if in.Usage != nil {
		usage = provider.Usage{InputTokens: in.Usage.PromptTokens, OutputTokens: in.Usage.CompletionTokens}
	}

	out := map[string]any{
		"id":            orDefault(in.ID, "msg_translated"),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []any{map[string]any{"type": "text", "text": content}},
		"stop_reason":   mapFinishToStop(finish),
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens},
	}
	enc, _ := json.Marshal(out)
	_, err = w.Write(enc)
	return usage, err
}

func openAIStreamToAnthropic(w io.Writer, body io.Reader, model string) (provider.Usage, error) {
	if err := writeAnthropicEvent(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_translated", "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}); err != nil {
		return provider.Usage{}, err
	}
	if err := writeAnthropicEvent(w, "content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	}); err != nil {
		return provider.Usage{}, err
	}

	var usage provider.Usage
	finish := "stop"
	sc := newSSEScanner(body)
	for sc.Scan() {
		payload := sc.data()
		if payload == nil || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *oaUsage `json:"usage"`
		}
		if err := json.Unmarshal(payload, &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			usage = provider.Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		}
		if len(chunk.Choices) > 0 {
			if text := chunk.Choices[0].Delta.Content; text != "" {
				if err := writeAnthropicEvent(w, "content_block_delta", map[string]any{
					"type": "content_block_delta", "index": 0,
					"delta": map[string]any{"type": "text_delta", "text": text},
				}); err != nil {
					return usage, err
				}
			}
			if fr := chunk.Choices[0].FinishReason; fr != "" {
				finish = fr
			}
		}
	}
	if err := sc.Err(); err != nil {
		return usage, err
	}

	_ = writeAnthropicEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	_ = writeAnthropicEvent(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": mapFinishToStop(finish), "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": usage.OutputTokens},
	})
	err := writeAnthropicEvent(w, "message_stop", map[string]any{"type": "message_stop"})
	return usage, err
}

// AnthropicResponseToOpenAI reads an Anthropic Messages response from body and
// writes the equivalent OpenAI Chat Completions response to w.
func AnthropicResponseToOpenAI(w io.Writer, body io.Reader, stream bool, model string) (provider.Usage, error) {
	if stream {
		return anthropicStreamToOpenAI(w, body, model)
	}
	return anthropicJSONToOpenAI(w, body, model)
}

func anthropicJSONToOpenAI(w io.Writer, body io.Reader, model string) (provider.Usage, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return provider.Usage{}, err
	}
	var in struct {
		ID      string `json:"id"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return provider.Usage{}, fmt.Errorf("translate: parse anthropic response: %w", err)
	}
	var text string
	for _, c := range in.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	usage := provider.Usage{InputTokens: in.Usage.InputTokens, OutputTokens: in.Usage.OutputTokens}
	out := map[string]any{
		"id": orDefault(in.ID, "chatcmpl_translated"), "object": "chat.completion", "model": model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": mapStopToFinish(in.StopReason),
		}},
		"usage": oaUsage{PromptTokens: usage.InputTokens, CompletionTokens: usage.OutputTokens, TotalTokens: usage.InputTokens + usage.OutputTokens},
	}
	enc, _ := json.Marshal(out)
	_, err = w.Write(enc)
	return usage, err
}

func anthropicStreamToOpenAI(w io.Writer, body io.Reader, model string) (provider.Usage, error) {
	const id = "chatcmpl_translated"
	openAIChunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id": id, "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
	}
	if err := writeDataEvent(w, openAIChunk(map[string]any{"role": "assistant", "content": ""}, nil)); err != nil {
		return provider.Usage{}, err
	}

	var usage provider.Usage
	finish := "stop"
	sc := newSSEScanner(body)
	for sc.Scan() {
		payload := sc.data()
		if payload == nil {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Delta   struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Message struct {
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(payload, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			usage.InputTokens = ev.Message.Usage.InputTokens
			if ev.Message.Usage.OutputTokens > 0 {
				usage.OutputTokens = ev.Message.Usage.OutputTokens
			}
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				if err := writeDataEvent(w, openAIChunk(map[string]any{"content": ev.Delta.Text}, nil)); err != nil {
					return usage, err
				}
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				finish = mapStopToFinish(ev.Delta.StopReason)
			}
			if ev.Usage.OutputTokens > 0 {
				usage.OutputTokens = ev.Usage.OutputTokens
			}
		}
	}
	if err := sc.Err(); err != nil {
		return usage, err
	}

	_ = writeDataEvent(w, openAIChunk(map[string]any{}, finish))
	_ = writeDataEvent(w, map[string]any{
		"id": id, "object": "chat.completion.chunk", "model": model, "choices": []any{},
		"usage": oaUsage{PromptTokens: usage.InputTokens, CompletionTokens: usage.OutputTokens, TotalTokens: usage.InputTokens + usage.OutputTokens},
	})
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return usage, err
}

// writeAnthropicEvent writes one Anthropic SSE event (event + data lines).
func writeAnthropicEvent(w io.Writer, event string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	return err
}

// writeDataEvent writes one OpenAI style SSE event (data line only).
func writeDataEvent(w io.Writer, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// sseScanner reads an SSE stream line by line and exposes the JSON payload of
// each data line.
type sseScanner struct {
	sc      *bufio.Scanner
	payload []byte
}

func newSSEScanner(r io.Reader) *sseScanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return &sseScanner{sc: sc}
}

func (s *sseScanner) Scan() bool {
	for s.sc.Scan() {
		line := bytes.TrimRight(s.sc.Bytes(), "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		s.payload = bytes.TrimSpace(line[len("data:"):])
		return true
	}
	return false
}

func (s *sseScanner) data() []byte { return s.payload }

func (s *sseScanner) Err() error { return s.sc.Err() }
