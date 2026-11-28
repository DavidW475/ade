// Package config loads the global ADE configuration (~/.config/ade/config.yaml).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// DefaultListen is the address the daemon listens on without configuration.
const DefaultListen = "127.0.0.1:7000"

// Config is the global configuration of the daemon.
type Config struct {
	// Listen is the address of the HTTP server. Only loopback addresses are allowed (ADR-008).
	Listen string `yaml:"listen"`
	// DataDir contains the SQLite database, daemon secret and log.
	DataDir string `yaml:"data_dir"`
	// WorkspacesDir contains the workspace clones (Linux file system, not /mnt/*).
	WorkspacesDir string `yaml:"workspaces_dir"`
	// Providers contains provider configurations.
	Providers map[string]Provider `yaml:"providers"`
	// Defaults are default settings for new agent sessions.
	Defaults Defaults `yaml:"defaults"`
	// IDE configures the browser IDE (openvscode-server).
	IDE IDE `yaml:"ide"`
	// Sandbox configures the Docker sandbox.
	Sandbox Sandbox `yaml:"sandbox"`
	// Approvals configures approvals.
	Approvals Approvals `yaml:"approvals"`
	// Models maps aliases (e.g. "opus") to a provider and model ID (doc/factory.md#4-models-and-providers).
	Models map[string]Model `yaml:"models"`
	// Pricing extends or overrides the built-in prices; the key is a model ID or its prefix
	// (longest match wins).
	Pricing map[string]Price `yaml:"pricing"`
}

// Model is the target of a model alias.
type Model struct {
	// Provider is the provider (anthropic, openai).
	Provider string `yaml:"provider" json:"provider"`
	// ID is the model ID at the provider.
	ID string `yaml:"id" json:"id"`
}

// Price are prices in USD per million tokens.
type Price struct {
	Input      float64 `yaml:"input" json:"input"`
	Output     float64 `yaml:"output" json:"output"`
	CacheRead  float64 `yaml:"cache_read" json:"cache_read"`
	CacheWrite float64 `yaml:"cache_write" json:"cache_write"`
}

// KnownProviders are the providers ADE knows.
var KnownProviders = []string{"anthropic", "openai"}

// DefaultApprovalTimeout is the waiting time for an approval without approvals.timeout.
const DefaultApprovalTimeout = 10 * time.Minute

// Approvals configures approvals (doc/architecture.md#43-approvals).
type Approvals struct {
	// Timeout is the waiting time for a decision in the dashboard; after that the request
	// counts as denied.
	Timeout time.Duration `yaml:"timeout"`
}

// Sandbox configures the Docker sandbox (doc/sandbox.md).
type Sandbox struct {
	// Image is the default image (empty: ghcr.io/davidw475/ade-agent-base matching the ADE
	// version). Projects can name their own in .ade/sandbox.yaml.
	Image string `yaml:"image"`
	// Docker is the docker binary (empty: docker in PATH), also podman (ADE detects the
	// runtime itself, doc/sandbox.md#6-container-runtimes).
	Docker string `yaml:"docker"`
}

// DefaultIDEIdleTimeout is the time without an open IDE tab after which ADE stops an IDE.
const DefaultIDEIdleTimeout = 15 * time.Minute

// IDE configures the browser IDE.
type IDE struct {
	// Binary is the path to an own openvscode-server. Empty means: ADE downloads the pinned
	// version on demand and verifies its checksum.
	Binary string `yaml:"binary"`
	// IdleTimeout is the time without a connection after which an IDE is stopped.
	IdleTimeout time.Duration `yaml:"idle_timeout"`
}

// Provider describes the access to a model provider.
type Provider struct {
	// APIKey is a reference like "env:ANTHROPIC_API_KEY", never the plain-text key itself.
	APIKey string `yaml:"api_key"`
	// OAuthToken is a reference to a subscription token from `claude setup-token`
	// (anthropic only, not together with APIKey).
	OAuthToken string `yaml:"oauth_token"`
}

// Configured reports whether an API key or an OAuth token is configured.
func (p Provider) Configured() bool { return p.APIKey != "" || p.OAuthToken != "" }

// ResolveAPIKey resolves the API key reference (see Resolve).
func (p Provider) ResolveAPIKey() (string, error) { return Resolve(p.APIKey) }

