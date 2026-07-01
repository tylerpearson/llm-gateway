// Package guard defines the pre-call guardrail seam: a hook invoked before the
// upstream request that can allow a request through, rewrite (mask) its body, or
// block it outright. It mirrors the eval.MirrorHook seam but, unlike that
// post-routing observer, a guard can change or reject the request.
//
// This is distinct from prompt redaction (internal/config Security.RedactPrompts):
// redaction only keeps content out of logs, whereas a guard acts on the request
// that is actually sent upstream. Guards must not persist prompt content; they
// inspect the in-flight body and return a decision.
//
// v1 ships the interface, a no-op default, and one reference implementation
// (RegexMasker). Output/response guarding is intentionally out of scope: it
// conflicts with streaming, which would require buffering the full response.
package guard

import "context"

// Action is what a guard decided to do with a request.
type Action int

const (
	// Allow lets the request proceed unchanged.
	Allow Action = iota
	// Block rejects the request; it is never sent upstream.
	Block
	// Mask replaces the request body with Decision.Rewrite before it is sent.
	Mask
)

// Request is the pre-call snapshot handed to a guard. Body is the request body
// about to be sent upstream; guards must treat it as read only and return a
// rewritten copy in Decision.Rewrite rather than mutating it in place.
type Request struct {
	RequestID string
	KeyID     string
	TeamID    string
	Provider  string
	Model     string
	Body      []byte
}

// Decision is a guard's verdict. Category and Reason describe why (for metrics
// and the client error). Rewrite carries the replacement body when Action is
// Mask; it is ignored otherwise.
type Decision struct {
	Action   Action
	Category string
	Reason   string
	Rewrite  []byte
}

// Guard inspects a request before it is sent upstream. Implementations must be
// safe for concurrent use and must not block the request path on slow work.
type Guard interface {
	Inspect(ctx context.Context, req Request) Decision
}

// NopGuard allows every request unchanged. It is the default in v1.
type NopGuard struct{}

// Inspect always allows.
func (NopGuard) Inspect(context.Context, Request) Decision {
	return Decision{Action: Allow}
}

var _ Guard = NopGuard{}
