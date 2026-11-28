package daemon

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/DavidW475/ade/internal/config"
)

// ErrAlreadyRunning means that a daemon already uses the same data directory.
var ErrAlreadyRunning = errors.New("ADE daemon is already running")

// lockFile holds an exclusive flock lock on daemon.lock as long as the daemon runs.
// The kernel releases the lock when the process ends, even after a crash; a leftover file
// therefore blocks nothing.
type lockFile struct {
	f *os.File
}

func acquireLock(path string) (*lockFile, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G703: lock file in the configured data directory
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if pid := readPID(path); pid > 0 {
				return nil, fmt.Errorf("%w (PID %d, Lock %s)", ErrAlreadyRunning, pid, path)
			}
			return nil, fmt.Errorf("%w (Lock %s)", ErrAlreadyRunning, path)
		}
		return nil, fmt.Errorf("lock lock file: %w", err)
	}
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &lockFile{f: f}, nil
}

// release releases the lock and clears the PID.
func (l *lockFile) release() {
	_ = l.f.Truncate(0)
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}

func readPID(path string) int {
	data, err := os.ReadFile(path) //nolint:gosec // G703: lock file in the configured data directory
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

// RunningPID returns the PID from the lock file (0 if none is recorded).
func RunningPID(cfg *config.Config) int { return readPID(cfg.LockPath()) }
