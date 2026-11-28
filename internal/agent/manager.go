// Package agent manages the life cycle of agent sessions: start the process in the workspace
// (with PTY or pipes), translate the output into events through adapters, store and
// distribute events live, follow-up instructions, stopping and resuming after a daemon restart.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/DavidW475/ade/internal/agent/adapter"
	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/provider"
	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/workspace"
)

// DefaultStopTimeout is the time between SIGTERM and SIGKILL when stopping.
const DefaultStopTimeout = 5 * time.Second

// ResumePrompt is the instruction when a session is resumed without its own text.
const ResumePrompt = "Continue where you left off."

// Options configures the manager.
type Options struct {
	Store      *store.Store
	Projects   *project.Service
	Workspaces *workspace.Manager
	Adapters   *adapter.Registry
	// DataDir takes the terminal logs (<DataDir>/sessions/<id>.log).
	DataDir string
	Logger  *slog.Logger
	// DefaultAdapter applies if neither the request nor project.yaml names one.
	DefaultAdapter string
	// AdapterEnv returns additional environment variables for an adapter without a sandbox
	// (e.g. API keys).
	AdapterEnv func(adapter string) []string
	// SandboxEnv returns them for an adapter in the sandbox: never real keys, but base URLs
	// to the egress proxy that inserts the keys (doc/sandbox.md#4-secrets).
	SandboxEnv func(adapter string) []string
	// Hooks returns the approval hook for adapters with Capabilities.Approvals (optional).
	Hooks Hooks
	// Access returns an adapter's access to its provider (base URL, if the CLI needs it as an
	// argument) and whether the egress proxy measures the usage (optional).
	Access func(adapter string, sandbox bool) (access *adapter.ProviderAccess, metered bool)
	// Catalog resolves model aliases and computes costs (nil: no aliases, built-in prices
	// only).
	Catalog *provider.Catalog
	// Budgets asks for an approval when a budget is exceeded (nil: the session stays paused
	// until somebody resumes it).
	Budgets Budgets
	// FlushInterval and StopTimeout override the defaults (tests).
	FlushInterval time.Duration
	StopTimeout   time.Duration
}

// Hooks provides a session's approval hook (doc/architecture.md#43-approvals).
type Hooks interface {
	// Prepare returns hook and additional environment for a session (nil: no hook).
	Prepare(sessionID string, sandbox bool) (*adapter.Hook, []string)
	// Release releases the credentials of an ended session.
	Release(sessionID string)
}

// Budgets creates budget approvals (doc/factory.md#3-execution).
type Budgets interface {
	// RequestBudget creates the approval a; decided runs after the decision.
	RequestBudget(ctx context.Context, a *store.Approval, decided func(*store.Approval)) error
}

// Manager starts and supervises agent sessions.
type Manager struct {
	opts Options
	log  *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	runs     map[string]*run // sessions with a (waiting or running) process
	hubs     map[string]*hub
	closing  bool
	listener func(sessionID, status string)
}

// SetStatusListener sets a function that runs after every status change of a session (e.g.
// the factory runner). It runs in its own goroutine and may block.
func (m *Manager) SetStatusListener(fn func(sessionID, status string)) {
	m.mu.Lock()
	m.listener = fn
	m.mu.Unlock()
}

func (m *Manager) notify(id, status string) {
	m.mu.Lock()
	fn := m.listener
	m.mu.Unlock()
	if fn != nil {
		go fn(id, status)
	}
}

// NewManager creates a manager.
func NewManager(opts Options) *Manager {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = DefaultFlushInterval
	}
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = DefaultStopTimeout
	}
	if opts.DefaultAdapter == "" {
		opts.DefaultAdapter = "claude"
	}
	if opts.Catalog == nil {
		opts.Catalog = provider.NewCatalog(nil)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		opts: opts, log: opts.Logger, ctx: ctx, cancel: cancel,
		runs: map[string]*run{}, hubs: map[string]*hub{},
	}
}

