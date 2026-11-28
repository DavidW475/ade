// Package daemon starts the ADE daemon and provides the client the CLI uses to talk to it.
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
	"github.com/DavidW475/ade/internal/agent/adapter/codex"
	"github.com/DavidW475/ade/internal/agent/adapter/pty"
	"github.com/DavidW475/ade/internal/api"
	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/approval"
	"github.com/DavidW475/ade/internal/config"
	"github.com/DavidW475/ade/internal/factory"
	"github.com/DavidW475/ade/internal/ide"
	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/provider"
	"github.com/DavidW475/ade/internal/proxy"
	"github.com/DavidW475/ade/internal/sandbox"
	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/trigger"
	"github.com/DavidW475/ade/internal/workspace"
)

// Run starts the daemon and blocks until ctx is done.
func Run(ctx context.Context, cfg *config.Config, version string, log *slog.Logger) error {
	if err := privateDir(cfg.DataDir, log); err != nil {
		return err
	}

	// One daemon per data directory – even if a second one would run on another port.
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
	var agents *agent.Manager
	sb.SetEgressSink(func(ws string, evs []proxy.Event) {
		storeEgress(st, log, ws, evs)
		if agents != nil {
			agents.RecordProxyUsage(context.Background(), ws, proxyUsage(evs))
		}
	})
	sb.SetSpecSource(workspaces.SandboxSpec)
	defer sb.Close()

	approvals := approval.New(approval.Options{
		Store: st, Timeout: cfg.Approvals.Timeout, Logger: log, Rules: projects.ApprovalConfig,
		OnGrant: sb.ReloadPolicies,
		// Local read-only roles of a factory work in the snapshot (agent/snapshot.go).
		Workdir: func(sess *store.Session) string {
			if sess.ReadOnly {
				return agent.SnapshotDir(cfg.DataDir, sess.ID)
			}
			return ""
		},
	})
	if err := approvals.Recover(ctx); err != nil {
		return err
	}
	// Runs before sb.Close (defer order): wake waiting requests before the proxies are disconnected.
	defer approvals.Close()
	sb.SetQuestionHandler(approvals.Question)
	workspaces.SetSandbox(sb, cfg.Defaults.Sandbox)
	// Reconcile before the first agent, so it does not stop a freshly started container.
	rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
	if err := sb.Recover(rctx, knownWorkspace(st)); err != nil {
		log.Info("sandbox reconciliation skipped", "reason", err)
	}
	rcancel()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := sb.StopAll(sctx); err != nil {
			log.Debug("stop sandbox containers", "err", err)
		}
	}()

	agents = agent.NewManager(agent.Options{
		Store:          st,
		Projects:       projects,
		Workspaces:     workspaces,
		Adapters:       adapter.NewRegistry(claude.New(), codex.New(), pty.New()),
		DataDir:        cfg.DataDir,
		Logger:         log,
		DefaultAdapter: cfg.Defaults.Adapter,
		AdapterEnv:     adapterEnv(cfg, log),
		SandboxEnv:     sandboxAdapterEnv(cfg),
		Hooks:          newHooks(cfg, approvals, sb.HookAvailable(), log),
		Access:         providerAccess(cfg),
		Catalog:        provider.NewCatalog(cfg),
		Budgets:        approvals,
	})
	if err := agents.Recover(ctx); err != nil {
		return err
	}
	// Runs before workspaces.Close (defer order): first end agents, then setups.
	defer agents.Close()

	runs := factory.New(factory.Options{
		Store: st, Projects: projects, Workspaces: workspaces, Agents: agents, Gates: approvals,
		DataDir: cfg.DataDir, Logger: log,
	})
	if err := runs.Recover(ctx); err != nil {
		return err
	}
	// Runs before agents.Close (defer order): the runner should not resume interrupted
	// sessions right away.
	defer runs.Close()

	// Runs before runs.Close (defer order): no new runs while the runner ends.
	triggers := trigger.New(trigger.Options{Store: st, Projects: projects, Runs: runs, Logger: log})
	triggers.Start()
	defer triggers.Close()

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
				return "", apperr.Conflict("workspace %s has no working directory (status %s)", id, w.Status)
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
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
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
			Approvals:  approvals,
			Usage:      st,
			Runs:       runs,
			Triggers:   triggers,
			// Like the manager: without configuration claude applies.
			DefaultAdapter: defaultAdapter(cfg),
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("ADE daemon running", "listen", cfg.Listen, "url", cfg.BaseURL(), "version", version)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("ADE daemon stopping")
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

