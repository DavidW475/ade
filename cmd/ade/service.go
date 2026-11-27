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

const installServiceUsage = `Verwendung:
  ade install-service [--dry-run] [--config <pfad>]

Richtet den Daemon als systemd-User-Dienst ein (~/.config/systemd/user/ade.service) und
startet ihn. Unter WSL muss systemd aktiv sein (systemd=true in /etc/wsl.conf). Der Dienst
übernimmt den aktuellen PATH; nach einem neuen Binary oder geändertem PATH erneut ausführen.
`

const uninstallServiceUsage = `Verwendung:
  ade uninstall-service [--config <pfad>]

Stoppt den systemd-User-Dienst und entfernt die Unit.
`

// newServiceManager ist austauschbar für Tests.
var newServiceManager = func(cfg *config.Config, cfgPath string) (*service.Manager, error) {
	return service.New(cfgPath, cfg.LogPath())
}

func cmdInstallService(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("install-service", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, installServiceUsage) }
	dry := fs.Bool("dry-run", false, "Unit nur ausgeben, nichts ändern")
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
	// Ein Daemon außerhalb von systemd hält den Lock; der Dienst würde nur neu starten und scheitern.
	c := daemon.NewClient(cfg)
	if _, err := c.Health(ctx); err == nil && !m.Active(ctx) {
		pid := daemon.RunningPID(cfg)
		hint := "zuerst beenden"
		if pid > 0 {
			hint = fmt.Sprintf("zuerst beenden (kill %d)", pid)
		}
		return fmt.Errorf("ein ADE-Daemon läuft bereits außerhalb von systemd auf %s; %s", cfg.Listen, hint)
	} else if err != nil && !errors.Is(err, daemon.ErrNotRunning) {
		return err
	}

	// Ohne Datenverzeichnis kann systemd das Log (StandardOutput=append:) nicht öffnen.
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	if err := m.Install(ctx); err != nil {
		return err
	}
	h, err := c.WaitHealthy(ctx)
	if err != nil {
		return fmt.Errorf("Dienst installiert, aber der Daemon antwortet nicht (Log: %s, `systemctl --user status ade`): %w", cfg.LogPath(), err)
	}
	_, _ = fmt.Fprintf(stdout, "Dienst %s installiert und gestartet (%s).\n", service.Name, h.Version)
	_, _ = fmt.Fprintf(stdout, "  Unit: %s\n  Log:  %s\n", m.UnitPath(), cfg.LogPath())
	if on, ok := service.Linger(ctx); ok && !on {
		_, _ = fmt.Fprintln(stdout, "Hinweis: Der Dienst startet mit deiner ersten Anmeldung (WSL-Shell). Damit er auch ohne")
		_, _ = fmt.Fprintln(stdout, "offene Sitzung läuft: sudo loginctl enable-linger \"$USER\"")
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
	_, _ = fmt.Fprintf(stdout, "Dienst %s gestoppt und entfernt.\n", service.Name)
	return nil
}

// ensureRunning startet den Daemon über systemd, wenn die Unit für diese Konfiguration
// installiert ist, sonst selbst im Hintergrund.
func ensureRunning(ctx context.Context, cfg *config.Config, cfgPath string) (h *api.HealthResponse, how string, err error) {
	c := daemon.NewClient(cfg)
	if h, err := c.Health(ctx); err == nil {
		return h, "", nil
	} else if !errors.Is(err, daemon.ErrNotRunning) {
		return nil, "", err
	}
	if m, err := newServiceManager(cfg, cfgPath); err == nil && m.InstalledConfig() == cfgPath {
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil { //nolint:gosec // G703: Datenverzeichnis aus der eigenen Konfiguration
			return nil, "", err
		}
		if err := m.Start(ctx); err != nil {
			return nil, "", err
		}
		h, err := c.WaitHealthy(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("Dienst gestartet, aber der Daemon antwortet nicht (Log: %s, `systemctl --user status ade`): %w", cfg.LogPath(), err)
		}
		return h, "systemd", nil
	}
	h, _, err = daemon.EnsureRunning(ctx, cfg, cfgPath)
	if err != nil {
		return nil, "", err
	}
	return h, "process", nil
}