// Adapters returns the registry.
func (m *Manager) Adapters() *adapter.Registry { return m.opts.Adapters }

// Catalog returns model aliases and prices.
func (m *Manager) Catalog() *provider.Catalog { return m.opts.Catalog }

// StartRequest is the body of POST /api/agents.
type StartRequest struct {
	ProjectID string `json:"project_id"`
	// WorkspaceID is optional; without it ADE creates a new workspace.
	WorkspaceID string `json:"workspace_id,omitempty"`
	// Adapter is optional (default: project.yaml, then the global configuration).
	Adapter string `json:"adapter,omitempty"`
	Task    string `json:"task,omitempty"`
	// Command is the command for terminal adapters (default: login shell).
	Command        []string `json:"command,omitempty"`
	Model          string   `json:"model,omitempty"`
	PermissionMode string   `json:"permission_mode,omitempty"`
	// Sandbox applies to a new workspace (nil: defaults.sandbox). For an existing workspace
	// it must match it, if set.
	Sandbox *bool `json:"sandbox,omitempty"`
	// BudgetUSD is the session's budget (nil: budget.session_usd from project.yaml, 0: none).
	BudgetUSD *float64 `json:"budget_usd,omitempty"`
}

// Start creates a session and starts the process in the background as soon as the
// workspace's setup is done.
func (m *Manager) Start(ctx context.Context, req StartRequest) (*store.Session, error) {
	return m.start(ctx, req, startExtra{})
}

// RoleStart are the start options of a factory role (StartRole).
type RoleStart struct {
	// Role is the name of the role (shown on the session).
	Role string
	// Prompt is the complete instruction (role prompt, task, artifacts).
	Prompt string
	// ReadOnly: the role must not change the workspace. In the sandbox it runs in the reader
	// container (/workspace read-only), otherwise in a snapshot of the workspace.
	ReadOnly bool
}

// StartRole starts a session for a factory role in the existing workspace
// req.WorkspaceID.
func (m *Manager) StartRole(ctx context.Context, req StartRequest, rs RoleStart) (*store.Session, error) {
	if req.WorkspaceID == "" {
		return nil, apperr.Invalid("workspace_id missing")
	}
	if len(rs.Prompt) > maxRolePrompt {
		return nil, apperr.Invalid("instruction of role %s is too long (at most %d KiB)", rs.Role, maxRolePrompt>>10)
	}
	return m.start(ctx, req, startExtra(rs))
}

// maxRolePrompt limits a role's instruction (prompt, task and artifacts).
const maxRolePrompt = 512 << 10

// startExtra are internal start options (e.g. for feedback sessions and roles).
type startExtra struct {
	Role string
	// Prompt replaces the task as the instruction to the agent; Task stays the short version
	// for list and history.
	Prompt   string
	ReadOnly bool
}

