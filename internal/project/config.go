// Package project verwaltet registrierte Projekte und ihre Konfiguration (.ade/project.yaml).
package project

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/DavidW475/ade/internal/approval"
	"github.com/DavidW475/ade/internal/git"
)

// ConfigFile ist der Pfad der Projekt-Konfiguration relativ zum Repo.
const ConfigFile = ".ade/project.yaml"

// KnownAdapters sind die Adapter, die in project.yaml erlaubt sind.
var KnownAdapters = []string{"claude", "codex", "pty"}

// Config ist der Inhalt von .ade/project.yaml. Alle Felder sind optional.
type Config struct {
	// Name ist der Anzeigename; Default ist der Verzeichnisname des Repos.
	Name string `yaml:"name" json:"name,omitempty"`
	// BaseBranch ist der Branch, von dem Workspaces abzweigen; Default ist der Branch,
	// der beim Registrieren ermittelt wurde.
	BaseBranch string `yaml:"base_branch" json:"base_branch,omitempty"`
	// Adapter ist der Standard-Adapter für neue Agent-Sessions.
	Adapter string `yaml:"adapter" json:"adapter,omitempty"`
	// Setup sind Shell-Befehle, die nach dem Anlegen eines Workspaces darin laufen.
	Setup []string `yaml:"setup" json:"setup,omitempty"`
	// Approvals legt fest, welche Werkzeuge ohne Rückfrage laufen (Sandbox/lokal).
	Approvals *approval.Config `yaml:"approvals" json:"approvals,omitempty"`
	// Budget begrenzt die Kosten der Agenten (M8).
	Budget *Budget `yaml:"budget" json:"budget,omitempty"`
	// Triggers starten Fabrik-Runs automatisch (M10).
	Triggers []Trigger `yaml:"triggers" json:"triggers,omitempty"`
}

// Budget sind Kostengrenzen in USD; 0 heißt keine Grenze.
type Budget struct {
	// SessionUSD gilt je Session (überschreibbar beim Start).
	SessionUSD float64 `yaml:"session_usd" json:"session_usd,omitempty"`
	// MonthlyUSD gilt für alle Sessions des Projekts im Kalendermonat (UTC).
	MonthlyUSD float64 `yaml:"monthly_usd" json:"monthly_usd,omitempty"`
}

// MaxBudgetUSD ist die größte erlaubte Budget-Angabe.
const MaxBudgetUSD = 1e6

// SessionBudget liefert das Session-Budget (0: keins); c darf nil sein.
func (c *Config) SessionBudget() float64 {
	if c == nil || c.Budget == nil {
		return 0
	}
	return c.Budget.SessionUSD
}

// MonthlyBudget liefert das Monatsbudget (0: keins); c darf nil sein.
func (c *Config) MonthlyBudget() float64 {
	if c == nil || c.Budget == nil {
		return 0
	}
	return c.Budget.MonthlyUSD
}

// ConfigError ist eine ungültige project.yaml.
type ConfigError struct {
	Path string
	Msg  string
}

func (e *ConfigError) Error() string { return fmt.Sprintf("%s: %s", e.Path, e.Msg) }

// LoadConfig liest .ade/project.yaml aus repo. Fehlt die Datei, ist das Ergebnis eine
// leere Konfiguration ohne Fehler. Ungültige Dateien liefern einen *ConfigError.
func LoadConfig(ctx context.Context, repo string) (*Config, error) {
	path := filepath.Join(repo, ConfigFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return &Config{}, &ConfigError{Path: ConfigFile, Msg: err.Error()}
	}
	return ParseConfig(ctx, data)
}

// ParseConfig prüft und liest den Inhalt einer project.yaml.
func ParseConfig(ctx context.Context, data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return &Config{}, &ConfigError{Path: ConfigFile, Msg: strings.TrimPrefix(err.Error(), "yaml: ")}
	}
	if msg := cfg.validate(ctx); msg != "" {
		return &Config{}, &ConfigError{Path: ConfigFile, Msg: msg}
	}
	return &cfg, nil
}

func (c *Config) validate(ctx context.Context) string {
	c.Name = strings.TrimSpace(c.Name)
	if len(c.Name) > 100 {
		return "name: höchstens 100 Zeichen"
	}
	if c.BaseBranch != "" && !git.ValidBranchName(ctx, c.BaseBranch) {
		return fmt.Sprintf("base_branch: %q ist kein gültiger Branch-Name", c.BaseBranch)
	}
	if c.Adapter != "" && !slices.Contains(KnownAdapters, c.Adapter) {
		return fmt.Sprintf("adapter: %q ist unbekannt (erlaubt: %s)", c.Adapter, strings.Join(KnownAdapters, ", "))
	}
	for i, cmd := range c.Setup {
		if strings.TrimSpace(cmd) == "" {
			return fmt.Sprintf("setup[%d]: leerer Befehl", i)
		}
	}
	if err := c.Approvals.Validate(); err != nil {
		return err.Error()
	}
	if msg := validateTriggers(c.Triggers); msg != "" {
		return msg
	}
	if b := c.Budget; b != nil {
		for _, v := range []struct {
			name string
			v    float64
		}{{"budget.session_usd", b.SessionUSD}, {"budget.monthly_usd", b.MonthlyUSD}} {
			if v.v < 0 || v.v > MaxBudgetUSD || v.v != v.v {
				return fmt.Sprintf("%s: zwischen 0 und %.0f", v.name, MaxBudgetUSD)
			}
		}
	}
	return ""
}
