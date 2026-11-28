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

func stopTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("listen: 127.0.0.1:1\ndata_dir: "+filepath.Join(dir, "data")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestStopNotRunning(t *testing.T) {
	fakeService(t)
	var out, errOut bytes.Buffer
	if err := run([]string{"stop", "--config", stopTestConfig(t)}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not running") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestStopService(t *testing.T) {
	cfgPath := stopTestConfig(t)
	unitDir := t.TempDir()
	var calls []string
	old := newServiceManager
	t.Cleanup(func() { newServiceManager = old })
	newServiceManager = func(cfg *config.Config, p string) (*service.Manager, error) {
		return &service.Manager{
			UnitDir: unitDir, Exe: "/bin/ade", ConfigPath: p, LogPath: cfg.LogPath(), Path: "/usr/bin",
			Run: func(_ context.Context, args ...string) ([]byte, error) {
				calls = append(calls, strings.Join(args, " "))
				if args[0] == "is-active" {
					return []byte("active\n"), nil
				}
				return nil, nil
			},
		}, nil
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := newServiceManager(cfg, cfgPath)
	if err := os.WriteFile(m.UnitPath(), []byte(m.Unit()), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if err := run([]string{"stop", "--config", cfgPath}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "service stopped") {
		t.Fatalf("output = %q", out.String())
	}
	if got := strings.Join(calls, "; "); !strings.Contains(got, "stop "+service.Name) {
		t.Fatalf("systemctl calls = %q", got)
	}
}

func TestStopRejectsArguments(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"stop", "--config", stopTestConfig(t), "now"}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("err = %v", err)
	}
}