func (m *Manager) start(ctx context.Context, req StartRequest, extra startExtra) (*store.Session, error) {
	req.Task = strings.TrimSpace(req.Task)
	ws, p, err := m.resolveWorkspace(ctx, req.ProjectID, req.WorkspaceID)
	if err != nil {
		return nil, err
	}
	name := req.Adapter
	if name == "" && p != nil {
		name = p.Config.Adapter
	}
	if name == "" {
		name = m.opts.DefaultAdapter
	}
	a, err := m.opts.Adapters.Get(name)
	if err != nil {
		return nil, apperr.Invalid("%v", err)
	}
	caps := a.Capabilities()
	if !caps.Terminal && req.Task == "" {
		return nil, apperr.Invalid("task missing")
	}
	if len(req.Command) > 0 && !caps.Terminal {
		return nil, apperr.Invalid("a command is only possible with the pty adapter")
	}
	for _, c := range req.Command {
		if strings.ContainsRune(c, 0) {
			return nil, apperr.Invalid("invalid character in the command")
		}
	}
	if len(req.Task) > 100<<10 {
		return nil, apperr.Invalid("task is too long (at most 100 KiB)")
	}
	modelID, err := m.resolveModel(name, req.Model)
	if err != nil {
		return nil, err
	}
	budget := p.Config.SessionBudget()
	if req.BudgetUSD != nil {
		if b := *req.BudgetUSD; b < 0 || b > project.MaxBudgetUSD || b != b {
			return nil, apperr.Invalid("budget_usd: between 0 and %.0f", project.MaxBudgetUSD)
		}
		budget = *req.BudgetUSD
	}
	sandboxed := m.opts.Workspaces.SandboxDefault()
	if req.Sandbox != nil {
		sandboxed = *req.Sandbox
	}
	if ws != nil {
		if req.Sandbox != nil && *req.Sandbox != ws.Sandbox {
			if ws.Sandbox {
				return nil, apperr.Invalid("workspace %s runs in the sandbox – create a new workspace for an agent without a sandbox", ws.ID)
			}
			return nil, apperr.Invalid("workspace %s runs without a sandbox – create a new workspace for the sandbox", ws.ID)
		}
		sandboxed = ws.Sandbox
	}
	// Check up front whether the call can be built (e.g. CLI installed, options valid).
	if _, err := a.Command(adapter.Spec{Workdir: "/", Task: req.Task, Command: req.Command, Model: modelID, PermissionMode: req.PermissionMode, Sandbox: sandboxed, ReadOnly: extra.ReadOnly}); err != nil {
		return nil, apperr.Invalid("%v", err)
	}

	if ws == nil {
		created, err := m.opts.Workspaces.Create(ctx, p.ID, workspace.CreateOptions{Sandbox: &sandboxed})
		if err != nil {
			return nil, err
		}
		ws = created
	}
	if !caps.Terminal {
		if other := m.activeAgentIn(ws.ID, extra.ReadOnly); other != "" {
			return nil, apperr.Conflict("agent %s is already running in workspace %s – stop it first or use a new workspace", other, ws.ID)
		}
	}

	sess := &store.Session{
		ID:             project.NewID(),
		WorkspaceID:    ws.ID,
		Adapter:        name,
		Task:           req.Task,
		Command:        req.Command,
		Model:          req.Model,
		PermissionMode: req.PermissionMode,
		Role:           extra.Role,
		Status:         store.SessionStarting,
		BudgetUSD:      budget,
		ReadOnly:       extra.ReadOnly,
	}
	if modelID != req.Model {
		sess.ModelID = modelID
	}
	if err := m.opts.Store.CreateSession(ctx, sess); err != nil {
		return nil, err
	}
	m.emit(sess.ID, adapter.TypeStatus, adapter.Status{Status: store.SessionStarting, Reason: "session created"})
	if req.Task != "" {
		m.emit(sess.ID, adapter.TypeMessage, adapter.Message{Role: "user", Text: req.Task})
	}
	prompt := req.Task
	if extra.Prompt != "" {
		prompt = extra.Prompt
	}
	spec := adapter.Spec{SessionID: sess.ID, Workdir: ws.Path, Task: prompt, Command: req.Command, Model: modelID, PermissionMode: req.PermissionMode, Sandbox: ws.Sandbox, ReadOnly: sess.ReadOnly}
	// Return the state at creation: the process can change the status right away.
	created := *sess
	if err := m.launch(sess, a, ws, spec); err != nil {
		return nil, err
	}
	m.log.Info("session started", "id", sess.ID, "workspace", ws.ID, "adapter", name)
	return &created, nil
}

// resolveModel resolves a model alias for the adapter name. Names that are not an alias go
// to the CLI unchanged.
func (m *Manager) resolveModel(name, model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", nil
	}
	id, prov, alias := m.opts.Catalog.Resolve(model)
	if !alias {
		return model, nil
	}
	if want := provider.ForAdapter(name); want != "" && prov != want {
		return "", apperr.Invalid("model %s belongs to %s, the adapter %s talks to %s", model, prov, name, want)
	}
	return id, nil
}

