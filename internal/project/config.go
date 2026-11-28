// Package project manages registered projects and their configuration (.ade/project.yaml).
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

// ConfigFile is the path of the project configuration relative to the repository.
const ConfigFile = ".ade/project.yaml"

// KnownAdapters are the adapters allowed in project.yaml.
var KnownAdapters = []string{"claude", "codex", "pty"}

// Config is the contents of .ade/project.yaml. All fields are optional.
type Config struct {
	// Name is the display name; the default is the repository's directory name.
	Name string `yaml:"name" json:"name,omitempty"`
	// BaseBranch is the branch workspaces start from; the default is the branch determined
	// at registration.
	BaseBranch string `yaml:"base_branch" json:"base_branch,omitempty"`
	// Adapter is the default adapter for new agent sessions.
	Adapter string `yaml:"adapter" json:"adapter,omitempty"`
	// Setup are shell commands that run in a workspace after it was created.
	Setup []string `yaml:"setup" json:"setup,omitempty"`
	// Approvals defines which tools run without asking (sandbox/local).
	Approvals *approval.Config `yaml:"approvals" json:"approvals,omitempty"`
	// Budget limits the agents' costs (M8).
	Budget *Budget `yaml:"budget" json:"budget,omitempty"`
	// Triggers start factory runs automatically (M10).
	Triggers []Trigger `yaml:"triggers" json:"triggers,omitempty"`
}

// Budget are cost limits in USD; 0 means no limit.
type Budget struct {
	// SessionUSD applies per session (can be overridden at start).
	SessionUSD float64 `yaml:"session_usd" json:"session_usd,omitempty"`
	// MonthlyUSD applies to all sessions of the project in the calendar month (UTC).
	MonthlyUSD float64 `yaml:"monthly_usd" json:"monthly_usd,omitempty"`
}

// MaxBudgetUSD is the largest allowed budget value.
const MaxBudgetUSD = 1e6

// SessionBudget returns the session budget (0: none); c may be nil.
func (c *Config) SessionBudget() float64 {
	if c == nil || c.Budget == nil {
		return 0
	}
	return c.Budget.SessionUSD
}

// MonthlyBudget returns the monthly budget (0: none); c may be nil.
func (c *Config) MonthlyBudget() float64 {
	if c == nil || c.Budget == nil {
		return 0
	}
	return c.Budget.MonthlyUSD
}

// ConfigError is an invalid project.yaml.
type ConfigError struct {
	Path string
	Msg  string
}

func (e *ConfigError) Error() string { return fmt.Sprintf("%s: %s", e.Path, e.Msg) }

// LoadConfig reads .ade/project.yaml from repo. If the file is missing, the result is an
// empty configuration without an error. Invalid files return a *ConfigError.
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

// ParseConfig checks and reads the contents of a project.yaml.
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
		return "name: at most 100 characters"
	}
	if c.BaseBranch != "" && !git.ValidBranchName(ctx, c.BaseBranch) {
		return fmt.Sprintf("base_branch: %q is not a valid branch name", c.BaseBranch)
	}
	if c.Adapter != "" && !slices.Contains(KnownAdapters, c.Adapter) {
		return fmt.Sprintf("adapter: %q is unknown (allowed: %s)", c.Adapter, strings.Join(KnownAdapters, ", "))
	}
	for i, cmd := range c.Setup {
		if strings.TrimSpace(cmd) == "" {
			return fmt.Sprintf("setup[%d]: empty command", i)
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
				return fmt.Sprintf("%s: between 0 and %.0f", v.name, MaxBudgetUSD)
			}
		}
	}
	return ""
}
