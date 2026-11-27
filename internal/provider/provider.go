// Package provider bündelt das Wissen über Modell-Anbieter (doc/architecture.md#2-komponenten):
// welcher Adapter welchen Anbieter spricht, Modell-Aliase aus der Konfiguration und die
// Kostenberechnung aus Token-Zahlen.
package provider

import (
	"math"
	"sort"
	"strings"

	"github.com/DavidW475/ade/internal/config"
)

// Anbieter.
const (
	Anthropic = "anthropic"
	OpenAI    = "openai"
)

// ForAdapter liefert den Anbieter, den ein Adapter spricht (leer: keiner, z. B. pty).
func ForAdapter(adapter string) string {
	switch adapter {
	case "claude":
		return Anthropic
	case "codex":
		return OpenAI
	}
	return ""
}

// Usage sind Token-Zahlen eines oder mehrerer API-Aufrufe. Input zählt nur Tokens, die
// weder aus dem Prompt-Cache gelesen noch in ihn geschrieben wurden (Anthropic-Zählweise;
// OpenAI zählt gelesene Cache-Tokens in input_tokens mit, das rechnen die Parser heraus).
type Usage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
}

// Add addiert u und v.
func (u Usage) Add(v Usage) Usage {
	return Usage{u.Input + v.Input, u.Output + v.Output, u.CacheRead + v.CacheRead, u.CacheWrite + v.CacheWrite}
}

// Sub liefert u - v, nie negativ.
func (u Usage) Sub(v Usage) Usage {
	return Usage{max(u.Input-v.Input, 0), max(u.Output-v.Output, 0), max(u.CacheRead-v.CacheRead, 0), max(u.CacheWrite-v.CacheWrite, 0)}
}

// Zero meldet, ob keine Tokens gezählt sind.
func (u Usage) Zero() bool { return u == Usage{} }

// PricesAsOf ist der Stand der eingebauten Preise. Sie sind Schätzwerte; maßgeblich sind die
// Preisseiten der Anbieter. Abweichungen gehören in pricing in config.yaml.
const PricesAsOf = "2026-11"

// builtinPrices sind Listenpreise in USD je Million Tokens, Schlüssel ist der Anfang der
// Modell-ID (längster Treffer gilt). Anthropic: Cache lesen 10 %, schreiben 125 % des
// Eingabepreises; OpenAI: kein Aufpreis fürs Schreiben.
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

// Catalog löst Aliase auf und berechnet Kosten.
type Catalog struct {
	models map[string]config.Model
	prices map[string]config.Price
	keys   []string // Preis-Schlüssel, längste zuerst
}

// NewCatalog erzeugt einen Katalog aus der Konfiguration (nil: nur eingebaute Preise).
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

// Alias ist ein Modell-Alias für die API.
type Alias struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

// Aliases liefert alle Aliase, sortiert nach Name.
func (c *Catalog) Aliases() []Alias {
	out := make([]Alias, 0, len(c.models))
	for n, m := range c.models {
		out = append(out, Alias{Name: n, Provider: m.Provider, ID: m.ID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Resolve löst einen Alias auf. Ist name kein Alias, liefert es name unverändert mit
// leerem Anbieter (die CLI kennt das Modell vielleicht selbst).
func (c *Catalog) Resolve(name string) (id, prov string, alias bool) {
	if m, ok := c.models[name]; ok {
		return m.ID, m.Provider, true
	}
	return name, "", false
}

// Price liefert den Preis eines Modells (längster passender Anfang der ID).
func (c *Catalog) Price(model string) (config.Price, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	// Anbieter-Präfixe wie "anthropic/" oder "openai/" (LiteLLM, OpenRouter) ignorieren.
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

// Cost berechnet die Kosten in USD. priced ist false, wenn der Preis des Modells unbekannt ist.
func (c *Catalog) Cost(model string, u Usage) (usd float64, priced bool) {
	p, ok := c.Price(model)
	if !ok {
		return 0, false
	}
	usd = (float64(u.Input)*p.Input + float64(u.Output)*p.Output +
		float64(u.CacheRead)*p.CacheRead + float64(u.CacheWrite)*p.CacheWrite) / 1e6
	return math.Round(usd*1e8) / 1e8, true
}
