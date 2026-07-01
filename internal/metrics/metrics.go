// Package metrics defines the gateway's Prometheus instrumentation: request
// counts, latency, tokens, cost, cache effectiveness, limit rejections, and
// upstream errors. Labels are kept to bounded cardinality (provider, model,
// status); high cardinality dimensions like team and key are recorded in
// ClickHouse instead.
package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds the gateway collectors.
type Metrics struct {
	requests        *prometheus.CounterVec
	duration        *prometheus.HistogramVec
	tokens          *prometheus.CounterVec
	cost            *prometheus.CounterVec
	cacheEvents     *prometheus.CounterVec
	limitRejections *prometheus.CounterVec
	upstreamErrors  *prometheus.CounterVec
	upstreamRetries *prometheus.CounterVec
	failovers       *prometheus.CounterVec
	breakerOpen     *prometheus.GaugeVec
}

// New builds and registers the collectors on reg.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "llmgw", Name: "requests_total",
			Help: "Total proxied requests by provider, model, status, and cache result.",
		}, []string{"provider", "model", "status", "cache"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "llmgw", Name: "request_duration_seconds",
			Help: "Request latency in seconds by provider and model.", Buckets: prometheus.DefBuckets,
		}, []string{"provider", "model"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "llmgw", Name: "tokens_total",
			Help: "Total tokens by provider, model, and kind (input or output).",
		}, []string{"provider", "model", "kind"}),
		cost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "llmgw", Name: "cost_usd_total",
			Help: "Total attributed cost in USD by provider and model.",
		}, []string{"provider", "model"}),
		cacheEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "llmgw", Name: "cache_events_total",
			Help: "Cache lookups by result (hit or miss).",
		}, []string{"result"}),
		limitRejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "llmgw", Name: "limit_rejections_total",
			Help: "Hard limit rejections by exceeded scope.",
		}, []string{"scope"}),
		upstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "llmgw", Name: "upstream_errors_total",
			Help: "Upstream request failures by provider.",
		}, []string{"provider"}),
		upstreamRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "llmgw", Name: "upstream_retries_total",
			Help: "Upstream attempt retries by provider.",
		}, []string{"provider"}),
		failovers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "llmgw", Name: "failover_total",
			Help: "Failovers from a primary provider to a fallback provider.",
		}, []string{"from", "to"}),
		breakerOpen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "llmgw", Name: "breaker_open",
			Help: "Circuit breaker state per target (1 open, 0 closed).",
		}, []string{"provider", "model"}),
	}
	reg.MustRegister(m.requests, m.duration, m.tokens, m.cost, m.cacheEvents, m.limitRejections,
		m.upstreamErrors, m.upstreamRetries, m.failovers, m.breakerOpen)
	return m
}

// ObserveRequest records a completed request. cacheResult is "hit", "miss", or
// "" when the cache is not enabled.
func (m *Metrics) ObserveRequest(provider, model string, status int, dur time.Duration, inputTokens, outputTokens int, costUSD float64, cacheResult string) {
	cacheLabel := cacheResult
	if cacheLabel == "" {
		cacheLabel = "off"
	}
	m.requests.WithLabelValues(provider, model, strconv.Itoa(status), cacheLabel).Inc()
	m.duration.WithLabelValues(provider, model).Observe(dur.Seconds())
	if inputTokens > 0 {
		m.tokens.WithLabelValues(provider, model, "input").Add(float64(inputTokens))
	}
	if outputTokens > 0 {
		m.tokens.WithLabelValues(provider, model, "output").Add(float64(outputTokens))
	}
	if costUSD > 0 {
		m.cost.WithLabelValues(provider, model).Add(costUSD)
	}
	if cacheResult == "hit" || cacheResult == "miss" {
		m.cacheEvents.WithLabelValues(cacheResult).Inc()
	}
}

// IncLimitRejection counts a hard limit rejection for the given scope.
func (m *Metrics) IncLimitRejection(scope string) {
	m.limitRejections.WithLabelValues(scope).Inc()
}

// IncUpstreamError counts an upstream failure for the given provider.
func (m *Metrics) IncUpstreamError(provider string) {
	m.upstreamErrors.WithLabelValues(provider).Inc()
}

// IncUpstreamRetry counts a retried attempt against the given provider.
func (m *Metrics) IncUpstreamRetry(provider string) {
	m.upstreamRetries.WithLabelValues(provider).Inc()
}

// IncFailover counts a failover from one provider to another.
func (m *Metrics) IncFailover(from, to string) {
	m.failovers.WithLabelValues(from, to).Inc()
}

// SetBreakerOpen records whether the breaker for a target is open.
func (m *Metrics) SetBreakerOpen(provider, model string, open bool) {
	v := 0.0
	if open {
		v = 1.0
	}
	m.breakerOpen.WithLabelValues(provider, model).Set(v)
}
