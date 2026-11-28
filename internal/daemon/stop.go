package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/DavidW475/ade/internal/config"
)

// stopPoll is the interval in which Stop and WaitStopped check the lock.
const stopPoll = 100 * time.Millisecond

// Running reports whether a daemon holds the lock of the configuration's data directory.
func Running(cfg *config.Config) bool { return lockHeld(cfg.LockPath()) }

// Stop ends the daemon that holds the lock of the configuration: SIGTERM to the PID from the
// lock file, then it waits until the lock is free. The daemon shuts down as with Ctrl+C:
// running sessions become interrupted and can be resumed after the next start.
// stopped is false if no daemon was running.
func Stop(ctx context.Context, cfg *config.Config) (stopped bool, err error) {
	path := cfg.LockPath()
	pid := 0
	for {
		if !lockHeld(path) {
			return false, nil
		}
		// Right after locking the daemon has not written its PID yet.
		if pid = readPID(path); pid > 0 {
			break
		}
		if err := sleep(ctx, stopPoll); err != nil {
			return false, fmt.Errorf("daemon holds %s without a PID: %w", path, err)
		}
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return false, fmt.Errorf("stop daemon (PID %d): %w", pid, err)
	}
	if err := WaitStopped(ctx, cfg); err != nil {
		return false, fmt.Errorf("daemon (PID %d) does not stop: %w", pid, err)
	}
	return true, nil
}

// WaitStopped waits until no daemon holds the lock of the configuration any more.
func WaitStopped(ctx context.Context, cfg *config.Config) error {
	for lockHeld(cfg.LockPath()) {
		if err := sleep(ctx, stopPoll); err != nil {
			return err
		}
	}
	return nil
}

// lockHeld reports whether another open file holds the flock lock on path. It only takes a
// shared lock for a moment; a daemon starting at exactly that time reports ErrAlreadyRunning.
func lockHeld(path string) bool {
	f, err := os.Open(path) //nolint:gosec // G304: lock file from the own configuration
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
