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
	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer waitCancel()
	h, err := c.WaitHealthy(waitCtx)
	if err != nil {
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
		t.Errorf("Secret-Rechte = %v", st.Mode().Perm())
	}

	login, err := c.LoginURL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(login, "http://localhost:"+cfg.Port()+"/auth/login?ticket=") {
		t.Fatalf("LoginURL = %q", login)
	}

	// Ticket über die echte Verbindung einlösen.
	req, _ := http.NewRequest("GET", strings.Replace(login, "localhost", "127.0.0.1", 1), nil)
	req.Host = "localhost:" + cfg.Port()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("Login: HTTP %d", resp.StatusCode)
	}

	if pid := readPID(cfg.LockPath()); pid != os.Getpid() {
		t.Errorf("PID in Lock-Datei = %d, want %d", pid, os.Getpid())
	}

	// Ein zweiter Daemon auf demselben Datenverzeichnis – auch auf anderem Port – startet
	// nicht und überschreibt das Secret nicht.
	before, _ := os.ReadFile(cfg.SecretPath())
	other := *cfg
	other.Listen = testConfig(t).Listen
	err = Run(ctx, &other, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("zweiter Daemon: err = %v, want ErrAlreadyRunning", err)
	}
	after, _ := os.ReadFile(cfg.SecretPath())
	if string(before) != string(after) {
		t.Fatal("zweiter Daemon hat das Secret überschrieben")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Daemon beendet sich nicht")
	}
	if _, err := os.Stat(cfg.SecretPath()); !os.IsNotExist(err) {
		t.Errorf("Secret nach Shutdown noch vorhanden: %v", err)
	}

	// Nach dem Beenden ist die Sperre frei.
	l, err := acquireLock(cfg.LockPath())
	if err != nil {
		t.Fatalf("Lock nach Shutdown: %v", err)
	}
	l.release()
}

func TestHealthNotRunning(t *testing.T) {
	cfg := testConfig(t)
	if _, err := NewClient(cfg).Health(context.Background()); err == nil {
		t.Fatal("Fehler erwartet")
	}
}

func TestPrivateDirTightensPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // umask umgehen
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
		t.Fatalf("Rechte %o, want 700", m)
	}
}
