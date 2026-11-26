package daemon

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// ErrAlreadyRunning bedeutet, dass bereits ein Daemon dasselbe Datenverzeichnis benutzt.
var ErrAlreadyRunning = errors.New("ADE-Daemon läuft bereits")

// lockFile hält eine exklusive flock-Sperre auf daemon.lock, solange der Daemon läuft.
// Der Kernel gibt die Sperre beim Prozessende frei, auch nach einem Absturz; eine
// liegengebliebene Datei blockiert daher nichts.
type lockFile struct {
	f *os.File
}

func acquireLock(path string) (*lockFile, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("Lock-Datei öffnen: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if pid := readPID(path); pid > 0 {
				return nil, fmt.Errorf("%w (PID %d, Lock %s)", ErrAlreadyRunning, pid, path)
			}
			return nil, fmt.Errorf("%w (Lock %s)", ErrAlreadyRunning, path)
		}
		return nil, fmt.Errorf("Lock-Datei sperren: %w", err)
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

// release gibt die Sperre frei und leert die PID.
func (l *lockFile) release() {
	_ = l.f.Truncate(0)
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}

func readPID(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}
