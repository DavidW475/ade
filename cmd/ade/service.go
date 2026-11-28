package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/DavidW475/ade/internal/api"
	"github.com/DavidW475/ade/internal/config"
	"github.com/DavidW475/ade/internal/daemon"
	"github.com/DavidW475/ade/internal/service"
)

const installServiceUsage = `Usage:
  ade install-service [--dry-run] [--config <path>]

Sets up the daemon as a systemd user service (~/.config/systemd/user/ade.service) and starts
it. Under WSL systemd must be active (systemd=true in /etc/wsl.conf). The service takes over
the current PATH; run it again after a new binary or a changed PATH.
`

const uninstallServiceUsage = `Usage:
  ade uninstall-service [--config <path>]

Stops the systemd user service and removes the unit.
`

// newServiceManager is replaceable for tests.
var newServiceManager = func(cfg *config.Config, cfgPath string) (*service.Manager, error) {
	return service.New(cfgPath, cfg.LogPath())
}

func cmdInstallService(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("install-service", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, installServiceUsage) }
	dry := fs.Bool("dry-run", false, "only print the unit, change nothing")
	cfg, cfgPath, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	m, err := newServiceManager(cfg, cfgPath)
	if err != nil {
		return err
	}
	if *dry {
		_, _ = fmt.Fprintf(stdout, "# %s\n%s", m.UnitPath(), m.Unit())
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.Available(ctx); err != nil {
		return err
	}
	// A daemon outside systemd holds the lock; the service would only restart and fail.
	c := daemon.NewClient(cfg)
	if _, err := c.Health(ctx); err == nil && !m.Active(ctx) {
		pid := daemon.RunningPID(cfg)
		hint := "stop it first"
		if pid > 0 {
			hint = fmt.Sprintf("stop it first (kill %d)", pid)
		}
		return fmt.Errorf("an ADE daemon is already running outside systemd on %s; %s", cfg.Listen, hint)
	} else if err != nil && !errors.Is(err, daemon.ErrNotRunning) {
		return err
	}

	// Without the data directory systemd cannot open the log (StandardOutput=append:).
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	if err := m.Install(ctx); err != nil {
		return err
	}
	h, err := c.WaitHealthy(ctx)
	if err != nil {
		return fmt.Errorf("service installed, but the daemon does not answer (log: %s, `systemctl --user status ade`): %w", cfg.LogPath(), err)
	}
	_, _ = fmt.Fprintf(stdout, "service %s installed and started (%s).\n", service.Name, h.Version)
	_, _ = fmt.Fprintf(stdout, "  Unit: %s\n  Log:  %s\n", m.UnitPath(), cfg.LogPath())
	if on, ok := service.Linger(ctx); ok && !on {
		_, _ = fmt.Fprintln(stdout, "Note: the service starts with your first login (WSL shell). To keep it running without an")
		_, _ = fmt.Fprintln(stdout, "open session: sudo loginctl enable-linger \"$USER\"")
	}
	return nil
}

func cmdUninstallService(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("uninstall-service", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, uninstallServiceUsage) }
	cfg, cfgPath, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	m, err := newServiceManager(cfg, cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.Uninstall(ctx); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "service %s stopped and removed.\n", service.Name)
	return nil
}

// ensureRunning starts the daemon through systemd if the unit is installed for this
// configuration, otherwise itself in the background.
func ensureRunning(ctx context.Context, cfg *config.Config, cfgPath string) (h *api.HealthResponse, how string, err error) {
	c := daemon.NewClient(cfg)
	if h, err := c.Health(ctx); err == nil {
		return h, "", nil
	} else if !errors.Is(err, daemon.ErrNotRunning) {
		return nil, "", err
	}
	if m, err := newServiceManager(cfg, cfgPath); err == nil && m.InstalledConfig() == cfgPath {
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil { //nolint:gosec // G703: data directory from the own configuration
			return nil, "", err
		}
		if err := m.Start(ctx); err != nil {
			return nil, "", err
		}
		h, err := c.WaitHealthy(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("service started, but the daemon does not answer (log: %s, `systemctl --user status ade`): %w", cfg.LogPath(), err)
		}
		return h, "systemd", nil
	}
	h, _, err = daemon.EnsureRunning(ctx, cfg, cfgPath)
	if err != nil {
		return nil, "", err
	}
	return h, "process", nil
}
