package translate

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tylerpearson/llm-gateway/internal/provider"
)

func TestAnthropicRequestToOpenAI(t *testing.T) {
	in := `{"model":"claude-x","system":"be brief","max_tokens":100,"stop_sequences":["END"],
	        "messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"text","text":"hi"}]}]}`
	out, err := AnthropicRequestToOpenAI([]byte(in), "glm-4.5")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got openAIRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Model != "glm-4.5" {
		t.Errorf("model = %q, want glm-4.5", got.Model)
	}
	if got.MaxTokens != 100 {
		t.Errorf("max_tokens = %d, want 100", got.MaxTokens)
	}
	if len(got.Messages) != 3 || got.Messages[0].Role != "system" || got.Messages[0].Content != "be brief" {
		t.Fatalf("messages = %+v, want system first", got.Messages)
	}
	if got.Messages[1].Content != "hello" || got.Messages[2].Content != "hi" {
		t.Errorf("message content not preserved: %+v", got.Messages)
	}
	if string(got.Stop) != `["END"]` {
		t.Errorf("stop = %s, want [\"END\"]", got.Stop)
	}
}

func TestOpenAIRequestToAnthropic(t *testing.T) {
	in := `{"model":"gpt","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],"stop":"STOP"}`
	out, err := OpenAIRequestToAnthropic([]byte(in), "claude-haiku")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got anthropicRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Model != "claude-haiku" {
		t.Errorf("model = %q", got.Model)
	}
	if got.MaxTokens != defaultMaxTokens {
		t.Errorf("max_tokens = %d, want default %d", got.MaxTokens, defaultMaxTokens)
	}
	if textFromRaw(got.System) != "sys" {
		t.Errorf("system = %q, want sys", textFromRaw(got.System))
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
		t.Errorf("messages = %+v, want single user message", got.Messages)
	}
	if len(got.StopSequences) != 1 || got.StopSequences[0] != "STOP" {
		t.Errorf("stop_sequences = %v, want [STOP]", got.StopSequences)
	}
}

func TestOpenAIResponseToAnthropic_JSON(t *testing.T) {
	in := `{"id":"x","choices":[{"message":{"content":"hello world"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":7}}`
	var buf bytes.Buffer
	usage, err := OpenAIResponseToAnthropic(&buf, strings.NewReader(in), false, "glm-4.5")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if usage != (provider.Usage{InputTokens: 5, OutputTokens: 7}) {
		t.Errorf("usage = %+v, want 5/7", usage)
	}
	var got struct {
		Type       string `json:"type"`
		Role       string `json:"role"`
		Content    []struct{ Text string `json:"text"` } `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != "message" || got.Role != "assistant" {
		t.Errorf("got type=%q role=%q", got.Type, got.Role)
	}
	if len(got.Content) != 1 || got.Content[0].Text != "hello world" {
		t.Errorf("content = %+v", got.Content)
	}
	if got.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", got.StopReason)
	}
}

func TestOpenAIResponseToAnthropic_Stream(t *testing.T) {
	in := "data: {\"choices\":[{\"delta\":{\"content\":\"he\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"llo\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	var buf bytes.Buffer
	usage, err := OpenAIResponseToAnthropic(&buf, strings.NewReader(in), true, "glm-4.5")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if usage != (provider.Usage{InputTokens: 4, OutputTokens: 2}) {
		t.Errorf("usage = %+v, want 4/2", usage)
	}
	out := buf.String()
	for _, want := range []string{"event: message_start", "event: content_block_start", "text_delta", "he", "llo", "event: message_delta", "event: message_stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream output missing %q\n%s", want, out)
		}
	}
}

func TestAnthropicResponseToOpenAI_JSON(t *testing.T) {
	in := `{"content":[{"type":"text","text":"hey"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":9}}`
	var buf bytes.Buffer
	usage, err := AnthropicResponseToOpenAI(&buf, strings.NewReader(in), false, "gpt-4o")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if usage != (provider.Usage{InputTokens: 3, OutputTokens: 9}) {
		t.Errorf("usage = %+v, want 3/9", usage)
	}
	var got struct {
		Object  string `json:"object"`
		Choices []struct {
			Message      struct{ Content string `json:"content"` } `json:"message"`
			FinishReason string                                    `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Object != "chat.completion" || len(got.Choices) != 1 {
		t.Fatalf("got %+v", got)
	}
	if got.Choices[0].Message.Content != "hey" || got.Choices[0].FinishReason != "stop" {
		t.Errorf("choice = %+v", got.Choices[0])
	}
}

func TestAnthropicResponseToOpenAI_Stream(t *testing.T) {
	in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":6,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"yo\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var buf bytes.Buffer
	usage, err := AnthropicResponseToOpenAI(&buf, strings.NewReader(in), true, "gpt-4o")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if usage != (provider.Usage{InputTokens: 6, OutputTokens: 5}) {
		t.Errorf("usage = %+v, want 6/5", usage)
	}
	out := buf.String()
	for _, want := range []string{`"role":"assistant"`, `"content":"yo"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream output missing %q\n%s", want, out)
		}
	}
}
