// This file implements the optional pre-call context-window check: a cheap,
// conservative estimate of a request's token size used to skip candidate models
// that clearly cannot fit it, so the gateway fails over to a larger-context
// model (or rejects the request) instead of sending a call guaranteed to fail
// upstream. The gateway has no real tokenizer available (no CGO), so the
// estimate is deliberately an over-estimate and is not an exact token count.
package proxy

import (
	"encoding/json"
	"math"

	"github.com/tylerpearson/llm-gateway/internal/pricing"
	"github.com/tylerpearson/llm-gateway/internal/router"
)

// contextCheck holds the pre-call context-window check configuration. It is off
// unless WithContextCheck installs it.
type contextCheck struct {
	enabled       bool
	table         pricing.Table
	charsPerToken int
	safetyMargin  float64
}

// estimateTokens returns a conservative token estimate for a request body:
// the body's character count divided by charsPerToken, inflated by safetyMargin,
// plus any requested output tokens (max_tokens or max_completion_tokens). It
// intentionally over-estimates; it is only used to skip models that clearly
// cannot fit the request.
func estimateTokens(body []byte, charsPerToken int, safetyMargin float64) int {
	if charsPerToken <= 0 {
		charsPerToken = 4
	}
	inputTokens := float64(len(body)) / float64(charsPerToken)
	inflated := inputTokens * (1 + safetyMargin)
	return int(math.Ceil(inflated)) + requestedMaxTokens(body)
}

// requestedMaxTokens reads the client's requested output token cap from the body,
// accepting both the Anthropic (max_tokens) and OpenAI (max_completion_tokens,
// or legacy max_tokens) field names. A missing or unparseable value counts as 0.
func requestedMaxTokens(body []byte) int {
	var m struct {
		MaxTokens           *int `json:"max_tokens"`
		MaxCompletionTokens *int `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return 0
	}
	if m.MaxCompletionTokens != nil && *m.MaxCompletionTokens > 0 {
		return *m.MaxCompletionTokens
	}
	if m.MaxTokens != nil && *m.MaxTokens > 0 {
		return *m.MaxTokens
	}
	return 0
}

// filterByContext drops candidates whose model has a known context window
// smaller than est. Candidates with an unknown window are kept (fail open). It
// returns the surviving candidates and the largest known window seen across all
// candidates, for use in the rejection message when nothing fits.
func (h *Handler) filterByContext(candidates []router.Target, est int) ([]router.Target, int) {
	out := make([]router.Target, 0, len(candidates))
	var largest int
	for _, t := range candidates {
		window, ok := h.ctxCheck.table.ContextWindow(t.Model)
		if !ok {
			out = append(out, t)
			continue
		}
		if window > largest {
			largest = window
		}
		if est > window {
			if h.metrics != nil {
				h.metrics.IncContextSkip(t.Model)
			}
			continue
		}
		out = append(out, t)
	}
	return out, largest
}
