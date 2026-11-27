// Package config lädt die globale ADE-Konfiguration (~/.config/ade/config.yaml).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// DefaultListen ist die Adresse, auf der der Daemon ohne Konfiguration lauscht.
const DefaultListen = "127.0.0.1:7000"

// Config ist die globale Konfiguration des Daemons.
type Config struct {
	// Listen ist die Adresse des HTTP-Servers. Erlaubt sind nur Loopback-Adressen (ADR-008).
	Listen string `yaml:"listen"`
	// DataDir enthält SQLite-Datenbank, Daemon-Secret und Log.
	DataDir string `yaml:"data_dir"`
	// WorkspacesDir enthält die Workspace-Clones (Linux-Dateisystem, nicht /mnt/*).
	WorkspacesDir string `yaml:"workspaces_dir"`
	// Providers enthält Anbieter-Konfigurationen (ab M6/M8 genutzt).
	Providers map[string]Provider `yaml:"providers"`
	// Defaults sind Voreinstellungen für neue Agent-Sessions.
	Defaults Defaults `yaml:"defaults"`
	// IDE konfiguriert die Browser-IDE (openvscode-server).
	IDE IDE `yaml:"ide"`
	// Sandbox konfiguriert die Docker-Sandbox.
	Sandbox Sandbox `yaml:"sandbox"`
	// Approvals konfiguriert Freigaben.
	Approvals Approvals `yaml:"approvals"`
}

// DefaultApprovalTimeout ist die Wartezeit auf eine Freigabe ohne approvals.timeout.
const DefaultApprovalTimeout = 10 * time.Minute

// Approvals konfiguriert Freigaben (doc/architecture.md#43-freigaben).
type Approvals struct {
	// Timeout ist die Wartezeit auf eine Entscheidung im Dashboard; danach gilt die
	// Anfrage als abgelehnt.
	Timeout time.Duration `yaml:"timeout"`
}

// Sandbox konfiguriert die Docker-Sandbox (doc/sandbox.md).
type Sandbox struct {
	// Image ist das Standard-Image (leer: ghcr.io/davidw475/ade-agent-base passend zur
	// ADE-Version). Projekte können in .ade/sandbox.yaml ein eigenes nennen.
	Image string `yaml:"image"`
	// Docker ist das docker-Binary (leer: docker im PATH).
	Docker string `yaml:"docker"`
}

// DefaultIDEIdleTimeout ist die Zeit ohne offenen IDE-Tab, nach der ADE eine IDE stoppt.
const DefaultIDEIdleTimeout = 15 * time.Minute

// IDE konfiguriert die Browser-IDE.
type IDE struct {
	// Binary ist der Pfad zu einem eigenen openvscode-server. Leer heißt: ADE lädt die
	// festgelegte Version bei Bedarf herunter und prüft ihre Prüfsumme.
	Binary string `yaml:"binary"`
	// IdleTimeout ist die Zeit ohne Verbindung, nach der eine IDE gestoppt wird.
	IdleTimeout time.Duration `yaml:"idle_timeout"`
}

// Provider beschreibt den Zugang zu einem Modell-Anbieter.
type Provider struct {
	// APIKey ist eine Referenz wie "env:ANTHROPIC_API_KEY", nie der Klartext-Key selbst.
	APIKey string `yaml:"api_key"`
}

// ResolveAPIKey löst die API-Key-Referenz auf (siehe Resolve).
func (p Provider) ResolveAPIKey() (string, error) { return Resolve(p.APIKey) }

// Resolve löst einen Konfigurationswert auf. Werte der Form "env:VAR" werden durch den Inhalt
// der Umgebungsvariable VAR ersetzt; eine leere oder fehlende Variable ist ein Fehler.
// Andere Werte werden unverändert zurückgegeben.
func Resolve(v string) (string, error) {
	name, ok := strings.CutPrefix(v, "env:")
	if !ok {
		return v, nil
	}
	if name == "" {
		return "", errors.New(`Referenz "env:" ohne Variablennamen`)
	}
	val := os.Getenv(name)
	if val == "" {
		return "", fmt.Errorf("Umgebungsvariable %s ist nicht gesetzt", name)
	}
	return val, nil
}

// Defaults sind Voreinstellungen für neue Agent-Sessions.
type Defaults struct {
	Adapter string `yaml:"adapter"`
	Sandbox bool   `yaml:"sandbox"`
}

