// Package pricing holds a versioned table of per model token rates and computes
// the USD cost of a request from its normalized token usage. Rates are USD per
// one million tokens. The table is versioned so cost rows in ClickHouse can be
// traced back to the rate card that produced them.
//
// The rates in DefaultTable are illustrative starting points and must be kept
// current against each provider's official pricing page. Treat them as config,
// not as ground truth.
package pricing

// Rates is the USD per million token rate card for one model. Cache rates are
// Anthropic specific and stay zero for providers that do not bill them.
type Rates struct {
	InputPerMTok      float64
	OutputPerMTok     float64
	CacheReadPerMTok  float64
	CacheWritePerMTok float64
}

// Table is a named, versioned set of model rates.
type Table struct {
	Version string
	Rates   map[string]Rates
}

// Cost returns the USD cost for the given token counts under model's rates and
// whether the model was found in the table. Unknown models return (0, false);
// the caller still logs the request, just with zero attributed cost.
func (t Table) Cost(model string, input, output, cacheRead, cacheWrite int) (float64, bool) {
	r, ok := t.Rates[model]
	if !ok {
		return 0, false
	}
	per := func(tokens int, rate float64) float64 {
		return float64(tokens) / 1_000_000 * rate
	}
	cost := per(input, r.InputPerMTok) +
		per(output, r.OutputPerMTok) +
		per(cacheRead, r.CacheReadPerMTok) +
		per(cacheWrite, r.CacheWritePerMTok)
	return cost, true
}

// DefaultTable returns the built in rate card. Update the version whenever rates
// change so attributed costs remain auditable.
func DefaultTable() Table {
	return Table{
		Version: "2026-06-30",
		Rates: map[string]Rates{
			// Anthropic. Cache write is the 5 minute write rate.
			"claude-haiku-4-5-20251001": {InputPerMTok: 1.00, OutputPerMTok: 5.00, CacheReadPerMTok: 0.10, CacheWritePerMTok: 1.25},
			"claude-sonnet-4-6":         {InputPerMTok: 3.00, OutputPerMTok: 15.00, CacheReadPerMTok: 0.30, CacheWritePerMTok: 3.75},
			"claude-opus-4-8":           {InputPerMTok: 15.00, OutputPerMTok: 75.00, CacheReadPerMTok: 1.50, CacheWritePerMTok: 18.75},
			// OpenAI. No separate cache billing modeled here.
			"gpt-4o":      {InputPerMTok: 2.50, OutputPerMTok: 10.00},
			"gpt-4o-mini": {InputPerMTok: 0.15, OutputPerMTok: 0.60},
		},
	}
}
