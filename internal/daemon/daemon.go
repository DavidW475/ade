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
	"os/exec"
	"path/filepath"
	"syscall"
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
	"github.com/DavidW475/ade/internal/proxy"
	"github.com/DavidW475/ade/internal/sandbox"
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

	sb := newSandbox(cfg, version, log)
	sb.SetEgressSink(func(ws string, evs []proxy.Event) { storeEgress(st, log, ws, evs) })
	sb.SetSpecSource(workspaces.SandboxSpec)
	defer sb.Close()
	workspaces.SetSandbox(sb, cfg.Defaults.Sandbox)
	// Abgleich vor dem ersten Agenten, damit er keinen frisch gestarteten Container stoppt.
	rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
	if err := sb.Recover(rctx, knownWorkspace(st)); err != nil {
		log.Info("Sandbox-Abgleich übersprungen", "grund", err)
	}
	rcancel()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := sb.StopAll(sctx); err != nil {
			log.Debug("Sandbox-Container stoppen", "err", err)
		}
	}()

	agents := agent.NewManager(agent.Options{
		Store:          st,
		Projects:       projects,
		Workspaces:     workspaces,
		Adapters:       adapter.NewRegistry(claude.New(), pty.New()),
		DataDir:        cfg.DataDir,
		Logger:         log,
		DefaultAdapter: cfg.Defaults.Adapter,
		AdapterEnv:     adapterEnv(cfg, log),
		SandboxEnv:     sandboxAdapterEnv(cfg),
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
		Sandbox: func(ctx context.Context, id string) (ide.Sandbox, error) {
			w, err := workspaces.Get(ctx, id)
			if err != nil || !w.Sandbox {
				return nil, err
			}
			if err := workspaces.EnsureSandbox(ctx, id); err != nil {
				return nil, err
			}
			return ideSandbox{sb: sb, ws: id}, nil
		},
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
			Sandbox:    sb,
			Egress:     st,
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
// nutzt die CLI ihre eigene Anmeldung (ADR-010). Gilt nur für Agenten ohne Sandbox.
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

// injectedProviders sind die Anbieter mit Secret-Injection über den Egress-Proxy: Variable
// für die Basis-URL und für den Platzhalter-Key, den die CLI statt des echten Keys sieht.
var injectedProviders = map[string]struct{ baseURL, key string }{
	"anthropic": {"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY"},
}

// KeyPlaceholder steht im Container statt eines API-Keys; der Proxy ersetzt ihn.
const KeyPlaceholder = "ade-proxy-injects-the-key"

// sandboxSecrets liefert die aufgelösten API-Keys für die Proxys (Secret-Injection).
func sandboxSecrets(cfg *config.Config, log *slog.Logger) func() map[string]string {
	return func() map[string]string {
		out := map[string]string{}
		for name := range injectedProviders {
			p, ok := cfg.Providers[name]
			if !ok || p.APIKey == "" {
				continue
			}
			v, err := p.ResolveAPIKey()
			if err != nil {
				log.Warn("API-Key nicht auflösbar", "provider", name, "err", err)
				continue
			}
			out[name] = v
		}
		return out
	}
}

// sandboxAdapterEnv liefert die Umgebung für Agenten in der Sandbox: Für jeden Anbieter mit
// konfiguriertem Key zeigt die Basis-URL auf den Proxy, der Key ist nur ein Platzhalter.
// Ohne Key nutzt die CLI ihre eigene Anmeldung über die Allowlist (Preset anthropic).
func sandboxAdapterEnv(cfg *config.Config) func(string) []string {
	return func(string) []string {
		var env []string
		for name, v := range injectedProviders {
			if p, ok := cfg.Providers[name]; ok && p.APIKey != "" {
				env = append(env, v.baseURL+"="+sandbox.ProviderBaseURL(name), v.key+"="+KeyPlaceholder)
				if name == "anthropic" {
					// Telemetrie u. Ä. ginge sonst mit dem Platzhalter direkt an Anthropic.
					env = append(env, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
				}
			}
		}
		return env
	}
}

func newSandbox(cfg *config.Config, version string, log *slog.Logger) *sandbox.Manager {
	image := cfg.Sandbox.Image
	if image == "" {
		image = sandbox.DefaultImage(version)
	}
	opts := sandbox.Options{Docker: cfg.Sandbox.Docker, DataDir: cfg.DataDir, Image: image, Logger: log, Secrets: sandboxSecrets(cfg, log)}
	if exe, err := os.Executable(); err == nil {
		if bin, err := sandbox.InstallProxyBinary(exe, filepath.Join(cfg.DataDir, "sandbox")); err == nil {
			opts.ProxyBinary = bin
		} else {
			log.Warn("Egress-Proxy nicht verfügbar – Sandbox-Workspaces lassen sich nicht starten", "err", err)
		}
	}
	return sandbox.New(opts)
}

// storeEgress speichert Egress-Events eines Proxys.
func storeEgress(st *store.Store, log *slog.Logger, ws string, evs []proxy.Event) {
	out := make([]store.EgressEvent, len(evs))
	for i, e := range evs {
		out[i] = store.EgressEvent{
			TS: e.Time, Kind: e.Kind, Method: e.Method, Host: e.Host, Port: e.Port, Path: e.Path, Provider: e.Provider,
			Allowed: e.Allowed, Reason: e.Reason, Status: e.Status, BytesOut: e.BytesOut, BytesIn: e.BytesIn,
			DurationMS: e.DurationMS, Error: e.Error,
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := st.AppendEgress(ctx, ws, out); err != nil {
		log.Warn("Egress-Events speichern", "workspace", ws, "err", err)
	}
}

// knownWorkspace meldet, ob es einen Workspace gibt (für den Abgleich der Container).
// Bei Datenbankfehlern gilt er als bekannt, damit nichts versehentlich gelöscht wird.
func knownWorkspace(st *store.Store) func(string) bool {
	return func(id string) bool {
		_, err := st.GetWorkspace(context.Background(), id)
		return !errors.Is(err, apperr.ErrNotFound)
	}
}

// ideSandbox bindet den Sandbox-Manager an einen Workspace für die IDE.
type ideSandbox struct {
	sb *sandbox.Manager
	ws string
}

func (s ideSandbox) Command(ctx context.Context, argv, env []string) *exec.Cmd {
	return s.sb.Command(ctx, s.ws, sandbox.Exec{Argv: argv, Env: env})
}

func (s ideSandbox) Signal(ctx context.Context, marker string, sig syscall.Signal) error {
	return s.sb.Signal(ctx, s.ws, marker, sig)
}

// HostPort liefert den Port der IDE-Weiterleitung des Proxys (das interne Netz des
// Agent-Containers veröffentlicht keine Ports).
func (s ideSandbox) HostPort(ctx context.Context, _ int) (int, error) {
	return s.sb.IDEHostPort(ctx, s.ws)
}

func (s ideSandbox) WriteFile(ctx context.Context, path string, data []byte) error {
	return s.sb.WriteFile(ctx, s.ws, path, data)
}

// GC entfernt verwaiste Sandbox-Container, ohne dass ein Daemon läuft. Es hält dafür das
// Lock des Datenverzeichnisses, damit nicht gleichzeitig ein Daemon startet.
func GC(ctx context.Context, cfg *config.Config, version string, dryRun bool, log *slog.Logger) ([]sandbox.Container, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	lock, err := acquireLock(cfg.LockPath())
	if err != nil {
		return nil, err
	}
	defer lock.release()
	st, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	return newSandbox(cfg, version, log).GC(ctx, knownWorkspace(st), dryRun)
}
