// Package eval defines the v2 evaluation seams that are present but inert in
// v1. A MirrorHook is invoked post-routing so a future shadow-traffic
// evaluator can compare a candidate model against the served one without
// affecting the live response. v1 ships only the interface and a no-op
// implementation; the ClickHouse eval_runs and eval_results tables exist but
// are unused.
package eval

import "context"

// MirrorRequest is the post-routing snapshot handed to a MirrorHook. Body is the
// request body sent upstream; hooks must treat it as read only.
type MirrorRequest struct {
	RequestID      string
	KeyID          string
	TeamID         string
	RequestedModel string
	ServedModel    string
	Provider       string
	Body           []byte
}

// MirrorHook receives routed requests for future shadow evaluation. It must not
// block the request path; implementations are expected to hand work off
// asynchronously.
type MirrorHook interface {
	Mirror(ctx context.Context, req MirrorRequest)
}

// NopHook is the default no-op hook used in v1.
type NopHook struct{}

// Mirror does nothing.
func (NopHook) Mirror(context.Context, MirrorRequest) {}

var _ MirrorHook = NopHook{}
