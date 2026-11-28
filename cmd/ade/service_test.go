package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DavidW475/ade/internal/config"
	"github.com/DavidW475/ade/internal/service"
)

func fakeService(t *testing.T) (unitDir string) {
	t.Helper()
	unitDir = t.TempDir()
	old := newServiceManager
	t.Cleanup(func() { newServiceManager = old })
	newServiceManager = func(cfg *config.Config, cfgPath string) (*service.Manager, error) {
		return &service.Manager{
			UnitDir: unitDir, Exe: "/bin/ade", ConfigPath: cfgPath, LogPath: cfg.LogPath(), Path: "/usr/bin",
			Run: func(context.Context, ...string) ([]byte, error) { return nil, nil },
		}, nil
	}
	return unitDir
}

func TestInstallServiceDryRun(t *testing.T) {
	unitDir := fakeService(t)
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("listen: 127.0.0.1:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := run([]string{"install-service", "--dry-run", "--config", cfg}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `serve --config "`+cfg+`"`) {
		t.Errorf("output = %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(unitDir, service.Name)); err == nil {
		t.Error("--dry-run wrote the unit")
	}
}

func TestUninstallServiceNotInstalled(t *testing.T) {
	fakeService(t)
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("listen: 127.0.0.1:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err := run([]string{"uninstall-service", "--config", cfg}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "is not installed") {
		t.Fatalf("err = %v", err)
	}
}
