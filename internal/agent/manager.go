// Package agent verwaltet den Lebenszyklus von Agent-Sessions: Prozess im Workspace starten
// (mit PTY oder Pipes), Ausgabe über Adapter in Events übersetzen, Events speichern und live
// verteilen, Folgeanweisungen, Stoppen und Fortsetzen nach einem Daemon-Neustart.
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
	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/workspace"
)

// DefaultStopTimeout ist die Zeit zwischen SIGTERM und SIGKILL beim Stoppen.
const DefaultStopTimeout = 5 * time.Second

// ResumePrompt ist die Anweisung, wenn eine Session ohne eigenen Text fortgesetzt wird.
const ResumePrompt = "Mach dort weiter, wo du aufgehört hast."

// Options konfiguriert den Manager.
type Options struct {
	Store      *store.Store
	Projects   *project.Service
	Workspaces *workspace.Manager
	Adapters   *adapter.Registry
	// DataDir nimmt die Terminal-Logs auf (<DataDir>/sessions/<id>.log).
	DataDir string
	Logger  *slog.Logger
	// DefaultAdapter gilt, wenn weder die Anfrage noch project.yaml einen nennen.
	DefaultAdapter string
	// AdapterEnv liefert zusätzliche Umgebungsvariablen für einen Adapter ohne Sandbox
	// (z. B. API-Keys).
	AdapterEnv func(adapter string) []string
	// SandboxEnv liefert sie für einen Adapter in der Sandbox: nie echte Keys, sondern
	// Basis-URLs auf den Egress-Proxy, der die Keys einfügt (doc/sandbox.md#4-secrets).
	SandboxEnv func(adapter string) []string
	// Hooks liefert den Freigabe-Hook für Adapter mit Capabilities.Approvals (optional).
	Hooks Hooks
	// FlushInterval und StopTimeout überschreiben die Standardwerte (Tests).
	FlushInterval time.Duration
	StopTimeout   time.Duration
}

// Hooks stellt den Freigabe-Hook einer Session bereit (doc/architecture.md#43-freigaben).
type Hooks interface {
	// Prepare liefert Hook und zusätzliche Umgebung für eine Session (nil: kein Hook).
	Prepare(sessionID string, sandbox bool) (*adapter.Hook, []string)
	// Release gibt die Zugänge einer beendeten Session frei.
	Release(sessionID string)
}

// Manager startet und überwacht Agent-Sessions.
type Manager struct {
	opts Options
	log  *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	runs    map[string]*run // Sessions mit (wartendem oder laufendem) Prozess
	hubs    map[string]*hub
	closing bool
}

// NewManager erzeugt einen Manager.
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
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		opts: opts, log: opts.Logger, ctx: ctx, cancel: cancel,
		runs: map[string]*run{}, hubs: map[string]*hub{},
	}
}

// Adapters liefert die Registry.
func (m *Manager) Adapters() *adapter.Registry { return m.opts.Adapters }

// StartRequest ist der Body von POST /api/agents.
type StartRequest struct {
	ProjectID string `json:"project_id"`
	// WorkspaceID ist optional; ohne sie legt ADE einen neuen Workspace an.
	WorkspaceID string `json:"workspace_id,omitempty"`
	// Adapter ist optional (Standard: project.yaml, dann globale Konfiguration).
	Adapter string `json:"adapter,omitempty"`
	Task    string `json:"task,omitempty"`
	// Command ist der Befehl für Terminal-Adapter (Standard: Login-Shell).
	Command        []string `json:"command,omitempty"`
	Model          string   `json:"model,omitempty"`
	PermissionMode string   `json:"permission_mode,omitempty"`
	// Sandbox gilt für einen neuen Workspace (nil: defaults.sandbox). Bei einem bestehenden
	// Workspace muss sie, falls gesetzt, zu ihm passen.
	Sandbox *bool `json:"sandbox,omitempty"`
}

// Start legt eine Session an und startet den Prozess im Hintergrund, sobald das Setup des
// Workspaces fertig ist.
func (m *Manager) Start(ctx context.Context, req StartRequest) (*store.Session, error) {
	return m.start(ctx, req, startExtra{})
}

