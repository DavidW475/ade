// Package provider bundles the knowledge about model providers (doc/architecture.md#2-components):
// which adapter talks to which provider, model aliases from the configuration and the
// cost calculation from token counts.
package provider

import (
	"math"
	"sort"
	"strings"

	"github.com/DavidW475/ade/internal/config"
)

// Providers.
const (
	Anthropic = "anthropic"
	OpenAI    = "openai"
)

// ForAdapter returns the provider an adapter talks to (empty: none, e.g. pty).
func ForAdapter(adapter string) string {
	switch adapter {
	case "claude":
		return Anthropic
	case "codex":
		return OpenAI
	}
	return ""
}

// Usage are token counts of one or more API calls. Input only counts tokens that were
// neither read from nor written to the prompt cache (Anthropic counting; OpenAI includes read
// cache tokens in input_tokens, the parsers subtract them).
type Usage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
}

// Add adds u and v.
func (u Usage) Add(v Usage) Usage {
	return Usage{u.Input + v.Input, u.Output + v.Output, u.CacheRead + v.CacheRead, u.CacheWrite + v.CacheWrite}
}

// Sub returns u - v, never negative.
func (u Usage) Sub(v Usage) Usage {
	return Usage{max(u.Input-v.Input, 0), max(u.Output-v.Output, 0), max(u.CacheRead-v.CacheRead, 0), max(u.CacheWrite-v.CacheWrite, 0)}
}

// Zero reports whether no tokens are counted.
func (u Usage) Zero() bool { return u == Usage{} }

// PricesAsOf is the date of the built-in prices. They are estimates; the providers' pricing
// pages are authoritative. Deviations belong in pricing in config.yaml.
const PricesAsOf = "2026-11"

// builtinPrices are list prices in USD per million tokens, the key is the start of the model
// ID (longest match wins). Anthropic: cache read 10 %, write 125 % of the input price;
// OpenAI: no surcharge for writing.
var builtinPrices = map[string]config.Price{
	// Anthropic
	"claude-fable":      anthropic(10, 50),
	"claude-opus-5-5":   anthropic(4, 20),
	"claude-opus-5":     anthropic(5, 25),
	"claude-opus-4-5":   anthropic(5, 25),
	"claude-opus-4-6":   anthropic(5, 25),
	"claude-opus-4-7":   anthropic(5, 25),
	"claude-opus-4-8":   anthropic(5, 25),
	"claude-opus-4":     anthropic(15, 75),
	"claude-sonnet-5-5": anthropic(2, 10),
	"claude-sonnet":     anthropic(3, 15),
	"claude-haiku-4-5":  anthropic(1, 5),
	"claude-haiku":      anthropic(1, 5),
	// OpenAI
	"gpt-5.5":       openai(5, 0.5, 30),
	"gpt-5.3-codex": openai(1.75, 0.175, 14),
	"gpt-5-mini":    openai(0.25, 0.025, 2),
	"gpt-5-nano":    openai(0.05, 0.005, 0.4),
	"gpt-5":         openai(1.25, 0.125, 10),
}

func anthropic(in, out float64) config.Price {
	return config.Price{Input: in, Output: out, CacheRead: in / 10, CacheWrite: in * 1.25}
}

func openai(in, cached, out float64) config.Price {
	return config.Price{Input: in, Output: out, CacheRead: cached, CacheWrite: in}
}

// Catalog resolves aliases and computes costs.
type Catalog struct {
	models map[string]config.Model
	prices map[string]config.Price
	keys   []string // price keys, longest first
}

// NewCatalog creates a catalog from the configuration (nil: built-in prices only).
func NewCatalog(cfg *config.Config) *Catalog {
	c := &Catalog{models: map[string]config.Model{}, prices: map[string]config.Price{}}
	for k, v := range builtinPrices {
		c.prices[k] = v
	}
	if cfg != nil {
		for k, v := range cfg.Models {
			c.models[k] = v
		}
		for k, v := range cfg.Pricing {
			c.prices[k] = v
		}
	}
	for k := range c.prices {
		c.keys = append(c.keys, k)
	}
	sort.Slice(c.keys, func(i, j int) bool {
		if len(c.keys[i]) != len(c.keys[j]) {
			return len(c.keys[i]) > len(c.keys[j])
		}
		return c.keys[i] < c.keys[j]
	})
	return c
}

// Alias is a model alias for the API.
type Alias struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

// Aliases returns all aliases, sorted by name.
func (c *Catalog) Aliases() []Alias {
	out := make([]Alias, 0, len(c.models))
	for n, m := range c.models {
		out = append(out, Alias{Name: n, Provider: m.Provider, ID: m.ID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Resolve resolves an alias. If name is not an alias, it returns name unchanged with an
// empty provider (the CLI may know the model itself).
func (c *Catalog) Resolve(name string) (id, prov string, alias bool) {
	if m, ok := c.models[name]; ok {
		return m.ID, m.Provider, true
	}
	return name, "", false
}

// Price returns the price of a model (longest matching start of the ID).
func (c *Catalog) Price(model string) (config.Price, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	// Ignore provider prefixes like "anthropic/" or "openai/" (LiteLLM, OpenRouter).
	if i := strings.LastIndexByte(model, '/'); i >= 0 {
		model = model[i+1:]
	}
	if model == "" {
		return config.Price{}, false
	}
	for _, k := range c.keys {
		if strings.HasPrefix(model, strings.ToLower(k)) {
			return c.prices[k], true
		}
	}
	return config.Price{}, false
}

// Cost computes the costs in USD. priced is false if the model's price is unknown.
func (c *Catalog) Cost(model string, u Usage) (usd float64, priced bool) {
	p, ok := c.Price(model)
	if !ok {
		return 0, false
	}
	usd = (float64(u.Input)*p.Input + float64(u.Output)*p.Output +
		float64(u.CacheRead)*p.CacheRead + float64(u.CacheWrite)*p.CacheWrite) / 1e6
	return math.Round(usd*1e8) / 1e8, true
}
