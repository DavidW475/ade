package sandbox

import (
	"fmt"
	"slices"
	"sort"

	"github.com/DavidW475/ade/internal/proxy"
)

// Presets sind im Binary hinterlegte Domain-Listen für egress.presets.
//
// Ohne TLS-Aufbrechen prüft der Proxy nur Hosts, keine Pfade oder Methoden: „github-readonly“
// heißt, dass der Container GitHub erreicht, aber keine Zugangsdaten dafür hat – Pushes und
// API-Schreibzugriffe scheitern an der fehlenden Anmeldung, nicht am Proxy.
var Presets = map[string][]string{
	"anthropic": {"api.anthropic.com", "console.anthropic.com", "platform.claude.com", "claude.ai", "statsig.anthropic.com"},
	"github-readonly": {
		"github.com", "api.github.com", "codeload.github.com", "raw.githubusercontent.com",
		"objects.githubusercontent.com", "release-assets.githubusercontent.com", "ghcr.io", "pkg-containers.githubusercontent.com",
	},
	"npm":      {"registry.npmjs.org", "registry.yarnpkg.com"},
	"pypi":     {"pypi.org", "files.pythonhosted.org"},
	"go-proxy": {"proxy.golang.org", "sum.golang.org"},
}

// DefaultPresets gelten, wenn .ade/sandbox.yaml keinen egress-Abschnitt hat.
var DefaultPresets = []string{"anthropic", "github-readonly", "npm", "pypi", "go-proxy"}

// Werte für egress.on_blocked.
const (
	OnBlockedDeny = "deny"
	OnBlockedAsk  = "ask"
)

// Egress ist der Abschnitt egress in .ade/sandbox.yaml.
type Egress struct {
	Presets []string `yaml:"presets" json:"presets"`
	Allow   []string `yaml:"allow" json:"allow"`
	Deny    []string `yaml:"deny" json:"deny"`
	// OnBlocked: deny (Standard) oder ask (Freigabe im Dashboard, M7).
	OnBlocked string `yaml:"on_blocked" json:"on_blocked"`
	// Default: kein egress-Abschnitt, es gelten DefaultPresets.
	Default bool `yaml:"-" json:"default"`
}

// PresetNames liefert die Namen aller Presets (sortiert).
func PresetNames() []string {
	names := make([]string, 0, len(Presets))
	for k := range Presets {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func defaultEgress() *Egress {
	return &Egress{Presets: slices.Clone(DefaultPresets), OnBlocked: OnBlockedDeny, Default: true}
}

func (e *Egress) validate() string {
	for _, p := range e.Presets {
		if _, ok := Presets[p]; !ok {
			return fmt.Sprintf("egress.presets: unbekanntes Preset %q (verfügbar: %v)", p, PresetNames())
		}
	}
	if _, err := proxy.CompileRules(e.Allow, e.Deny); err != nil {
		return "egress." + err.Error()
	}
	switch e.OnBlocked {
	case "", OnBlockedDeny, OnBlockedAsk:
	default:
		return fmt.Sprintf("egress.on_blocked: %q ist ungültig (deny oder ask)", e.OnBlocked)
	}
	return ""
}

// Rules liefert die wirksamen Allow- und Deny-Listen (Presets aufgelöst, ohne Doppelte).
func (e *Egress) Rules() (allow, deny []string) {
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			allow = append(allow, s)
		}
	}
	for _, p := range e.Presets {
		for _, h := range Presets[p] {
			add(h)
		}
	}
	for _, h := range e.Allow {
		add(h)
	}
	return allow, slices.Clone(e.Deny)
}