// startExtra sind interne Startoptionen (z. B. für Feedback-Sessions).
type startExtra struct {
	Role string
	// Prompt ersetzt die Aufgabe als Anweisung an den Agenten; Task bleibt die Kurzfassung
	// für Liste und Verlauf.
	Prompt string
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
		return nil, apperr.Invalid("Aufgabe fehlt")
	}
	if len(req.Command) > 0 && !caps.Terminal {
		return nil, apperr.Invalid("ein Befehl ist nur beim pty-Adapter möglich")
	}
	for _, c := range req.Command {
		if strings.ContainsRune(c, 0) {
			return nil, apperr.Invalid("ungültiges Zeichen im Befehl")
		}
	}
	if len(req.Task) > 100<<10 {
		return nil, apperr.Invalid("Aufgabe ist zu lang (höchstens 100 KiB)")
	}
	sandboxed := m.opts.Workspaces.SandboxDefault()
	if req.Sandbox != nil {
		sandboxed = *req.Sandbox
	}
	if ws != nil {
		if req.Sandbox != nil && *req.Sandbox != ws.Sandbox {
			if ws.Sandbox {
				return nil, apperr.Invalid("Workspace %s läuft in der Sandbox – für einen Agenten ohne Sandbox einen neuen Workspace anlegen", ws.ID)
			}
			return nil, apperr.Invalid("Workspace %s läuft ohne Sandbox – für die Sandbox einen neuen Workspace anlegen", ws.ID)
		}
		sandboxed = ws.Sandbox
	}
	// Vorab prüfen, ob sich der Aufruf bauen lässt (z. B. CLI installiert, Optionen gültig).
	if _, err := a.Command(adapter.Spec{Workdir: "/", Task: req.Task, Command: req.Command, Model: req.Model, PermissionMode: req.PermissionMode, Sandbox: sandboxed}); err != nil {
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
		if other := m.activeAgentIn(ws.ID); other != "" {
			return nil, apperr.Conflict("im Workspace %s läuft bereits der Agent %s – erst stoppen oder einen neuen Workspace verwenden", ws.ID, other)
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
	}
	if err := m.opts.Store.CreateSession(ctx, sess); err != nil {
		return nil, err
	}
	m.emit(sess.ID, adapter.TypeStatus, adapter.Status{Status: store.SessionStarting, Reason: "Session angelegt"})
	if req.Task != "" {
		m.emit(sess.ID, adapter.TypeMessage, adapter.Message{Role: "user", Text: req.Task})
	}
	prompt := req.Task
	if extra.Prompt != "" {
		prompt = extra.Prompt
	}
	spec := adapter.Spec{SessionID: sess.ID, Workdir: ws.Path, Task: prompt, Command: req.Command, Model: req.Model, PermissionMode: req.PermissionMode, Sandbox: ws.Sandbox}
	if err := m.launch(sess, a, ws, spec); err != nil {
		return nil, err
	}
	m.log.Info("Session gestartet", "id", sess.ID, "workspace", ws.ID, "adapter", name)
	return m.opts.Store.GetSession(ctx, sess.ID)
}

