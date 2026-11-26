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

	"github.com/DavidW475/ade/internal/agent"
	"github.com/DavidW475/ade/internal/agent/adapter"
	"github.com/DavidW475/ade/internal/agent/adapter/claude"
	"github.com/DavidW475/ade/internal/agent/adapter/pty"
	"github.com/DavidW475/ade/internal/api"
	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/config"
	"github.com/DavidW475/ade/internal/ide"
	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/workspace"
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

	projects := project.NewService(st)
	workspaces := workspace.NewManager(st, projects, cfg.WorkspacesDir, log)
	if err := workspaces.Recover(ctx); err != nil {
		return err
	}
	defer workspaces.Close()

	agents := agent.NewManager(agent.Options{
		Store:          st,
		Projects:       projects,
		Workspaces:     workspaces,
		Adapters:       adapter.NewRegistry(claude.New(), pty.New()),
		DataDir:        cfg.DataDir,
		Logger:         log,
		DefaultAdapter: cfg.Defaults.Adapter,
		AdapterEnv:     adapterEnv(cfg, log),
	})
	if err := agents.Recover(ctx); err != nil {
		return err
	}
	// Läuft vor workspaces.Close (defer-Reihenfolge): erst Agenten beenden, dann Setups.
	defer agents.Close()

	ides := ide.NewManager(ide.Options{
		Dir:         cfg.IDEDir(),
		Binary:      cfg.IDE.Binary,
		IdleTimeout: cfg.IDE.IdleTimeout,
		Logger:      log,
		Workspaces: func(ctx context.Context, id string) (string, error) {
			w, err := workspaces.Get(ctx, id)
			if err != nil {
				return "", err
			}
			if w.Status == store.WorkspaceCreating || w.Status == store.WorkspaceError {
				return "", apperr.Conflict("Workspace %s hat kein Arbeitsverzeichnis (Status %s)", id, w.Status)
			}
			return w.Path, nil
		},
	})
	ides.Recover()
	defer ides.Close()

	workspaces.SetBusyCheck(agents.WorkspaceBusy)
	workspaces.SetBeforeDelete(func(ctx context.Context, id string) error {
		if err := agents.BeforeWorkspaceDelete(ctx, id); err != nil {
			return err
		}
		return ides.BeforeWorkspaceDelete(ctx, id)
	})

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
			Version:    version,
			Port:       cfg.Port(),
			Store:      st,
			Auth:       api.NewAuth(secret),
			Logger:     log,
			Projects:   projects,
			Workspaces: workspaces,
			Agents:     agents,
			IDE:        ides,
			// Wie der Manager: ohne Konfiguration gilt claude.
			DefaultAdapter: defaultAdapter(cfg),
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

func defaultAdapter(cfg *config.Config) string {
	if cfg.Defaults.Adapter != "" {
		return cfg.Defaults.Adapter
	}
	return "claude"
}

// adapterEnv liefert API-Keys aus der Konfiguration an die passenden Adapter. Ohne Eintrag
// nutzt die CLI ihre eigene Anmeldung (ADR-010).
func adapterEnv(cfg *config.Config, log *slog.Logger) func(string) []string {
	keys := map[string]struct{ provider, env string }{
		"claude": {"anthropic", "ANTHROPIC_API_KEY"},
		"codex":  {"openai", "OPENAI_API_KEY"},
	}
	return func(name string) []string {
		k, ok := keys[name]
		if !ok {
			return nil
		}
		p, ok := cfg.Providers[k.provider]
		if !ok || p.APIKey == "" {
			return nil
		}
		v, err := p.ResolveAPIKey()
		if err != nil {
			log.Warn("API-Key nicht auflösbar, die CLI nutzt ihre eigene Anmeldung", "provider", k.provider, "err", err)
			return nil
		}
		return []string{k.env + "=" + v}
	}
}
