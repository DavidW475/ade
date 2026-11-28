package sandbox

import (
	"fmt"
	"slices"
	"sort"

	"github.com/DavidW475/ade/internal/proxy"
)

// Presets are domain lists built into the binary for egress.presets.
//
// Without TLS interception the proxy only checks hosts, no paths or methods: “github-readonly”
// means that the container reaches GitHub but has no credentials for it – pushes and API
// write access fail because of the missing login, not at the proxy.
var Presets = map[string][]string{
	"anthropic": {"api.anthropic.com", "console.anthropic.com", "platform.claude.com", "claude.ai", "statsig.anthropic.com"},
	// Codex CLI: API, login with a ChatGPT account and model list.
	"openai": {"api.openai.com", "auth.openai.com", "chatgpt.com"},
	"github-readonly": {
		"github.com", "api.github.com", "codeload.github.com", "raw.githubusercontent.com",
		"objects.githubusercontent.com", "release-assets.githubusercontent.com", "ghcr.io", "pkg-containers.githubusercontent.com",
	},
	"npm":      {"registry.npmjs.org", "registry.yarnpkg.com"},
	"pypi":     {"pypi.org", "files.pythonhosted.org"},
	"go-proxy": {"proxy.golang.org", "sum.golang.org"},
}

// DefaultPresets apply if .ade/sandbox.yaml has no egress section.
var DefaultPresets = []string{"anthropic", "openai", "github-readonly", "npm", "pypi", "go-proxy"}

// Values for egress.on_blocked.
const (
	OnBlockedDeny = "deny"
	OnBlockedAsk  = "ask"
)

// Egress is the egress section in .ade/sandbox.yaml.
type Egress struct {
	Presets []string `yaml:"presets" json:"presets"`
	Allow   []string `yaml:"allow" json:"allow"`
	Deny    []string `yaml:"deny" json:"deny"`
	// OnBlocked: deny (default) or ask (approval in the dashboard, M7).
	OnBlocked string `yaml:"on_blocked" json:"on_blocked"`
	// Default: no egress section, DefaultPresets apply.
	Default bool `yaml:"-" json:"default"`
}

// PresetNames returns the names of all presets (sorted).
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
			return fmt.Sprintf("egress.presets: unknown preset %q (available: %v)", p, PresetNames())
		}
	}
	if _, err := proxy.CompileRules(e.Allow, e.Deny); err != nil {
		return "egress." + err.Error()
	}
	switch e.OnBlocked {
	case "", OnBlockedDeny, OnBlockedAsk:
	default:
		return fmt.Sprintf("egress.on_blocked: %q is invalid (deny or ask)", e.OnBlocked)
	}
	return ""
}

// Rules returns the effective allow and deny lists (presets resolved, without duplicates).
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
