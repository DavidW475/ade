package daemon

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/config"
)

// TestLockHolderHelper is not a test: started by TestStop as a subprocess, it plays the daemon
// (holds the lock until SIGTERM).
func TestLockHolderHelper(t *testing.T) {
	path := os.Getenv("ADE_TEST_LOCK_HOLDER")
	if path == "" {
		t.Skip("helper process")
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	l, err := acquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = os.Stdout.WriteString("locked\n")
	<-sig
	l.release()
	_, _ = os.Stdout.WriteString("stopped\n")
}

func TestStop(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// No daemon: nothing to stop.
	if stopped, err := Stop(ctx, cfg); err != nil || stopped {
		t.Fatalf("Stop without daemon = %v, %v", stopped, err)
	}

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLockHolderHelper$", "-test.v") //nolint:gosec // G204: own test binary
	cmd.Env = append(os.Environ(), "ADE_TEST_LOCK_HOLDER="+filepath.Join(dir, "daemon.lock"))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	lines := bufio.NewScanner(out)
	for lines.Scan() {
		if lines.Text() == "locked" {
			break
		}
	}
	if !Running(cfg) {
		t.Fatal("Running = false while the helper holds the lock")
	}
	if pid := RunningPID(cfg); pid != cmd.Process.Pid {
		t.Fatalf("PID = %d, want %d", pid, cmd.Process.Pid)
	}

	stopped, err := Stop(ctx, cfg)
	if err != nil || !stopped {
		t.Fatalf("Stop = %v, %v", stopped, err)
	}
	if Running(cfg) {
		t.Fatal("lock still held after Stop")
	}
	// The helper ended through its SIGTERM handler, not killed.
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper: %v", err)
	}
	if lock, err := acquireLock(cfg.LockPath()); err != nil {
		t.Fatalf("lock after Stop: %v", err)
	} else {
		lock.release()
	}
}