// ResolveSecret resolves the configured credential: the OAuth token if set (oauth is true),
// otherwise the API key.
func (p Provider) ResolveSecret() (secret string, oauth bool, err error) {
	if p.OAuthToken != "" {
		v, err := Resolve(p.OAuthToken)
		return v, true, err
	}
	v, err := Resolve(p.APIKey)
	return v, false, err
}

// secretFileMaxSize limits files referenced with "file:".
const secretFileMaxSize = 64 << 10

// Resolve resolves a configuration value. Values of the form "env:VAR" are replaced by the
// contents of the environment variable VAR, values of the form "file:PATH" by the contents of
// the file (without leading and trailing whitespace; "~/" is the home directory). The file must
// only be accessible to its owner (e.g. mode 0600). An empty or missing variable or file is an
// error. Other values are returned unchanged.
func Resolve(v string) (string, error) {
	if path, ok := strings.CutPrefix(v, "file:"); ok {
		return resolveFile(path)
	}
	name, ok := strings.CutPrefix(v, "env:")
	if !ok {
		return v, nil
	}
	if name == "" {
		return "", errors.New(`reference "env:" without a variable name`)
	}
	val := os.Getenv(name)
	if val == "" {
		return "", fmt.Errorf("environment variable %s is not set", name)
	}
	return val, nil
}

func resolveFile(path string) (string, error) {
	if path == "" {
		return "", errors.New(`reference "file:" without a path`)
	}
	path, err := expandHome(path)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("file:%s: absolute path or ~/ required", path)
	}
	f, err := os.Open(path) //nolint:gosec // G304: path from the user's own configuration
	if err != nil {
		return "", fmt.Errorf("read secret: %w", err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("read secret: %w", err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("secret %s is not a regular file", path)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("secret %s is accessible to others (mode %o); run: chmod 600 %s", path, st.Mode().Perm(), path)
	}
	data, err := io.ReadAll(io.LimitReader(f, secretFileMaxSize+1))
	if err != nil {
		return "", fmt.Errorf("read secret: %w", err)
	}
	if len(data) > secretFileMaxSize {
		return "", fmt.Errorf("secret %s is larger than %d KiB", path, secretFileMaxSize>>10)
	}
	val := strings.TrimSpace(string(data))
	if val == "" {
		return "", fmt.Errorf("secret %s is empty", path)
	}
	return val, nil
}

// Defaults are default settings for new agent sessions.
type Defaults struct {
	Adapter string `yaml:"adapter"`
	Sandbox bool   `yaml:"sandbox"`
}

// DefaultPath returns the default path of the configuration file
// ($ADE_CONFIG, otherwise $XDG_CONFIG_HOME/ade/config.yaml or ~/.config/ade/config.yaml).
func DefaultPath() (string, error) {
	if p := os.Getenv("ADE_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("determine config directory: %w", err)
	}
	return filepath.Join(dir, "ade", "config.yaml"), nil
}

// Default returns the configuration that applies without config.yaml.
func Default() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("determine home directory: %w", err)
	}
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		dataHome = filepath.Join(home, ".local", "share")
	}
	return &Config{
		Listen:        DefaultListen,
		DataDir:       filepath.Join(dataHome, "ade"),
		WorkspacesDir: filepath.Join(home, ".ade", "workspaces"),
		Providers:     map[string]Provider{},
		Defaults:      Defaults{Adapter: "claude", Sandbox: true},
		IDE:           IDE{IdleTimeout: DefaultIDEIdleTimeout},
		Approvals:     Approvals{Timeout: DefaultApprovalTimeout},
		Models:        map[string]Model{},
		Pricing:       map[string]Price{},
	}, nil
}

// Load reads the configuration from path. If the file is missing, the defaults apply.
// Given values override the defaults; unknown keys are an error.
func Load(path string) (*Config, error) {
	cfg, err := Default()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return cfg, cfg.validate()
	case err != nil:
		return nil, fmt.Errorf("read configuration: %w", err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("configuration %s: %w", path, err)
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]Provider{}
	}
	if cfg.Models == nil {
		cfg.Models = map[string]Model{}
	}
	if cfg.Pricing == nil {
		cfg.Pricing = map[string]Price{}
	}
	for _, p := range []*string{&cfg.DataDir, &cfg.WorkspacesDir, &cfg.IDE.Binary, &cfg.Sandbox.Docker} {
		if *p, err = expandHome(*p); err != nil {
			return nil, err
		}
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("configuration %s: %w", path, err)
	}
	return cfg, nil
}

