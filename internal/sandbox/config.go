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

// ConfigFile is the path of the sandbox configuration relative to the project repository.
const ConfigFile = ".ade/sandbox.yaml"

// Defaults for a container's resources.
const (
	DefaultMemory = "4g"
	DefaultCPUs   = 2.0
	DefaultPIDs   = 512
)

// ImageRepo is the base image from images/agent-base.
const ImageRepo = "ghcr.io/davidw475/ade-agent-base"

// DefaultImage returns the base image matching the ADE version: releases (v1.2.3) and
// pre-releases (v1.2.3-rc.1) use the image of the same version without "v" that the image
// workflow builds for the tag; development builds and snapshots :latest.
func DefaultImage(version string) string {
	if m := releaseRE.FindStringSubmatch(version); m != nil {
		return ImageRepo + ":" + m[1]
	}
	return ImageRepo + ":latest"
}

var releaseRE = regexp.MustCompile(`^v(\d+\.\d+\.\d+(?:-(?:alpha|beta|rc)\.\d+)?)$`)

// Resources limits a container.
type Resources struct {
	// Memory in the format of docker --memory (e.g. 4g, 512m).
	Memory string  `yaml:"memory" json:"memory"`
	CPUs   float64 `yaml:"cpus" json:"cpus"`
	PIDs   int     `yaml:"pids" json:"pids"`
}

// Config is the contents of .ade/sandbox.yaml. All fields are optional.
type Config struct {
	// Image overrides the default image (e.g. one derived from the base image).
	Image     string    `yaml:"image" json:"image,omitempty"`
	Resources Resources `yaml:"resources" json:"resources"`
	// Egress are the network rules (nil in the file: defaults, see DefaultPresets).
	Egress *Egress `yaml:"egress" json:"egress"`
}

// ConfigError is an invalid sandbox.yaml.
type ConfigError struct{ Msg string }

func (e *ConfigError) Error() string { return ConfigFile + ": " + e.Msg }

// LoadConfig reads .ade/sandbox.yaml from repo; if the file is missing, the defaults apply.
// Invalid files return the defaults and a *ConfigError.
func LoadConfig(repo string) (*Config, error) {
	data, err := os.ReadFile(filepath.Join(repo, ConfigFile)) //nolint:gosec // G304: path in the registered repository
	if errors.Is(err, os.ErrNotExist) {
		return withDefaults(&Config{}), nil
	}
	if err != nil {
		return withDefaults(&Config{}), &ConfigError{Msg: err.Error()}
	}
	return ParseConfig(data)
}

// ParseConfig checks and reads the contents of a sandbox.yaml.
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
		return fmt.Sprintf("image: %q is not a valid image name", c.Image)
	}
	if m := c.Resources.Memory; m != "" && !memoryRE.MatchString(m) {
		return fmt.Sprintf("resources.memory: %q is invalid (e.g. 4g or 512m)", m)
	}
	if v := c.Resources.CPUs; v < 0 || v > 256 {
		return fmt.Sprintf("resources.cpus: %v is invalid (0 < cpus ≤ 256)", v)
	}
	if v := c.Resources.PIDs; v < 0 || (v > 0 && v < 32) || v > 1<<20 {
		return fmt.Sprintf("resources.pids: %d is invalid (at least 32)", v)
	}
	return ""
}