func (m *Manager) resolveWorkspace(ctx context.Context, projectID, workspaceID string) (*store.Workspace, *project.View, error) {
	var ws *store.Workspace
	if workspaceID != "" {
		w, err := m.opts.Workspaces.Get(ctx, workspaceID)
		if err != nil {
			return nil, nil, err
		}
		if projectID != "" && projectID != w.ProjectID {
			return nil, nil, apperr.Invalid("Workspace %s gehört nicht zu Projekt %s", workspaceID, projectID)
		}
		switch w.Status {
		case store.WorkspaceCreating, store.WorkspaceError:
			return nil, nil, apperr.Conflict("Workspace %s ist nicht bereit (Status %s)", w.ID, w.Status)
		}
		ws, projectID = w, w.ProjectID
	}
	if projectID == "" {
		return nil, nil, apperr.Invalid("project_id oder workspace_id fehlt")
	}
	p, err := m.opts.Projects.Get(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	return ws, p, nil
}

// activeAgentIn liefert die ID einer laufenden Nicht-Terminal-Session im Workspace.
func (m *Manager) activeAgentIn(workspaceID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.runs {
		if r.workspaceID == workspaceID && !r.caps.Terminal {
			return id
		}
	}
	return ""
}

// Get liefert eine Session.
func (m *Manager) Get(ctx context.Context, id string) (*View, error) {
	s, err := m.opts.Store.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	return m.view(s), nil
}

// List liefert Sessions.
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

// View ist eine Session mit dem, was die UI zusätzlich braucht.
type View struct {
	*store.Session
	Capabilities adapter.Capabilities `json:"capabilities"`
	// Live: Zur Session gehört gerade ein Prozess dieses Daemons.
	Live bool `json:"live"`
	// Resumable: Die Session lässt sich mit einer Anweisung fortsetzen.
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

// Input schickt eine Folgeanweisung an eine laufende Session oder setzt eine beendete
// Session fort, wenn der Adapter das kann.
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
			return nil, apperr.Invalid("Adapter %s nimmt keine Folgeanweisungen an", sess.Adapter)
		}
		if text == "" {
			return nil, apperr.Invalid("leere Anweisung")
		}
		// Status und Nachricht vor dem Senden: Ein schneller Agent kann seine Runde sonst
		// beenden (idle), bevor „running“ gesetzt ist, und bliebe dann scheinbar beschäftigt.
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
		return nil, apperr.Conflict("Session %s läuft nicht und lässt sich nicht fortsetzen", id)
	}
	if store.SessionActive(sess.Status) {
		return nil, apperr.Conflict("Session %s hat den Status %s, aber keinen Prozess", id, sess.Status)
	}
	ws, err := m.opts.Workspaces.Get(ctx, sess.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if other := m.activeAgentIn(ws.ID); other != "" && !caps.Terminal {
		return nil, apperr.Conflict("im Workspace %s läuft bereits der Agent %s", ws.ID, other)
	}
	if text == "" {
		text = ResumePrompt
	}
	if err := m.opts.Store.UpdateSession(ctx, id, store.SessionUpdate{
		Status: store.Ptr(store.SessionStarting), Error: store.Ptr(""), ClearEnded: true, ClearExitCode: true,
	}); err != nil {
		return nil, err
	}
	m.emit(id, adapter.TypeStatus, adapter.Status{Status: store.SessionStarting, Reason: "wird fortgesetzt"})
	m.emit(id, adapter.TypeMessage, adapter.Message{Role: "user", Text: text})
	spec := adapter.Spec{SessionID: id, Workdir: ws.Path, Task: text, Model: sess.Model, PermissionMode: sess.PermissionMode, ResumeID: sess.ResumeID, Sandbox: ws.Sandbox}
	if err := m.launch(sess, a, ws, spec); err != nil {
		return nil, err
	}
	m.log.Info("Session fortgesetzt", "id", id, "resume_id", sess.ResumeID)
	return m.Get(ctx, id)
}

// Stop beendet den Prozess einer Session und wartet kurz auf sein Ende.
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
			// Kein Prozess, aber aktiver Status (sollte nicht vorkommen): Status bereinigen.
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

// BeforeWorkspaceDelete stoppt alle Sessions eines Workspaces und löscht ihre Terminal-Logs.
// Die Datensätze entfernt die Datenbank zusammen mit dem Workspace.
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
	}
	return nil
}

// Recover räumt nach einem Neustart oder Absturz des Daemons auf: Verwaiste Prozesse
// früherer Sessions werden beendet, aktive Sessions als unterbrochen markiert.
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
		m.log.Warn("verwaiste Agent-Prozesse beendet", "anzahl", len(killed))
	}
	for _, s := range ss {
		if err := m.opts.Store.UpdateSession(ctx, s.ID, store.SessionUpdate{
			Status: store.Ptr(store.SessionInterrupted), Error: store.Ptr("durch Neustart des Daemons unterbrochen"),
			PID: store.Ptr(0), Ended: true,
		}); err != nil {
			return err
		}
		m.emit(s.ID, adapter.TypeStatus, adapter.Status{Status: store.SessionInterrupted, Reason: "Daemon wurde neu gestartet"})
	}
	if len(ss) > 0 {
		m.log.Warn("unterbrochene Sessions markiert", "anzahl", len(ss))
	}
	for _, s := range ss {
		m.releaseHub(s.ID)
	}
	return nil
}

// Close beendet alle Prozesse (Status „unterbrochen“, fortsetzbar) und schreibt alle Events.
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

