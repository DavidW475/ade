// Package daemon startet den ADE-Daemon und bietet den Client, mit dem die CLI ihn anspricht.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/DavidW475/ade/internal/api"
	"github.com/DavidW475/ade/internal/config"
	"github.com/DavidW475/ade/internal/store"
)

// Run startet den Daemon und blockiert, bis ctx beendet wird.
func Run(ctx context.Context, cfg *config.Config, version string, log *slog.Logger) error {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("Datenverzeichnis anlegen: %w", err)
	}

	// Ein Daemon pro Datenverzeichnis – auch wenn ein zweiter auf einem anderen Port liefe.
	lock, err := acquireLock(cfg.LockPath())
	if err != nil {
		return err
	}
	defer lock.release()

	st, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("auf %s lauschen: %w", cfg.Listen, err)
	}

	secret := api.RandomToken()
	if err := writeSecret(cfg.SecretPath(), secret); err != nil {
		_ = ln.Close()
		return err
	}
	defer func() { _ = os.Remove(cfg.SecretPath()) }()

	srv := &http.Server{
		Handler: api.New(api.Options{
			Version: version,
			Port:    cfg.Port(),
			Store:   st,
			Auth:    api.NewAuth(secret),
			Logger:  log,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("ADE-Daemon läuft", "listen", cfg.Listen, "url", cfg.BaseURL(), "version", version)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("ADE-Daemon wird beendet")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// writeSecret schreibt das Secret atomar und nur für den Nutzer lesbar.
func writeSecret(path, secret string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".daemon.secret-*")
	if err != nil {
		return fmt.Errorf("Daemon-Secret schreiben: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(secret + "\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
