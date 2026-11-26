package daemon

import (
	"context"
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

	// Ein zweiter Daemon auf demselben Port darf das Secret nicht überschreiben.
	before, _ := os.ReadFile(cfg.SecretPath())
	if err := Run(ctx, cfg, "test", slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("zweiter Daemon auf belegtem Port gestartet")
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
}

func TestHealthNotRunning(t *testing.T) {
	cfg := testConfig(t)
	if _, err := NewClient(cfg).Health(context.Background()); err == nil {
		t.Fatal("Fehler erwartet")
	}
}
