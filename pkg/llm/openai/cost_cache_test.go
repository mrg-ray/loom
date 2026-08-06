package openai

import (
	"math"
	"net/http"
	"testing"
)

// The OpenAI-compatible shape reports prompt_tokens INCLUSIVE of cached tokens,
// so a cache-blind total over-charges a cache-heavy call several fold. These are
// the real counts from a 20-turn ContextCompilation benchmark run.
func TestCalculateCost_CacheTiersAreBilledSeparately(t *testing.T) {
	c := &Client{model: "coding-agent/claude-sonnet-4-6"}
	const (
		promptTokens = 950830 // includes the two cache buckets below
		cacheRead    = 872023
		cacheWrite   = 78708
		output       = 11140
	)
	got := c.calculateCost(promptTokens, output, cacheRead, cacheWrite)

	// 99 uncached@1.0x + 78,708 write@1.25x + 872,023 read@0.10x + 11,140 out
	// at the $2.50/$10 fallback rates for this un-catalogued model id.
	want := (99*2.50 + 78708*2.50*1.25 + 872023*2.50*0.10 + 11140*10.00) / 1e6
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("cache-aware cost = %.6f, want %.6f", got, want)
	}
	// A cache-blind total would bill every input token at the full rate.
	if blind := (promptTokens*2.50 + output*10.00) / 1e6; got >= blind {
		t.Fatalf("cache tiers not applied: got %.6f, cache-blind would be %.6f", got, blind)
	}
}

// The gateway computes its own cache-aware cost; it is authoritative.
func TestProviderCostHeaderWins(t *testing.T) {
	h := http.Header{}
	h.Set(providerCostHeader, "0.01810725")
	if got := parseProviderCost(h); got != 0.01810725 {
		t.Fatalf("parseProviderCost = %v", got)
	}
	if got := costOrEstimate(parseProviderCost(h), func() float64 { return 99.0 }); got != 0.01810725 {
		t.Fatalf("provider cost must win, got %v", got)
	}
	// Absent or unusable header falls back to the estimate.
	for _, bad := range []string{"", "not-a-number", "-1"} {
		hh := http.Header{}
		hh.Set(providerCostHeader, bad)
		if got := costOrEstimate(parseProviderCost(hh), func() float64 { return 42.0 }); got != 42.0 {
			t.Fatalf("header %q: expected fallback, got %v", bad, got)
		}
	}
}