// DBPath is the path of the SQLite database.
func (c *Config) DBPath() string { return filepath.Join(c.DataDir, "ade.db") }

// LockPath is the path of the lock file that prevents a second daemon on the same data
// directory. It contains the PID of the running daemon.
func (c *Config) LockPath() string { return filepath.Join(c.DataDir, "daemon.lock") }

// SecretPath is the path of the daemon secret the CLI uses to authenticate with the daemon.
func (c *Config) SecretPath() string { return filepath.Join(c.DataDir, "daemon.secret") }

// IDEDir contains the downloaded IDE, its extensions and the data per workspace.
func (c *Config) IDEDir() string { return filepath.Join(c.DataDir, "ide") }

// LogPath is the path of the daemon log when the daemon was started via `ade start`.
func (c *Config) LogPath() string { return filepath.Join(c.DataDir, "daemon.log") }

// Port returns the port from Listen.
func (c *Config) Port() string {
	_, port, _ := net.SplitHostPort(c.Listen)
	return port
}

// BaseURL is the URL at which the dashboard is reachable in the browser.
// Under WSL, Windows forwards `localhost` to WSL.
func (c *Config) BaseURL() string { return "http://localhost:" + c.Port() }

func (c *Config) validate() error {
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen %q: %w", c.Listen, err)
	}
	if port == "" || port == "0" {
		return fmt.Errorf("listen %q: fixed port required", c.Listen)
	}
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("listen %q: only loopback addresses are allowed (e.g. 127.0.0.1)", c.Listen)
		}
	}
	if c.DataDir == "" || !filepath.IsAbs(c.DataDir) {
		return fmt.Errorf("data_dir %q: absolute path required", c.DataDir)
	}
	if c.WorkspacesDir == "" || !filepath.IsAbs(c.WorkspacesDir) {
		return fmt.Errorf("workspaces_dir %q: absolute path required", c.WorkspacesDir)
	}
	if c.IDE.Binary != "" && !filepath.IsAbs(c.IDE.Binary) {
		return fmt.Errorf("ide.binary %q: absolute path required", c.IDE.Binary)
	}
	if strings.HasPrefix(c.Sandbox.Image, "-") || strings.ContainsAny(c.Sandbox.Image, " \t\n") {
		return fmt.Errorf("sandbox.image %q: invalid image name", c.Sandbox.Image)
	}
	if c.IDE.IdleTimeout < time.Minute {
		return fmt.Errorf("ide.idle_timeout %s: at least 1m", c.IDE.IdleTimeout)
	}
	if t := c.Approvals.Timeout; t < 10*time.Second || t > 2*time.Hour {
		return fmt.Errorf("approvals.timeout %s: between 10s and 2h", t)
	}
	for name, m := range c.Models {
		if !validName(name) {
			return fmt.Errorf("models: invalid alias %q (letters, digits, . _ -)", name)
		}
		if !slices.Contains(KnownProviders, m.Provider) {
			return fmt.Errorf("models.%s.provider %q: unknown (allowed: %s)", name, m.Provider, strings.Join(KnownProviders, ", "))
		}
		if !validName(m.ID) {
			return fmt.Errorf("models.%s.id %q: invalid model ID", name, m.ID)
		}
	}
	for name, p := range c.Providers {
		if p.OAuthToken == "" {
			continue
		}
		if name != "anthropic" {
			return fmt.Errorf("providers.%s.oauth_token: only supported for anthropic", name)
		}
		if p.APIKey != "" {
			return fmt.Errorf("providers.%s: set either api_key or oauth_token, not both", name)
		}
	}
	for id, p := range c.Pricing {
		if !validName(id) {
			return fmt.Errorf("pricing: invalid model ID %q", id)
		}
		if p.Input < 0 || p.Output < 0 || p.CacheRead < 0 || p.CacheWrite < 0 {
			return fmt.Errorf("pricing.%s: prices must not be negative", id)
		}
	}
	return nil
}

// validName checks aliases and model IDs: not empty, no leading "-", only letters,
// digits and . _ - : / @ (e.g. "claude-opus-5-5", "gpt-5.3-codex", "openai/gpt-5").
func validName(s string) bool {
	if s == "" || len(s) > 200 || s[0] == '-' {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("._-:/@[]", r):
		default:
			return false
		}
	}
	return true
}

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine home directory: %w", err)
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}
