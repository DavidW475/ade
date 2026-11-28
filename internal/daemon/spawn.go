package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/DavidW475/ade/internal/api"
	"github.com/DavidW475/ade/internal/config"
)

// EnsureRunning checks whether a daemon is running and otherwise starts `<exe> serve` in the background.
// started is true if a new daemon was started.
func EnsureRunning(ctx context.Context, cfg *config.Config, configPath string) (h *api.HealthResponse, started bool, err error) {
	c := NewClient(cfg)
	if h, err := c.Health(ctx); err == nil {
		return h, false, nil
	} else if !errors.Is(err, ErrNotRunning) {
		return nil, false, err
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, false, err
	}
	logf, err := os.OpenFile(cfg.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open daemon log: %w", err)
	}
	defer func() { _ = logf.Close() }()

	cmd := exec.Command(exe, "serve", "--config", configPath)
	cmd.Stdout, cmd.Stderr = logf, logf
	// Own session: the daemon survives closing the terminal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, false, fmt.Errorf("start daemon: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	healthy := make(chan struct{})
	var health *api.HealthResponse
	var healthErr error
	go func() {
		health, healthErr = c.WaitHealthy(waitCtx)
		close(healthy)
	}()

	select {
	case <-healthy:
		if healthErr != nil {
			return nil, true, fmt.Errorf("daemon does not answer (log: %s): %w", cfg.LogPath(), healthErr)
		}
		return health, true, nil
	case err := <-exited:
		cancel()
		<-healthy
		return nil, true, fmt.Errorf("daemon exited immediately (%w):\n%s", err, tail(cfg.LogPath(), 10))
	}
}

func tail(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "(no log)"
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
