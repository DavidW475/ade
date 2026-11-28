package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/DavidW475/ade/internal/config"
	"github.com/DavidW475/ade/internal/daemon"
)

const stopUsage = `Usage:
  ade stop [--config <path>]

Stops the daemon of the configuration: the systemd service if it runs as one, otherwise the
background process (SIGTERM to the PID in daemon.lock). Waits until the daemon has ended.
Running agent sessions become interrupted and can be resumed after "ade start".
`

const restartUsage = `Usage:
  ade restart [--config <path>]

Stops the daemon (like "ade stop") and starts it again, e.g. after changing config.yaml.
Does not open the browser.
`

// stopTimeout covers the daemon's shutdown (HTTP server, agents, IDE, containers).
const stopTimeout = 60 * time.Second

func cmdStop(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, stopUsage) }
	cfg, cfgPath, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	how, err := stopDaemon(ctx, cfg, cfgPath)
	if err != nil {
		return err
	}
	switch how {
	case "systemd":
		_, _ = fmt.Fprintln(stdout, "ADE service stopped")
	case "process":
		_, _ = fmt.Fprintln(stdout, "ADE daemon stopped")
	default:
		_, _ = fmt.Fprintln(stdout, "ADE daemon is not running")
	}
	return nil
}

func cmdRestart(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, restartUsage) }
	cfg, cfgPath, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout+30*time.Second)
	defer cancel()
	if _, err := stopDaemon(ctx, cfg, cfgPath); err != nil {
		return err
	}
	h, how, err := ensureRunning(ctx, cfg, cfgPath)
	if err != nil {
		return err
	}
	what := "daemon"
	if how == "systemd" {
		what = "service"
	}
	_, _ = fmt.Fprintf(stdout, "ADE %s restarted (%s, log: %s)\n", what, h.Version, cfg.LogPath())
	return nil
}

// stopDaemon stops the daemon of the configuration and waits until its lock is free. how is
// "systemd" or "process", or "" if no daemon was running.
func stopDaemon(ctx context.Context, cfg *config.Config, cfgPath string) (how string, err error) {
	if m, err := newServiceManager(cfg, cfgPath); err == nil && m.InstalledConfig() == cfgPath && m.Active(ctx) {
		if err := m.Stop(ctx); err != nil {
			return "", err
		}
		if err := daemon.WaitStopped(ctx, cfg); err != nil {
			return "", fmt.Errorf("service stopped, but the daemon still holds %s: %w", cfg.LockPath(), err)
		}
		return "systemd", nil
	}
	stopped, err := daemon.Stop(ctx, cfg)
	if err != nil || !stopped {
		return "", err
	}
	return "process", nil
}