func (m *Manager) resolveWorkspace(ctx context.Context, projectID, workspaceID string) (*store.Workspace, *project.View, error) {
	var ws *store.Workspace
	if workspaceID != "" {
		w, err := m.opts.Workspaces.Get(ctx, workspaceID)
		if err != nil {
			return nil, nil, err
		}
		if projectID != "" && projectID != w.ProjectID {
			return nil, nil, apperr.Invalid("workspace %s does not belong to project %s", workspaceID, projectID)
		}
		switch w.Status {
		case store.WorkspaceCreating, store.WorkspaceError:
			return nil, nil, apperr.Conflict("workspace %s is not ready (status %s)", w.ID, w.Status)
		}
		ws, projectID = w, w.ProjectID
	}
	if projectID == "" {
		return nil, nil, apperr.Invalid("project_id or workspace_id missing")
	}
	p, err := m.opts.Projects.Get(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	return ws, p, nil
}

// activeAgentIn returns the ID of a running non-terminal session in the workspace that is in
// the way of a new agent. Read-only roles work in their own container or snapshot: they may
// run next to any agent and are in nobody's way (parallel steps of a factory). There are
// never two agents with write access in the same workspace.
func (m *Manager) activeAgentIn(workspaceID string, readOnly bool) string {
	if readOnly {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.runs {
		if r.workspaceID == workspaceID && !r.caps.Terminal && !r.readOnly {
			return id
		}
	}
	return ""
}

// Get returns a session.
func (m *Manager) Get(ctx context.Context, id string) (*View, error) {
	s, err := m.opts.Store.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	return m.view(s), nil
}

// List returns sessions.
func (m *Manager) List(ctx context.Context, f store.SessionFilter) ([]*View, error) {
	ss, err := m.opts.Store.ListSessions(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]*View, len(ss))
	for i, s := range ss {
		out[i] = m.view(s)
	}
	return out, nil
}

// View is a session with what the UI needs in addition.
type View struct {
	*store.Session
	Capabilities adapter.Capabilities `json:"capabilities"`
	// Live: a process of this daemon currently belongs to the session.
	Live bool `json:"live"`
	// Resumable: the session can be resumed with an instruction.
	Resumable bool `json:"resumable"`
}

func (m *Manager) view(s *store.Session) *View {
	v := &View{Session: s}
	if a, err := m.opts.Adapters.Get(s.Adapter); err == nil {
		v.Capabilities = a.Capabilities()
	}
	m.mu.Lock()
	_, v.Live = m.runs[s.ID]
	m.mu.Unlock()
	v.Resumable = !v.Live && v.Capabilities.Resume && s.ResumeID != "" && !store.SessionActive(s.Status)
	return v
}

// Input sends a follow-up instruction to a running session or resumes an ended session if
// the adapter can do that.
func (m *Manager) Input(ctx context.Context, id, text string) (*View, error) {
	text = strings.TrimSpace(text)
	sess, err := m.opts.Store.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	a, err := m.opts.Adapters.Get(sess.Adapter)
	if err != nil {
		return nil, apperr.Invalid("%v", err)
	}
	caps := a.Capabilities()

	m.mu.Lock()
	r := m.runs[id]
	m.mu.Unlock()
	if r != nil {
		if !caps.Input {
			return nil, apperr.Invalid("adapter %s does not accept follow-up instructions", sess.Adapter)
		}
		if text == "" {
			return nil, apperr.Invalid("empty instruction")
		}
		// Status and message before sending: otherwise a fast agent can finish its turn (idle)
		// before “running” is set, and would then seem busy.
		r.mu.Lock()
		prev := r.status
		r.mu.Unlock()
		r.setStatus(store.SessionRunning, "")
		if err := r.send(a, text); err != nil {
			r.setStatus(prev, "")
			return nil, err
		}
		m.emit(id, adapter.TypeMessage, adapter.Message{Role: "user", Text: text})
		return m.Get(ctx, id)
	}

	if !caps.Resume || sess.ResumeID == "" {
		return nil, apperr.Conflict("session %s is not running and cannot be resumed", id)
	}
	if store.SessionActive(sess.Status) {
		return nil, apperr.Conflict("session %s has status %s but no process", id, sess.Status)
	}
	ws, err := m.opts.Workspaces.Get(ctx, sess.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if other := m.activeAgentIn(ws.ID, sess.ReadOnly); other != "" && !caps.Terminal {
		return nil, apperr.Conflict("agent %s is already running in workspace %s", other, ws.ID)
	}
	if text == "" {
		text = ResumePrompt
	}
	if err := m.opts.Store.UpdateSession(ctx, id, store.SessionUpdate{
		Status: store.Ptr(store.SessionStarting), Error: store.Ptr(""), ClearEnded: true, ClearExitCode: true,
	}); err != nil {
		return nil, err
	}
	m.emit(id, adapter.TypeStatus, adapter.Status{Status: store.SessionStarting, Reason: "resuming"})
	m.emit(id, adapter.TypeMessage, adapter.Message{Role: "user", Text: text})
	model := sess.Model
	if sess.ModelID != "" {
		model = sess.ModelID
	}
	spec := adapter.Spec{SessionID: id, Workdir: ws.Path, Task: text, Model: model, PermissionMode: sess.PermissionMode, ResumeID: sess.ResumeID, Sandbox: ws.Sandbox, ReadOnly: sess.ReadOnly}
	if err := m.launch(sess, a, ws, spec); err != nil {
		return nil, err
	}
	m.log.Info("session resumed", "id", id, "resume_id", sess.ResumeID)
	return m.Get(ctx, id)
}

// Stop ends a session's process and briefly waits for it to end.
func (m *Manager) Stop(ctx context.Context, id string) (*View, error) {
	sess, err := m.opts.Store.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	r := m.runs[id]
	m.mu.Unlock()
	if r == nil {
		if store.SessionActive(sess.Status) {
			// No process but an active status (should not happen): clean up the status.
			m.finish(id, store.SessionStopped, "", nil)
		}
		return m.Get(ctx, id)
	}
	r.stop(store.SessionStopped)
	select {
	case <-r.done:
	case <-ctx.Done():
	case <-time.After(m.opts.StopTimeout + 2*time.Second):
	}
	return m.Get(ctx, id)
}

// BeforeWorkspaceDelete stops all sessions of a workspace and deletes their terminal logs.
// The database removes the records together with the workspace.
func (m *Manager) BeforeWorkspaceDelete(ctx context.Context, workspaceID string) error {
	m.mu.Lock()
	var runs []*run
	for _, r := range m.runs {
		if r.workspaceID == workspaceID {
			runs = append(runs, r)
		}
	}
	m.mu.Unlock()
	for _, r := range runs {
		r.stop(store.SessionStopped)
	}
	for _, r := range runs {
		select {
		case <-r.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ss, err := m.opts.Store.ListSessions(ctx, store.SessionFilter{WorkspaceID: workspaceID, Limit: 100000})
	if err != nil {
		return err
	}
	for _, s := range ss {
		m.flushHub(s.ID)
		_ = os.Remove(m.terminalLog(s.ID))
		if s.ReadOnly {
			m.removeSnapshot(s.ID)
		}
	}
	return nil
}

// Dismiss ends a session's process with the status “done” (e.g. a waiting agent whose turn
// the factory runner has taken over) and waits for it to end including the workspace backup.
// Without a process nothing happens.
func (m *Manager) Dismiss(ctx context.Context, id string) error {
	m.mu.Lock()
	r := m.runs[id]
	m.mu.Unlock()
	if r == nil {
		return nil
	}
	r.stop(store.SessionDone)
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// LastMessage returns the agent's last answer of a session (empty: none).
func (m *Manager) LastMessage(ctx context.Context, id string) (string, error) {
	m.flushHub(id)
	evs, err := m.opts.Store.LastEvents(ctx, id, adapter.TypeMessage, 200)
	if err != nil {
		return "", err
	}
	for _, e := range evs {
		var msg adapter.Message
		if json.Unmarshal(e.Payload, &msg) == nil && msg.Role == "assistant" && strings.TrimSpace(msg.Text) != "" {
			return msg.Text, nil
		}
	}
	return "", nil
}

// Recover cleans up after a restart or crash of the daemon: orphaned processes of earlier
// sessions are ended, active sessions marked as interrupted.
func (m *Manager) Recover(ctx context.Context) error {
	ss, err := m.opts.Store.ListSessions(ctx, store.SessionFilter{Active: true, Limit: 100000})
	if err != nil {
		return err
	}
	ids := map[string]bool{}
	for _, s := range ss {
		ids[s.ID] = true
	}
	if killed := killOrphans(ids); len(killed) > 0 {
		m.log.Warn("orphaned agent processes ended", "count", len(killed))
	}
	for _, s := range ss {
		if err := m.opts.Store.UpdateSession(ctx, s.ID, store.SessionUpdate{
			Status: store.Ptr(store.SessionInterrupted), Error: store.Ptr("interrupted by a daemon restart"),
			PID: store.Ptr(0), Ended: true,
		}); err != nil {
			return err
		}
		m.emit(s.ID, adapter.TypeStatus, adapter.Status{Status: store.SessionInterrupted, Reason: "daemon was restarted"})
	}
	if len(ss) > 0 {
		m.log.Warn("interrupted sessions marked", "count", len(ss))
	}
	for _, s := range ss {
		m.releaseHub(s.ID)
	}
	return nil
}

// Close ends all processes (status “interrupted”, resumable) and writes all events.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closing = true
	runs := make([]*run, 0, len(m.runs))
	for _, r := range m.runs {
		runs = append(runs, r)
	}
	m.mu.Unlock()
	for _, r := range runs {
		r.stop(store.SessionInterrupted)
	}
	m.wg.Wait()
	m.cancel()
	m.flushAll()
}

// Subscribe subscribes to the events of a session. The caller first reads the history up to
// the returned seq from the database (Events) and then the channel.
func (m *Manager) Subscribe(ctx context.Context, id string) (*Subscription, int64, error) {
	if _, err := m.opts.Store.GetSession(ctx, id); err != nil {
		return nil, 0, err
	}
	h, err := m.hub(id)
	if err != nil {
		return nil, 0, err
	}
	sub, seq := h.subscribe()
	sub.release = func() { m.releaseHub(id) }
	return sub, seq, nil
}

// Events returns stored events with seq in (since, until]; until <= 0 means no limit.
func (m *Manager) Events(ctx context.Context, id string, since, until int64, limit int) ([]store.Event, error) {
	m.flushHub(id)
	evs, err := m.opts.Store.ListEvents(ctx, id, since, limit)
	if err != nil || until <= 0 {
		return evs, err
	}
	for i, e := range evs {
		if e.Seq > until {
			return evs[:i], nil
		}
	}
	return evs, nil
}

// emit creates an event; serialization errors are logged.
func (m *Manager) emit(id, typ string, payload any) {
	var data []byte
	switch p := payload.(type) {
	case json.RawMessage:
		data = p
	case []byte:
		data = p
	default:
		b, err := json.Marshal(payload)
		if err != nil {
			m.log.Error("serialize event", "session", id, "type", typ, "err", err)
			return
		}
		data = b
	}
	h, err := m.hub(id)
	if err != nil {
		m.log.Error("event hub", "session", id, "err", err)
		return
	}
	h.publish(typ, data)
}

func (m *Manager) hub(id string) (*hub, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h := m.hubs[id]; h != nil {
		return h, nil
	}
	seq, err := m.opts.Store.MaxEventSeq(context.Background(), id)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	h := newHub(m.opts.Store, id, seq, m.opts.FlushInterval, func(err error) {
		m.log.Error("store events", "session", id, "err", err)
	})
	m.hubs[id] = h
	return h, nil
}

// releaseHub removes the hub of a session without a process and subscribers. Pending events
// are written first, so a new hub starts with the right seq.
func (m *Manager) releaseHub(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.hubs[id]
	if h == nil || m.runs[id] != nil {
		return
	}
	h.flush()
	if h.idle() {
		delete(m.hubs, id)
	}
}

func (m *Manager) flushHub(id string) {
	m.mu.Lock()
	h := m.hubs[id]
	m.mu.Unlock()
	if h != nil {
		h.flush()
	}
}

func (m *Manager) flushAll() {
	m.mu.Lock()
	hs := make([]*hub, 0, len(m.hubs))
	for _, h := range m.hubs {
		hs = append(hs, h)
	}
	m.mu.Unlock()
	for _, h := range hs {
		h.flush()
	}
}

// finish sets a session's final status and reports it as an event.
func (m *Manager) finish(id, status, errMsg string, exit *int) {
	u := store.SessionUpdate{Status: &status, Error: &errMsg, ExitCode: exit, PID: store.Ptr(0), Ended: true}
	if err := m.opts.Store.UpdateSession(context.Background(), id, u); err != nil && !errors.Is(err, apperr.ErrNotFound) {
		m.log.Error("store session status", "session", id, "err", err)
	}
	m.emit(id, adapter.TypeStatus, adapter.Status{Status: status, Reason: errMsg, ExitCode: exit})
	m.notify(id, status)
}

func (m *Manager) terminalLog(id string) string {
	return filepath.Join(m.opts.DataDir, "sessions", id+".log")
}

// TerminalLog returns the last terminal output of a session without a running process.
func (m *Manager) TerminalLog(id string) []byte {
	return readTail(m.terminalLog(id), TerminalReplay)
}

// Terminal is a client's connection to a session's PTY.
type Terminal struct {
	// Replay is the output so far (at most TerminalReplay bytes).
	Replay []byte
	// Output returns further output; it is closed when the process ends or the client is too
	// slow. Without a running process it is closed right away.
	Output <-chan []byte
	// Live reports whether a process is running (only then Write and Resize take effect).
	Live bool

	r      *run
	client *termClient
}

// Write sends input to the terminal.
func (t *Terminal) Write(p []byte) error {
	if t.r == nil {
		return errors.New("terminal has ended")
	}
	return t.r.write(p)
}

// Resize changes the terminal size.
func (t *Terminal) Resize(cols, rows uint16) error {
	if t.r == nil {
		return errors.New("terminal has ended")
	}
	if cols == 0 || rows == 0 || cols > 1000 || rows > 1000 {
		return errors.New("invalid terminal size")
	}
	return t.r.resize(cols, rows)
}

// Ended reports whether the process has ended (after a closed Output: end instead of overload).
func (t *Terminal) Ended() bool {
	if t.r == nil {
		return true
	}
	t.r.term.mu.Lock()
	defer t.r.term.mu.Unlock()
	return t.r.term.closed
}

// Close disconnects the client.
func (t *Terminal) Close() {
	if t.r != nil && t.client != nil {
		t.r.term.detach(t.client)
	}
}

// AttachTerminal connects a client to a session's terminal.
func (m *Manager) AttachTerminal(ctx context.Context, id string) (*Terminal, error) {
	sess, err := m.opts.Store.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	a, err := m.opts.Adapters.Get(sess.Adapter)
	if err != nil || !a.Capabilities().Terminal {
		return nil, apperr.Invalid("session %s has no terminal (adapter %s)", id, sess.Adapter)
	}
	m.mu.Lock()
	r := m.runs[id]
	m.mu.Unlock()
	if r != nil {
		r.mu.Lock()
		term := r.term
		r.mu.Unlock()
		if term != nil {
			replay, c := term.attach()
			return &Terminal{Replay: replay, Output: c.ch, Live: true, r: r, client: c}, nil
		}
	}
	closed := make(chan []byte)
	close(closed)
	return &Terminal{Replay: m.TerminalLog(id), Output: closed}, nil
}