// DefaultPath liefert den Standardpfad der Konfigurationsdatei
// ($ADE_CONFIG, sonst $XDG_CONFIG_HOME/ade/config.yaml bzw. ~/.config/ade/config.yaml).
func DefaultPath() (string, error) {
	if p := os.Getenv("ADE_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("Konfigurationsverzeichnis ermitteln: %w", err)
	}
	return filepath.Join(dir, "ade", "config.yaml"), nil
}

// Default liefert die Konfiguration, die ohne config.yaml gilt.
func Default() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("Home-Verzeichnis ermitteln: %w", err)
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
	}, nil
}

// Load liest die Konfiguration aus path. Fehlt die Datei, gelten die Defaults.
// Angegebene Werte überschreiben die Defaults; unbekannte Schlüssel sind ein Fehler.
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
		return nil, fmt.Errorf("Konfiguration lesen: %w", err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("Konfiguration %s: %w", path, err)
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]Provider{}
	}
	for _, p := range []*string{&cfg.DataDir, &cfg.WorkspacesDir, &cfg.IDE.Binary, &cfg.Sandbox.Docker} {
		if *p, err = expandHome(*p); err != nil {
			return nil, err
		}
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("Konfiguration %s: %w", path, err)
	}
	return cfg, nil
}

// DBPath ist der Pfad der SQLite-Datenbank.
func (c *Config) DBPath() string { return filepath.Join(c.DataDir, "ade.db") }

// LockPath ist der Pfad der Lock-Datei, die einen zweiten Daemon auf demselben
// Datenverzeichnis verhindert. Sie enthält die PID des laufenden Daemons.
func (c *Config) LockPath() string { return filepath.Join(c.DataDir, "daemon.lock") }

// SecretPath ist der Pfad des Daemon-Secrets, mit dem sich die CLI beim Daemon ausweist.
func (c *Config) SecretPath() string { return filepath.Join(c.DataDir, "daemon.secret") }

// IDEDir enthält die heruntergeladene IDE, ihre Erweiterungen und die Daten je Workspace.
func (c *Config) IDEDir() string { return filepath.Join(c.DataDir, "ide") }

// LogPath ist der Pfad des Daemon-Logs, wenn der Daemon über `ade start` gestartet wurde.
func (c *Config) LogPath() string { return filepath.Join(c.DataDir, "daemon.log") }

// Port liefert den Port aus Listen.
func (c *Config) Port() string {
	_, port, _ := net.SplitHostPort(c.Listen)
	return port
}

// BaseURL ist die URL, unter der das Dashboard im Browser erreichbar ist.
// Unter WSL leitet Windows `localhost` an die WSL weiter.
func (c *Config) BaseURL() string { return "http://localhost:" + c.Port() }

func (c *Config) validate() error {
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen %q: %w", c.Listen, err)
	}
	if port == "" || port == "0" {
		return fmt.Errorf("listen %q: fester Port erforderlich", c.Listen)
	}
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("listen %q: nur Loopback-Adressen sind erlaubt (z. B. 127.0.0.1)", c.Listen)
		}
	}
	if c.DataDir == "" || !filepath.IsAbs(c.DataDir) {
		return fmt.Errorf("data_dir %q: absoluter Pfad erforderlich", c.DataDir)
	}
	if c.WorkspacesDir == "" || !filepath.IsAbs(c.WorkspacesDir) {
		return fmt.Errorf("workspaces_dir %q: absoluter Pfad erforderlich", c.WorkspacesDir)
	}
	if c.IDE.Binary != "" && !filepath.IsAbs(c.IDE.Binary) {
		return fmt.Errorf("ide.binary %q: absoluter Pfad erforderlich", c.IDE.Binary)
	}
	if strings.HasPrefix(c.Sandbox.Image, "-") || strings.ContainsAny(c.Sandbox.Image, " \t\n") {
		return fmt.Errorf("sandbox.image %q: ungültiger Image-Name", c.Sandbox.Image)
	}
	if c.IDE.IdleTimeout < time.Minute {
		return fmt.Errorf("ide.idle_timeout %s: mindestens 1m", c.IDE.IdleTimeout)
	}
	if t := c.Approvals.Timeout; t < 10*time.Second || t > 2*time.Hour {
		return fmt.Errorf("approvals.timeout %s: zwischen 10s und 2h", t)
	}
	return nil
}

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("Home-Verzeichnis ermitteln: %w", err)
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}