// Subscribe abonniert die Events einer Session. Der Aufrufer liest zuerst die Historie bis
// zur gelieferten seq aus der Datenbank (Events) und danach den Kanal.
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

// Events liefert gespeicherte Events mit seq in (since, until]; until <= 0 heißt ohne Grenze.
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

// emit erzeugt ein Event; Fehler beim Serialisieren werden geloggt.
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
			m.log.Error("Event serialisieren", "session", id, "typ", typ, "err", err)
			return
		}
		data = b
	}
	h, err := m.hub(id)
	if err != nil {
		m.log.Error("Event-Hub", "session", id, "err", err)
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
		return nil, fmt.Errorf("Events lesen: %w", err)
	}
	h := newHub(m.opts.Store, id, seq, m.opts.FlushInterval, func(err error) {
		m.log.Error("Events speichern", "session", id, "err", err)
	})
	m.hubs[id] = h
	return h, nil
}

// releaseHub entfernt den hub einer Session ohne Prozess und Abonnenten. Ausstehende Events
// werden vorher geschrieben, damit ein neuer hub mit der richtigen seq beginnt.
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

// finish setzt den Endstatus einer Session und meldet ihn als Event.
func (m *Manager) finish(id, status, errMsg string, exit *int) {
	u := store.SessionUpdate{Status: &status, Error: &errMsg, ExitCode: exit, PID: store.Ptr(0), Ended: true}
	if err := m.opts.Store.UpdateSession(context.Background(), id, u); err != nil && !errors.Is(err, apperr.ErrNotFound) {
		m.log.Error("Session-Status speichern", "session", id, "err", err)
	}
	m.emit(id, adapter.TypeStatus, adapter.Status{Status: status, Reason: errMsg, ExitCode: exit})
}

func (m *Manager) terminalLog(id string) string {
	return filepath.Join(m.opts.DataDir, "sessions", id+".log")
}

// TerminalLog liefert die letzte Terminal-Ausgabe einer Session ohne laufenden Prozess.
func (m *Manager) TerminalLog(id string) []byte {
	return readTail(m.terminalLog(id), TerminalReplay)
}

// Terminal ist die Verbindung eines Clients zum PTY einer Session.
type Terminal struct {
	// Replay ist die bisherige Ausgabe (höchstens TerminalReplay Bytes).
	Replay []byte
	// Output liefert weitere Ausgabe; er wird geschlossen, wenn der Prozess endet oder der
	// Client zu langsam ist. Ohne laufenden Prozess ist er sofort geschlossen.
	Output <-chan []byte
	// Live meldet, ob ein Prozess läuft (nur dann wirken Write und Resize).
	Live bool

	r      *run
	client *termClient
}

// Write schickt Eingaben an das Terminal.
func (t *Terminal) Write(p []byte) error {
	if t.r == nil {
		return errors.New("Terminal ist beendet")
	}
	return t.r.write(p)
}

// Resize ändert die Terminalgröße.
func (t *Terminal) Resize(cols, rows uint16) error {
	if t.r == nil {
		return errors.New("Terminal ist beendet")
	}
	if cols == 0 || rows == 0 || cols > 1000 || rows > 1000 {
		return errors.New("ungültige Terminalgröße")
	}
	return t.r.resize(cols, rows)
}

// Ended meldet, ob der Prozess beendet ist (nach geschlossenem Output: Ende statt Überlast).
func (t *Terminal) Ended() bool {
	if t.r == nil {
		return true
	}
	t.r.term.mu.Lock()
	defer t.r.term.mu.Unlock()
	return t.r.term.closed
}

// Close trennt den Client.
func (t *Terminal) Close() {
	if t.r != nil && t.client != nil {
		t.r.term.detach(t.client)
	}
}

// AttachTerminal verbindet einen Client mit dem Terminal einer Session.
func (m *Manager) AttachTerminal(ctx context.Context, id string) (*Terminal, error) {
	sess, err := m.opts.Store.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	a, err := m.opts.Adapters.Get(sess.Adapter)
	if err != nil || !a.Capabilities().Terminal {
		return nil, apperr.Invalid("Session %s hat kein Terminal (Adapter %s)", id, sess.Adapter)
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
