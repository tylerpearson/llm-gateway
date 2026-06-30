package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestObserveRequest(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	m.ObserveRequest("anthropic", "haiku", 200, 100*time.Millisecond, 10, 20, 0.005, "miss")

	if v := testutil.ToFloat64(m.requests.WithLabelValues("anthropic", "haiku", "200", "miss")); v != 1 {
		t.Errorf("requests_total = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.tokens.WithLabelValues("anthropic", "haiku", "input")); v != 10 {
		t.Errorf("input tokens = %v, want 10", v)
	}
	if v := testutil.ToFloat64(m.tokens.WithLabelValues("anthropic", "haiku", "output")); v != 20 {
		t.Errorf("output tokens = %v, want 20", v)
	}
	if v := testutil.ToFloat64(m.cost.WithLabelValues("anthropic", "haiku")); v != 0.005 {
		t.Errorf("cost = %v, want 0.005", v)
	}
	if v := testutil.ToFloat64(m.cacheEvents.WithLabelValues("miss")); v != 1 {
		t.Errorf("cache miss events = %v, want 1", v)
	}
}

func TestRejectionsAndErrors(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)
	m.IncLimitRejection("key:requests_per_min")
	m.IncUpstreamError("glm")
	if v := testutil.ToFloat64(m.limitRejections.WithLabelValues("key:requests_per_min")); v != 1 {
		t.Errorf("limit rejections = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.upstreamErrors.WithLabelValues("glm")); v != 1 {
		t.Errorf("upstream errors = %v, want 1", v)
	}
}

func TestCacheLabelOffNotCounted(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)
	m.ObserveRequest("anthropic", "haiku", 200, time.Millisecond, 1, 1, 0, "")
	// "off" cache result must not increment the hit/miss cache events counter.
	if n := testutil.CollectAndCount(m.cacheEvents); n != 0 {
		t.Errorf("cache events series = %d, want 0 for cache off", n)
	}
}
