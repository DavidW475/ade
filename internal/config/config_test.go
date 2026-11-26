package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	return home
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	home := setHome(t)

	cfg, err := Load(filepath.Join(home, "gibt-es-nicht.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != DefaultListen {
		t.Errorf("Listen = %q, want %q", cfg.Listen, DefaultListen)
	}
	if want := filepath.Join(home, ".local", "share", "ade"); cfg.DataDir != want {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, want)
	}
	if want := filepath.Join(home, ".ade", "workspaces"); cfg.WorkspacesDir != want {
		t.Errorf("WorkspacesDir = %q, want %q", cfg.WorkspacesDir, want)
	}
	if cfg.IDE.IdleTimeout != DefaultIDEIdleTimeout || cfg.IDE.Binary != "" {
		t.Errorf("IDE = %#v", cfg.IDE)
	}
	if cfg.BaseURL() != "http://localhost:7000" {
		t.Errorf("BaseURL = %q", cfg.BaseURL())
	}
}

func TestLoadOverridesDefaults(t *testing.T) {
	home := setHome(t)
	path := filepath.Join(home, "config.yaml")
	writeFile(t, path, `
listen: 127.0.0.1:7100
data_dir: ~/ade-data
providers:
  anthropic:
    api_key: env:ANTHROPIC_API_KEY
defaults:
  adapter: pty
  sandbox: false
ide:
  binary: ~/bin/openvscode-server
  idle_timeout: 1h
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:7100" || cfg.Port() != "7100" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if want := filepath.Join(home, "ade-data"); cfg.DataDir != want {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, want)
	}
	if want := filepath.Join(home, ".ade", "workspaces"); cfg.WorkspacesDir != want {
		t.Errorf("WorkspacesDir = %q, want Default %q", cfg.WorkspacesDir, want)
	}
	if cfg.Providers["anthropic"].APIKey != "env:ANTHROPIC_API_KEY" {
		t.Errorf("Providers = %#v", cfg.Providers)
	}
	if cfg.Defaults.Adapter != "pty" || cfg.Defaults.Sandbox {
		t.Errorf("Defaults = %#v", cfg.Defaults)
	}
	if cfg.IDE.Binary != filepath.Join(home, "bin", "openvscode-server") || cfg.IDE.IdleTimeout != time.Hour {
		t.Errorf("IDE = %#v", cfg.IDE)
	}
}

func TestLoadEmptyFile(t *testing.T) {
	home := setHome(t)
	path := filepath.Join(home, "config.yaml")
	writeFile(t, path, "")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != DefaultListen {
		t.Errorf("Listen = %q", cfg.Listen)
	}
}

func TestLoadRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"nicht-loopback":     "listen: 0.0.0.0:7000\n",
		"fremde-ip":          "listen: 192.168.1.5:7000\n",
		"ohne-port":          "listen: 127.0.0.1\n",
		"port-null":          "listen: 127.0.0.1:0\n",
		"unbekannter-schl":   "lissten: 127.0.0.1:7000\n",
		"relativer-data-dir": "data_dir: daten\n",
		"relatives-ide":      "ide:\n  binary: bin/ovs\n",
		"ide-idle-zu-kurz":   "ide:\n  idle_timeout: 10s\n",
		"ide-idle-kaputt":    "ide:\n  idle_timeout: bald\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			home := setHome(t)
			path := filepath.Join(home, "config.yaml")
			writeFile(t, path, content)
			if _, err := Load(path); err == nil {
				t.Fatal("Fehler erwartet")
			}
		})
	}
}

func TestLoadAcceptsLoopbackVariants(t *testing.T) {
	for _, listen := range []string{"localhost:7000", "[::1]:7000", "127.0.0.2:7000"} {
		home := setHome(t)
		path := filepath.Join(home, "config.yaml")
		writeFile(t, path, "listen: \""+listen+"\"\n")
		if _, err := Load(path); err != nil {
			t.Errorf("%s: %v", listen, err)
		}
	}
}

func TestDefaultPathEnv(t *testing.T) {
	t.Setenv("ADE_CONFIG", "/tmp/x.yaml")
	p, err := DefaultPath()
	if err != nil || p != "/tmp/x.yaml" {
		t.Fatalf("DefaultPath = %q, %v", p, err)
	}
	t.Setenv("ADE_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	p, err = DefaultPath()
	if err != nil || !strings.HasSuffix(p, "/cfg/ade/config.yaml") {
		t.Fatalf("DefaultPath = %q, %v", p, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolve(t *testing.T) {
	t.Setenv("ADE_TEST_KEY", "sk-123")
	t.Setenv("ADE_TEST_EMPTY", "")

	if v, err := Resolve("env:ADE_TEST_KEY"); err != nil || v != "sk-123" {
		t.Errorf("env: = %q, %v", v, err)
	}
	if v, err := Resolve("klartext"); err != nil || v != "klartext" {
		t.Errorf("Klartext = %q, %v", v, err)
	}
	for _, ref := range []string{"env:ADE_TEST_EMPTY", "env:ADE_TEST_GIBTS_NICHT", "env:"} {
		if _, err := Resolve(ref); err == nil {
			t.Errorf("%s: Fehler erwartet", ref)
		}
	}
	p := Provider{APIKey: "env:ADE_TEST_KEY"}
	if v, err := p.ResolveAPIKey(); err != nil || v != "sk-123" {
		t.Errorf("ResolveAPIKey = %q, %v", v, err)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	setHome(t)
	if _, err := Load(filepath.Join("..", "..", "examples", "config.yaml")); err != nil {
		t.Fatalf("examples/config.yaml: %v", err)
	}
}
