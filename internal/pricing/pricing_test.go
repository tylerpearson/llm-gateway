package pricing

import (
	"math"
	"testing"
)

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCost(t *testing.T) {
	tbl := Table{Rates: map[string]Rates{
		"m": {InputPerMTok: 3, OutputPerMTok: 15, CacheReadPerMTok: 0.3, CacheWritePerMTok: 3.75},
	}}

	tests := []struct {
		name                            string
		model                           string
		in, out, cacheR, cacheW         int
		want                            float64
		wantKnown                       bool
	}{
		{"one million in and out", "m", 1_000_000, 1_000_000, 0, 0, 18, true},
		{"half million input", "m", 500_000, 0, 0, 0, 1.5, true},
		{"cache tokens", "m", 0, 0, 1_000_000, 1_000_000, 0.3 + 3.75, true},
		{"zero tokens", "m", 0, 0, 0, 0, 0, true},
		{"unknown model", "ghost", 1_000_000, 0, 0, 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, known := tbl.Cost(tc.model, tc.in, tc.out, tc.cacheR, tc.cacheW)
			if known != tc.wantKnown {
				t.Fatalf("known = %v, want %v", known, tc.wantKnown)
			}
			if !almostEqual(got, tc.want) {
				t.Errorf("cost = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDefaultTable(t *testing.T) {
	tbl := DefaultTable()
	if tbl.Version == "" {
		t.Error("default table has no version")
	}
	if _, ok := tbl.Rates["claude-haiku-4-5-20251001"]; !ok {
		t.Error("default table missing claude-haiku-4-5-20251001")
	}
	cost, known := tbl.Cost("claude-opus-4-8", 1_000_000, 0, 0, 0)
	if !known || cost <= 0 {
		t.Errorf("opus input cost = %v, known %v; want positive", cost, known)
	}
}

func TestContextWindow(t *testing.T) {
	tbl := DefaultTable()
	if w, ok := tbl.ContextWindow("gpt-4o-mini"); !ok || w != 128_000 {
		t.Errorf("gpt-4o-mini window = %d, known %v; want 128000, true", w, ok)
	}
	if w, ok := tbl.ContextWindow("claude-opus-4-8"); !ok || w != 200_000 {
		t.Errorf("claude-opus-4-8 window = %d, known %v; want 200000, true", w, ok)
	}
	if _, ok := tbl.ContextWindow("unlisted-model"); ok {
		t.Error("unlisted model should report an unknown window")
	}
	// A non-positive window is treated as unknown.
	empty := Table{ContextWindows: map[string]int{"x": 0}}
	if _, ok := empty.ContextWindow("x"); ok {
		t.Error("zero window should report unknown")
	}
}