// writeSecret writes the secret atomically and readable only by the user.
func writeSecret(path, secret string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".daemon.secret-*")
	if err != nil {
		return fmt.Errorf("write daemon secret: %w", err)
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

// adapterEnv passes API keys from the configuration to the matching adapters. Without an
// entry the CLI uses its own login (ADR-010). Only applies to agents without a sandbox.
func adapterEnv(cfg *config.Config, log *slog.Logger) func(string) []string {
	keys := map[string]struct{ provider, env string }{
		"claude": {"anthropic", "ANTHROPIC_API_KEY"},
		"codex":  {"openai", "CODEX_API_KEY"},
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
			log.Warn("API key cannot be resolved, the CLI uses its own login", "provider", k.provider, "err", err)
			return nil
		}
		return []string{k.env + "=" + v}
	}
}

// injectedProviders are the providers with secret injection through the egress proxy: variable
// for the base URL and for the placeholder key the CLI sees instead of the real key.
var injectedProviders = map[string]struct{ baseURL, key string }{
	"anthropic": {"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY"},
	// Codex ignores OPENAI_BASE_URL; providerAccess passes the base URL as an argument.
	"openai": {"OPENAI_BASE_URL", "OPENAI_API_KEY"},
}

// openAIBaseURL is the base URL of the OpenAI API through the egress proxy.
var openAIBaseURL = sandbox.ProviderBaseURL(provider.OpenAI) + "/v1"

// providerAccess determines for an adapter whether the proxy inserts the key in the sandbox
// (then it also measures the usage) and which base URL the CLI needs as an argument.
func providerAccess(cfg *config.Config) func(string, bool) (*adapter.ProviderAccess, bool) {
	return func(name string, sandboxed bool) (*adapter.ProviderAccess, bool) {
		prov := provider.ForAdapter(name)
		if !sandboxed || prov == "" {
			return nil, false
		}
		if p, ok := cfg.Providers[prov]; !ok || p.APIKey == "" {
			return nil, false
		}
		if _, ok := injectedProviders[prov]; !ok {
			return nil, false
		}
		if name == "codex" {
			return &adapter.ProviderAccess{BaseURL: openAIBaseURL, KeyEnv: injectedProviders[prov].key}, true
		}
		return nil, true
	}
}

// proxyUsage gets the token usage from egress events.
func proxyUsage(evs []proxy.Event) []agent.ProxyUsage {
	var out []agent.ProxyUsage
	for _, e := range evs {
		if e.Usage == nil {
			continue
		}
		u := e.Usage
		out = append(out, agent.ProxyUsage{
			Provider: e.Provider, Model: u.Model, Time: e.Time,
			Tokens: provider.Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite},
		})
	}
	return out
}

// KeyPlaceholder is in the container instead of an API key; the proxy replaces it.
const KeyPlaceholder = "ade-proxy-injects-the-key"

// sandboxSecrets returns the resolved API keys for the proxies (secret injection).
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
				log.Warn("API key cannot be resolved", "provider", name, "err", err)
				continue
			}
			out[name] = v
		}
		return out
	}
}

