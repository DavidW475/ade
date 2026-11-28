package provider

import (
	"math"
	"testing"

	"github.com/DavidW475/ade/internal/config"
)

func TestCost(t *testing.T) {
	c := NewCatalog(&config.Config{
		Pricing: map[string]config.Price{"claude-sonnet-4-5": {Input: 1, Output: 2, CacheRead: 0.5, CacheWrite: 4}},
		Models:  map[string]config.Model{"opus": {Provider: Anthropic, ID: "claude-opus-5-5"}},
	})
	cases := []struct {
		model string
		u     Usage
		want  float64
		ok    bool
	}{
		// overridden (longest match beats "claude-sonnet")
		{"claude-sonnet-4-5-20250929", Usage{Input: 1e6, Output: 1e6, CacheRead: 1e6, CacheWrite: 1e6}, 7.5, true},
		{"claude-sonnet-4-6", Usage{Input: 1e6}, 3, true},
		{"claude-opus-4-1", Usage{Output: 1e6}, 75, true},
		{"claude-opus-4-5-20251101", Usage{Output: 1e6}, 25, true},
		{"anthropic/claude-haiku-4-5", Usage{Input: 2e6, CacheRead: 1e6}, 2.1, true},
		{"GPT-5.3-Codex", Usage{Input: 1e6, CacheRead: 1e6}, 1.925, true},
		{"gpt-5-mini-2025", Usage{Output: 1e6}, 2, true},
		{"llama-3", Usage{Input: 1e6}, 0, false},
		{"", Usage{Input: 1}, 0, false},
	}
	for _, tc := range cases {
		got, ok := c.Cost(tc.model, tc.u)
		if ok != tc.ok || math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("Cost(%q) = %v, %v; want %v, %v", tc.model, got, ok, tc.want, tc.ok)
		}
	}
	if id, prov, alias := c.Resolve("opus"); id != "claude-opus-5-5" || prov != Anthropic || !alias {
		t.Errorf("Resolve(opus) = %s %s %v", id, prov, alias)
	}
	if id, _, alias := c.Resolve("claude-x"); id != "claude-x" || alias {
		t.Errorf("Resolve(claude-x) = %s %v", id, alias)
	}
	if a := c.Aliases(); len(a) != 1 || a[0].Name != "opus" {
		t.Errorf("Aliases = %+v", a)
	}
}

func TestUsageMath(t *testing.T) {
	a := Usage{Input: 5, Output: 3, CacheRead: 2, CacheWrite: 1}
	if got := a.Add(a).Sub(Usage{Input: 20}); got != (Usage{Input: 0, Output: 6, CacheRead: 4, CacheWrite: 2}) {
		t.Errorf("Add/Sub = %+v", got)
	}
	if !(Usage{}).Zero() || a.Zero() {
		t.Error("Zero")
	}
	if ForAdapter("codex") != OpenAI || ForAdapter("claude") != Anthropic || ForAdapter("pty") != "" {
		t.Error("ForAdapter")
	}
}
