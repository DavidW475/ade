package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/config"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	dir := t.TempDir()
	return &config.Config{
		Listen:        addr,
		DataDir:       filepath.Join(dir, "data"),
		WorkspacesDir: filepath.Join(dir, "ws"),
	}
}

func TestRunServesAndCleansUp(t *testing.T) {
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, "test", slog.New(slog.NewTextHandler(io.Discard, nil))) }()

	c := NewClient(cfg)
	// Generous: under -race and with parallel packages the start sometimes takes over 5 s in CI.
	waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
	defer waitCancel()
	h, err := c.WaitHealthy(waitCtx)
	if err != nil {
		// If Run ended early, report its error instead of only the timeout.
		select {
		case runErr := <-done:
			t.Fatalf("daemon ended: %v (%v)", runErr, err)
		default:
		}
		t.Fatal(err)
	}
	if h.Version != "test" || h.Schema < 1 {
		t.Fatalf("Health = %#v", h)
	}

	st, err := os.Stat(cfg.SecretPath())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("secret permissions = %v", st.Mode().Perm())
	}

	login, err := c.LoginURL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(login, "http://localhost:"+cfg.Port()+"/auth/login?ticket=") {
		t.Fatalf("LoginURL = %q", login)
	}

	// Redeem the ticket over the real connection.
	req, _ := http.NewRequest("GET", strings.Replace(login, "localhost", "127.0.0.1", 1), nil)
	req.Host = "localhost:" + cfg.Port()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: HTTP %d", resp.StatusCode)
	}

	if pid := readPID(cfg.LockPath()); pid != os.Getpid() {
		t.Errorf("PID in lock file = %d, want %d", pid, os.Getpid())
	}

	// A second daemon on the same data directory – even on another port – does not start
	// and does not overwrite the secret.
	before, _ := os.ReadFile(cfg.SecretPath())
	other := *cfg
	other.Listen = testConfig(t).Listen
	err = Run(ctx, &other, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second daemon: err = %v, want ErrAlreadyRunning", err)
	}
	after, _ := os.ReadFile(cfg.SecretPath())
	if string(before) != string(after) {
		t.Fatal("second daemon overwrote the secret")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon does not stop")
	}
	if _, err := os.Stat(cfg.SecretPath()); !os.IsNotExist(err) {
		t.Errorf("secret still exists after shutdown: %v", err)
	}

	// After stopping, the lock is free.
	l, err := acquireLock(cfg.LockPath())
	if err != nil {
		t.Fatalf("lock after shutdown: %v", err)
	}
	l.release()
}

func TestHealthNotRunning(t *testing.T) {
	cfg := testConfig(t)
	if _, err := NewClient(cfg).Health(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
}

func TestPrivateDirTightensPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // bypass the umask
		t.Fatal(err)
	}
	if err := privateDir(dir, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m := fi.Mode().Perm(); m != 0o700 {
		t.Fatalf("permissions %o, want 700", m)
	}
}