// sandboxAdapterEnv returns the environment for agents in the sandbox: for every provider with
// a configured key the base URL points to the proxy, the key is only a placeholder.
// Without a key the CLI uses its own login through the allowlist (preset anthropic).
func sandboxAdapterEnv(cfg *config.Config) func(string) []string {
	return func(string) []string {
		var env []string
		for name, v := range injectedProviders {
			if p, ok := cfg.Providers[name]; ok && p.APIKey != "" {
				env = append(env, v.baseURL+"="+sandbox.ProviderBaseURL(name), v.key+"="+KeyPlaceholder)
				if name == "anthropic" {
					// Telemetry and the like would otherwise go to Anthropic directly with the placeholder.
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
			log.Warn("egress proxy not available – sandbox workspaces cannot be started", "err", err)
		}
	}
	return sandbox.New(opts)
}

// hooks provides Claude sessions with the approval hook (agent.Hooks): locally the running
// ade binary with a token per session, in the sandbox the mounted binary that asks through
// the egress proxy.
type hooks struct {
	svc       *approval.Service
	local     string // shell command locally (empty: no hook)
	localURL  string
	sandboxOK bool
}

func newHooks(cfg *config.Config, svc *approval.Service, sandboxOK bool, log *slog.Logger) *hooks {
	h := &hooks{svc: svc, localURL: "http://" + cfg.Listen + "/api/hook/pretooluse", sandboxOK: sandboxOK}
	if exe, err := os.Executable(); err == nil {
		h.local = approval.ShellQuote(exe) + " hook pretooluse"
	} else {
		log.Warn("approval hook for local agents not available", "err", err)
	}
	return h
}

func (h *hooks) Prepare(sessionID string, sandboxed bool) (*adapter.Hook, []string) {
	if sandboxed {
		if !h.sandboxOK {
			return nil, nil
		}
		return &adapter.Hook{Command: sandbox.ProxyBinary + " hook pretooluse", Timeout: h.svc.Timeout()},
			[]string{approval.EnvHookURL + "=" + sandbox.ProxyURL + proxy.HookPath}
	}
	if h.local == "" {
		return nil, nil
	}
	return &adapter.Hook{Command: h.local, Timeout: h.svc.Timeout()},
		[]string{approval.EnvHookURL + "=" + h.localURL, approval.EnvHookToken + "=" + h.svc.IssueToken(sessionID)}
}

func (h *hooks) Release(sessionID string) {
	h.svc.RevokeTokens(sessionID)
	h.svc.CancelSession(context.Background(), sessionID)
}

// storeEgress stores egress events of a proxy.
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
		log.Warn("store egress events", "workspace", ws, "err", err)
	}
}

// knownWorkspace reports whether a workspace exists (for reconciling the containers).
// On database errors it counts as known, so nothing is deleted by accident.
func knownWorkspace(st *store.Store) func(string) bool {
	return func(id string) bool {
		_, err := st.GetWorkspace(context.Background(), id)
		return !errors.Is(err, apperr.ErrNotFound)
	}
}

// ideSandbox binds the sandbox manager to a workspace for the IDE.
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

// HostPort returns the port of the proxy's IDE forwarding (the agent container's internal
// network publishes no ports).
func (s ideSandbox) HostPort(ctx context.Context, _ int) (int, error) {
	return s.sb.IDEHostPort(ctx, s.ws)
}

func (s ideSandbox) WriteFile(ctx context.Context, path string, data []byte) error {
	return s.sb.WriteFile(ctx, s.ws, path, data)
}

// privateDir creates the data directory and revokes access for group and others if it
// already exists with more permissions (e.g. created by hand): it contains the daemon
// secret, database and logs.
func privateDir(dir string, log *slog.Logger) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("check data directory: %w", err)
	}
	if m := fi.Mode().Perm(); m&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs the x bit
			return fmt.Errorf("set permissions of the data directory: %w", err)
		}
		log.Warn("data directory was readable by others, permissions set to 0700", "dir", dir, "before", fmt.Sprintf("%o", m))
	}
	return nil
}

// GC removes orphaned sandbox containers without a running daemon. It holds the data
// directory's lock for that, so that no daemon starts at the same time.
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
