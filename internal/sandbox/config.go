package sandbox

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ConfigFile ist der Pfad der Sandbox-Konfiguration relativ zum Projekt-Repo.
const ConfigFile = ".ade/sandbox.yaml"

// Voreinstellungen für die Ressourcen eines Containers.
const (
	DefaultMemory = "4g"
	DefaultCPUs   = 2.0
	DefaultPIDs   = 512
)

// ImageRepo ist das Basis-Image aus images/agent-base.
const ImageRepo = "ghcr.io/davidw475/ade-agent-base"

// DefaultImage liefert das Basis-Image passend zur ADE-Version: Releases (v1.2.3) nutzen
// das gleich versionierte Image, Entwicklungsstände :latest.
func DefaultImage(version string) string {
	if m := releaseRE.FindStringSubmatch(version); m != nil {
		return ImageRepo + ":" + m[1]
	}
	return ImageRepo + ":latest"
}

var releaseRE = regexp.MustCompile(`^v(\d+\.\d+\.\d+)$`)

// Resources begrenzt einen Container.
type Resources struct {
	// Memory im Format von docker --memory (z. B. 4g, 512m).
	Memory string  `yaml:"memory" json:"memory"`
	CPUs   float64 `yaml:"cpus" json:"cpus"`
	PIDs   int     `yaml:"pids" json:"pids"`
}

// Config ist der Inhalt von .ade/sandbox.yaml. Alle Felder sind optional.
type Config struct {
	// Image überschreibt das Standard-Image (z. B. ein vom Basis-Image abgeleitetes).
	Image     string    `yaml:"image" json:"image,omitempty"`
	Resources Resources `yaml:"resources" json:"resources"`
	// Egress sind die Netzwerk-Regeln (nil in der Datei: Voreinstellung, siehe DefaultPresets).
	Egress *Egress `yaml:"egress" json:"egress"`
}

// ConfigError ist eine ungültige sandbox.yaml.
type ConfigError struct{ Msg string }

func (e *ConfigError) Error() string { return ConfigFile + ": " + e.Msg }

// LoadConfig liest .ade/sandbox.yaml aus repo; fehlt die Datei, gelten die Voreinstellungen.
// Ungültige Dateien liefern die Voreinstellungen und einen *ConfigError.
func LoadConfig(repo string) (*Config, error) {
	data, err := os.ReadFile(filepath.Join(repo, ConfigFile)) //nolint:gosec // G304: Pfad im registrierten Repo
	if errors.Is(err, os.ErrNotExist) {
		return withDefaults(&Config{}), nil
	}
	if err != nil {
		return withDefaults(&Config{}), &ConfigError{Msg: err.Error()}
	}
	return ParseConfig(data)
}

// ParseConfig prüft und liest den Inhalt einer sandbox.yaml.
func ParseConfig(data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return withDefaults(&Config{}), &ConfigError{Msg: strings.TrimPrefix(err.Error(), "yaml: ")}
	}
	if msg := cfg.validate(); msg != "" {
		return withDefaults(&Config{}), &ConfigError{Msg: msg}
	}
	return withDefaults(&cfg), nil
}

func withDefaults(c *Config) *Config {
	if c.Resources.Memory == "" {
		c.Resources.Memory = DefaultMemory
	}
	if c.Resources.CPUs == 0 {
		c.Resources.CPUs = DefaultCPUs
	}
	if c.Resources.PIDs == 0 {
		c.Resources.PIDs = DefaultPIDs
	}
	if c.Egress == nil {
		c.Egress = defaultEgress()
	} else if c.Egress.OnBlocked == "" {
		c.Egress.OnBlocked = OnBlockedDeny
	}
	return c
}

var (
	memoryRE = regexp.MustCompile(`^[0-9]+[bkmgBKMG]?$`)
	imageRE  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:@-]*$`)
)

func (c *Config) validate() string {
	if c.Egress != nil {
		if msg := c.Egress.validate(); msg != "" {
			return msg
		}
	}
	c.Image = strings.TrimSpace(c.Image)
	if c.Image != "" && (len(c.Image) > 255 || !imageRE.MatchString(c.Image)) {
		return fmt.Sprintf("image: %q ist kein gültiger Image-Name", c.Image)
	}
	if m := c.Resources.Memory; m != "" && !memoryRE.MatchString(m) {
		return fmt.Sprintf("resources.memory: %q ist ungültig (z. B. 4g oder 512m)", m)
	}
	if v := c.Resources.CPUs; v < 0 || v > 256 {
		return fmt.Sprintf("resources.cpus: %v ist ungültig (0 < cpus ≤ 256)", v)
	}
	if v := c.Resources.PIDs; v < 0 || (v > 0 && v < 32) || v > 1<<20 {
		return fmt.Sprintf("resources.pids: %d ist ungültig (mindestens 32)", v)
	}
	return ""
}
