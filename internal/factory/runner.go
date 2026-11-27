package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DavidW475/ade/internal/agent"
	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/provider"
	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/workspace"
)

// Grenzen für Artefakte.
const (
	// MaxArtifact ist die größte gespeicherte Antwort einer Rolle.
	MaxArtifact = 1 << 20
	// maxArtifactInPrompt ist der Teil eines Artefakts, der in den Prompt der nächsten Rolle geht.
	maxArtifactInPrompt = 64 << 10
	// maxArtifactInPR ist der Teil eines Artefakts in der PR-Beschreibung.
	maxArtifactInPR = 16 << 10
)

// DefaultTick ist der Abstand, in dem der Runner alle aktiven Runs prüft (Zeitlimits und
// verpasste Statuswechsel).
const DefaultTick = 30 * time.Second

// Gates legt die Freigaben der Gates an (approval.Service).
type Gates interface {
	RequestGate(ctx context.Context, a *store.Approval, decided func(*store.Approval)) error
	Cancel(ctx context.Context, id, reason string) error
}

// Options konfiguriert den Runner.
type Options struct {
	Store      *store.Store
	Projects   *project.Service
	Workspaces *workspace.Manager
	Agents     *agent.Manager
	Gates      Gates
	// DataDir nimmt die Artefakte auf (<DataDir>/runs/<id>/).
	DataDir string
	Logger  *slog.Logger
	// Tick überschreibt DefaultTick (Tests).
	Tick time.Duration
}

// Runner führt Runs aus. Jeder Fortschritt eines Runs läuft unter seinem Lock in advance;
// Auslöser sind Statuswechsel seiner Sessions, Entscheidungen an Gates, der Start, der
// Neustart des Daemons und ein Takt.
type Runner struct {
	opts Options
	log  *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	reopen  map[string]bool // Gates nach Neustart neu anlegen
	closing bool
}

// New erzeugt den Runner und meldet ihn beim Agent-Manager für Statuswechsel an.
func New(opts Options) *Runner {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Tick <= 0 {
		opts.Tick = DefaultTick
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{opts: opts, log: opts.Logger, ctx: ctx, cancel: cancel, locks: map[string]*sync.Mutex{}, reopen: map[string]bool{}}
	opts.Agents.SetStatusListener(r.sessionChanged)
	return r
}

// Recover setzt nach einem Daemon-Start alle aktiven Runs fort: Unterbrochene Sessions
// laufen weiter, Gates bekommen eine neue Freigabe (die alte ist mit dem Neustart
// abgelaufen). Läuft nach dem Recover von Agenten und Freigaben.
func (r *Runner) Recover(ctx context.Context) error {
	runs, err := r.opts.Store.ListRuns(ctx, store.RunFilter{Active: true, Limit: 1000})
	if err != nil {
		return err
	}
	r.mu.Lock()
	for _, run := range runs {
		r.reopen[run.ID] = true
	}
	r.mu.Unlock()
	for _, run := range runs {
		r.kick(run.ID)
	}
	if len(runs) > 0 {
		r.log.Info("Runs werden fortgesetzt", "anzahl", len(runs))
	}
	r.wg.Add(1)
	go r.ticker()
	return nil
}

// Close hält den Runner an. Laufende Sessions beendet danach der Agent-Manager (Status
// unterbrochen); Recover setzt sie beim nächsten Start fort.
func (r *Runner) Close() {
	r.mu.Lock()
	r.closing = true
	r.mu.Unlock()
	r.cancel()
	r.wg.Wait()
}

func (r *Runner) ticker() {
	defer r.wg.Done()
	t := time.NewTicker(r.opts.Tick)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
			runs, err := r.opts.Store.ListRuns(r.ctx, store.RunFilter{Active: true, Limit: 1000})
			if err != nil {
				continue
			}
			for _, run := range runs {
				r.kick(run.ID)
			}
		}
	}
}

func (r *Runner) sessionChanged(sessionID, _ string) {
	id, err := r.opts.Store.RunOfSession(context.Background(), sessionID)
	if err != nil || id == "" {
		return
	}
	r.kick(id)
}

// kick lässt advance für einen Run im Hintergrund laufen.
func (r *Runner) kick(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.advance(id)
	}()
}

func (r *Runner) lock(id string) func() {
	r.mu.Lock()
	l := r.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		r.locks[id] = l
	}
	r.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (r *Runner) isClosing() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closing
}

// --- Start und Abfragen ----------------------------------------------------------------

// StartRequest ist der Body von POST /api/runs.
type StartRequest struct {
	ProjectID string `json:"project_id"`
	Factory   string `json:"factory"`
	// Task ist die Kurzform für inputs.task.
	Task   string            `json:"task,omitempty"`
	Inputs map[string]string `json:"inputs,omitempty"`
	// Sandbox gilt für den Workspace des Runs (nil: defaults.sandbox).
	Sandbox *bool `json:"sandbox,omitempty"`
	// Auslöser (nur intern, nicht über die API): Name, Verweis und das Issue, das der Pull
	// Request schließen soll. RequireBudget lehnt Fabriken ohne budget.max_cost_usd ab.
	Trigger       string `json:"-"`
	TriggerRef    string `json:"-"`
	Closes        int    `json:"-"`
	RequireBudget bool   `json:"-"`
}

// Start lädt die Fabrik, legt Workspace und Run an und beginnt mit dem ersten Schritt.
func (r *Runner) Start(ctx context.Context, req StartRequest) (*View, error) {
	if r.isClosing() {
		return nil, apperr.Conflict("der Daemon wird gerade beendet")
	}
	if req.ProjectID == "" {
		return nil, apperr.Invalid("project_id fehlt")
	}
	p, err := r.opts.Projects.Get(ctx, req.ProjectID)
	if err != nil {
		return nil, err
	}
	f, src, err := Load(p.RepoPath, strings.TrimSpace(req.Factory))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, apperr.NotFound("Fabrik %q gibt es weder in %s noch als Vorlage", req.Factory, Dir)
	}
	if err != nil {
		return nil, apperr.Invalid("%v", err)
	}
	inputs := map[string]string{}
	for k, v := range req.Inputs {
		inputs[k] = v
	}
	if t := strings.TrimSpace(req.Task); t != "" {
		if _, ok := f.Inputs["task"]; !ok {
			return nil, apperr.Invalid("die Fabrik %s hat keine Eingabe task", f.Name)
		}
		inputs["task"] = t
	}
	if inputs, err = f.CheckInputs(inputs); err != nil {
		return nil, apperr.Invalid("%v", err)
	}
	if err := r.checkRoles(f); err != nil {
		return nil, err
	}
	if req.RequireBudget && (f.Budget == nil || f.Budget.MaxCostUSD <= 0) {
		return nil, apperr.Invalid("die Fabrik %s hat kein budget.max_cost_usd – automatisch gestartete Runs brauchen eins (oder allow_unbudgeted: true am Auslöser)", f.Name)
	}
	def, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	task := inputs["task"]
	if task == "" {
		for _, k := range sortedKeys(inputs) {
			task = inputs[k]
			break
		}
	}

	wsOpts := workspace.CreateOptions{Sandbox: req.Sandbox}
	if b := f.BranchInput(); b != "" {
		wsOpts.BaseBranch = inputs[b]
	}
	ws, err := r.opts.Workspaces.Create(ctx, p.ID, wsOpts)
	if err != nil {
		return nil, err
	}
	run := &store.Run{
		ID: project.NewID(), ProjectID: p.ID, WorkspaceID: ws.ID, Factory: f.Name, Source: src,
		Definition: def, Inputs: inputs, Task: task, Sandbox: ws.Sandbox, Status: store.RunRunning,
		Trigger: req.Trigger, TriggerRef: req.TriggerRef, Closes: req.Closes,
	}
	if f.Budget != nil {
		run.BudgetUSD, run.MaxDuration = f.Budget.MaxCostUSD, f.Budget.MaxDuration
	}
	for _, s := range f.Flat() {
		st := store.RunStep{Name: s.Step.Step, Kind: s.Kind(), Role: s.Role}
		if role := f.Roles[s.Role]; role != nil {
			st.ReadOnly, st.Output = role.ReadOnly(), role.Output()
		}
		if s.Group >= 0 {
			st.Group = store.Ptr(s.Group)
		}
		run.Steps = append(run.Steps, st)
	}
	if err := r.opts.Store.CreateRun(ctx, run); err != nil {
		return nil, err
	}
	r.log.Info("Run gestartet", "id", run.ID, "fabrik", f.Name, "workspace", ws.ID)
	r.kick(run.ID)
	return r.view(ctx, run), nil
}

// checkRoles prüft vor dem Start, ob sich jede Rolle starten lässt (Adapter, Modell-Alias).
func (r *Runner) checkRoles(f *Factory) error {
	for _, name := range f.roleNames() {
		role := f.Roles[name]
		if _, err := r.opts.Agents.Adapters().Get(role.Adapter); err != nil {
			return apperr.Invalid("Rolle %s: %v", name, err)
		}
		if role.Model == "" {
			continue
		}
		if _, prov, alias := r.opts.Agents.Catalog().Resolve(role.Model); alias {
			if want := provider.ForAdapter(role.Adapter); want != "" && prov != want {
				return apperr.Invalid("Rolle %s: Modell %s gehört zu %s, der Adapter %s spricht %s", name, role.Model, prov, role.Adapter, want)
			}
		}
	}
	return nil
}

// View ist ein Run mit dem, was die UI zusätzlich braucht.
type View struct {
	*store.Run
	// Artifacts sind die vorhandenen Artefakte.
	Artifacts []Artifact `json:"artifacts"`
	// PRURL ist der Pull Request des Workspaces.
	PRURL string `json:"pr_url,omitempty"`
	// WorkspaceGone: Der Workspace wurde verworfen.
	WorkspaceGone bool `json:"workspace_gone,omitempty"`
}

// Artifact ist ein Artefakt eines Runs.
type Artifact struct {
	Name string `json:"name"`
	Step string `json:"step"`
	Role string `json:"role"`
	Size int64  `json:"size"`
	// Iterations sind die Durchläufe, aus denen es einen Stand gibt (aufsteigend).
	Iterations []int `json:"iterations,omitempty"`
}

func (r *Runner) view(ctx context.Context, run *store.Run) *View {
	v := &View{Run: run, Artifacts: []Artifact{}}
	for _, st := range run.Steps {
		if st.Output == "" {
			continue
		}
		if fi, err := os.Stat(r.artifactPath(run.ID, st.Output)); err == nil {
			a := Artifact{Name: st.Output, Step: st.Name, Role: st.Role, Size: fi.Size()}
			for _, e := range run.Executions() {
				if e.Output == st.Output && e.Status == store.StepDone {
					a.Iterations = append(a.Iterations, e.Iteration)
				}
			}
			v.Artifacts = append(v.Artifacts, a)
		}
	}
	if run.WorkspaceID != "" {
		w, err := r.opts.Store.GetWorkspace(ctx, run.WorkspaceID)
		switch {
		case err == nil:
			v.PRURL = w.PRURL
		case errors.Is(err, apperr.ErrNotFound):
			v.WorkspaceGone = true
		}
	}
	return v
}

// Get liefert einen Run.
func (r *Runner) Get(ctx context.Context, id string) (*View, error) {
	run, err := r.opts.Store.GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	return r.view(ctx, run), nil
}

// List liefert Runs.
func (r *Runner) List(ctx context.Context, f store.RunFilter) ([]*View, error) {
	runs, err := r.opts.Store.ListRuns(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]*View, len(runs))
	for i, run := range runs {
		out[i] = r.view(ctx, run)
	}
	return out, nil
}

// Factories liefert die Fabriken eines Projekts.
func (r *Runner) Factories(ctx context.Context, projectID string) ([]Summary, error) {
	p, err := r.opts.Projects.Get(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return List(p.RepoPath), nil
}

// ReadArtifact liefert den jüngsten Stand eines Artefakts.
func (r *Runner) ReadArtifact(ctx context.Context, id, name string) ([]byte, error) {
	return r.ReadArtifactIteration(ctx, id, name, 0)
}

// ReadArtifactIteration liefert ein Artefakt aus einem Durchlauf (0: jüngster Stand).
func (r *Runner) ReadArtifactIteration(ctx context.Context, id, name string, iteration int) ([]byte, error) {
	run, err := r.opts.Store.GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(run.Steps, func(s store.RunStep) bool { return s.Output == name }) {
		return nil, apperr.NotFound("Artefakt %s gibt es im Run %s nicht", name, id)
	}
	p := r.artifactPath(id, name)
	if iteration > 0 {
		p = r.iterationPath(id, name, iteration)
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, apperr.NotFound("Artefakt %s ist noch nicht entstanden", name)
	}
	return b, err
}

// Cancel bricht einen Run ab: Die laufende Session wird gestoppt, ein offenes Gate
// zurückgezogen.
func (r *Runner) Cancel(ctx context.Context, id string) (*View, error) {
	unlock := r.lock(id)
	defer unlock()
	run, err := r.opts.Store.GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	if !store.RunActive(run.Status) {
		return r.view(ctx, run), apperr.Conflict("Run %s ist bereits beendet (%s)", id, run.Status)
	}
	for _, st := range current(run) {
		switch {
		case st.SessionID != "" && st.Status == store.StepRunning:
			if _, err := r.opts.Agents.Stop(ctx, st.SessionID); err != nil && !errors.Is(err, apperr.ErrNotFound) {
				return nil, err
			}
		case st.ApprovalID != "" && st.Status == store.StepWaiting:
			if err := r.opts.Gates.Cancel(ctx, st.ApprovalID, "Run abgebrochen"); err != nil && !errors.Is(err, apperr.ErrNotFound) {
				return nil, err
			}
		}
		if st.Status == store.StepRunning || st.Status == store.StepWaiting {
			r.endRow(ctx, run, st, store.StepCanceled, "abgebrochen")
		}
	}
	r.endRun(ctx, run, store.RunCanceled, "von Hand abgebrochen")
	return r.Get(ctx, id)
}

// --- Zustandsmaschine ------------------------------------------------------------------

// advance schreibt einen Run fort, bis er auf etwas warten muss.
func (r *Runner) advance(id string) {
	unlock := r.lock(id)
	defer unlock()
	ctx := r.ctx
	for range 2 * maxSteps {
		if r.isClosing() {
			return
		}
		run, err := r.opts.Store.GetRun(ctx, id)
		if err != nil {
			if !errors.Is(err, apperr.ErrNotFound) && ctx.Err() == nil {
				r.log.Error("Run lesen", "run", id, "err", err)
			}
			return
		}
		if !store.RunActive(run.Status) {
			return
		}
		more, err := r.step(ctx, run)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.log.Warn("Run fehlgeschlagen", "run", id, "err", err)
			r.fail(ctx, run, err)
			return
		}
		if !more {
			return
		}
	}
}

// step führt den nächsten Übergang aus; more heißt, es geht sofort weiter.
func (r *Runner) step(ctx context.Context, run *store.Run) (more bool, err error) {
	if run.Current >= len(run.Steps) {
		r.endRun(ctx, run, store.RunDone, "")
		r.log.Info("Run fertig", "run", run.ID)
		return false, nil
	}
	var f Factory
	if err := json.Unmarshal(run.Definition, &f); err != nil {
		return false, fmt.Errorf("Definition des Runs nicht lesbar: %w", err)
	}
	st := run.Steps[run.Current]
	if st.Group != nil {
		return r.stepGroup(ctx, run, &f)
	}
	def := f.step(st.Name)
	if def == nil {
		return false, fmt.Errorf("Schritt %s fehlt in der Definition", st.Name)
	}
	switch {
	case store.StepFinished(st.Status):
		return true, r.next(ctx, run)
	case st.Status == store.StepPending:
		if def.When != "" && st.Kind != KindDecision {
			ok, err := r.cond(run, &f, def.When)
			if err != nil {
				return false, err
			}
			if !ok {
				msg := "übersprungen: " + def.When + " ist falsch"
				if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{Status: store.Ptr(store.StepSkipped), Error: &msg, Started: true, Ended: true}); err != nil {
					return false, err
				}
				r.log.Info("Schritt übersprungen", "run", run.ID, "schritt", st.Name)
				return true, nil
			}
		}
		switch st.Kind {
		case KindRole:
			return r.startRole(ctx, run, &f, st, 1)
		case KindGate:
			return false, r.openGate(ctx, run, st)
		case KindAction:
			return false, r.deliver(ctx, run, st)
		case KindDecision:
			return r.decide(ctx, run, &f, st, def)
		}
		return false, fmt.Errorf("Schritt %s: unbekannte Art %q", st.Name, st.Kind)
	}
	switch st.Status {
	case store.StepRunning:
		if st.Kind == KindAction {
			// Durch einen Neustart unterbrochen: erneut versuchen (ein bestehender PR wird nur
			// aktualisiert).
			return false, r.deliver(ctx, run, st)
		}
		return r.checkRole(ctx, run, &f, st)
	case store.StepWaiting:
		return r.checkGate(ctx, run, def, st)
	}
	return false, fmt.Errorf("Schritt %s hat den Status %s", st.Name, st.Status)
}

func (r *Runner) next(ctx context.Context, run *store.Run) error {
	return r.opts.Store.UpdateRun(ctx, run.ID, store.RunUpdate{Current: store.Ptr(run.Current + 1), Status: store.Ptr(store.RunRunning)})
}

func (r *Runner) setStatus(ctx context.Context, run *store.Run, status string) {
	if run.Status == status {
		return
	}
	if err := r.opts.Store.UpdateRun(ctx, run.ID, store.RunUpdate{Status: &status}); err != nil {
		r.log.Error("Run speichern", "run", run.ID, "err", err)
		return
	}
	run.Status = status
}

// endStep beendet die laufenden Ausführungen an der aktuellen Stelle (bei einer Gruppe alle
// noch offenen Mitglieder).
func (r *Runner) endStep(ctx context.Context, run *store.Run, status, msg string) {
	for _, st := range current(run) {
		if store.StepFinished(st.Status) || st.Status == store.StepFailed || st.Status == store.StepCanceled {
			continue
		}
		r.endRow(ctx, run, st, status, msg)
	}
}

func (r *Runner) endRow(ctx context.Context, run *store.Run, st store.RunStep, status, msg string) {
	if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{Status: &status, Error: &msg, Ended: true}); err != nil {
		r.log.Error("Schritt speichern", "run", run.ID, "err", err)
	}
}

// current liefert die Ausführungen an der aktuellen Stelle: den Schritt oder alle Mitglieder
// seiner parallelen Gruppe.
func current(run *store.Run) []store.RunStep {
	if run.Current >= len(run.Steps) {
		return nil
	}
	st := run.Steps[run.Current]
	if st.Group == nil {
		return []store.RunStep{st}
	}
	var out []store.RunStep
	for _, s := range run.Steps {
		if s.Group != nil && *s.Group == *st.Group {
			out = append(out, s)
		}
	}
	return out
}

func (r *Runner) endRun(ctx context.Context, run *store.Run, status, msg string) {
	u := store.RunUpdate{Status: &status, Error: &msg, Ended: true}
	if status == store.RunDone {
		u.Current = store.Ptr(len(run.Steps))
	}
	if err := r.opts.Store.UpdateRun(ctx, run.ID, u); err != nil {
		r.log.Error("Run speichern", "run", run.ID, "err", err)
	}
	r.mu.Lock()
	delete(r.reopen, run.ID)
	r.mu.Unlock()
}

// stepError ist ein Fehler eines bestimmten Schritts (Mitglied einer Gruppe).
type stepError struct {
	step string
	err  error
}

func (e *stepError) Error() string { return e.err.Error() }
func (e *stepError) Unwrap() error { return e.err }

// fail beendet den aktuellen Schritt (bei einer Gruppe alle offenen Mitglieder, laufende
// Sessions werden gestoppt) und den Run als fehlgeschlagen.
func (r *Runner) fail(ctx context.Context, run *store.Run, err error) {
	msg := err.Error()
	name := ""
	var se *stepError
	if errors.As(err, &se) {
		name = se.step
	} else if run.Current < len(run.Steps) {
		name = run.Steps[run.Current].Name
	}
	for _, st := range current(run) {
		if store.StepFinished(st.Status) || st.Status == store.StepFailed || st.Status == store.StepCanceled {
			continue
		}
		status, smsg := store.StepFailed, msg
		if st.Name != name {
			status, smsg = store.StepCanceled, "abgebrochen: Schritt "+name+" ist fehlgeschlagen"
			if st.Status == store.StepRunning && st.SessionID != "" {
				if _, err := r.opts.Agents.Stop(ctx, st.SessionID); err != nil && !errors.Is(err, apperr.ErrNotFound) {
					r.log.Warn("Session stoppen", "session", st.SessionID, "err", err)
				}
			}
		}
		r.endRow(ctx, run, st, status, smsg)
	}
	if name != "" {
		msg = "Schritt " + name + ": " + msg
	}
	r.endRun(ctx, run, store.RunFailed, msg)
}

// workTime ist die Arbeitszeit der Rollen bisher: die Zeit, in der mindestens eine Rolle
// lief (parallele Rollen zählen einmal, Warten an Gates nicht).
func workTime(run *store.Run, now time.Time) time.Duration {
	type span struct{ from, to time.Time }
	var spans []span
	for _, st := range run.Executions() {
		if st.Kind != KindRole || st.StartedAt == nil {
			continue
		}
		end := now
		if st.EndedAt != nil {
			end = *st.EndedAt
		}
		spans = append(spans, span{*st.StartedAt, end})
	}
	slices.SortFunc(spans, func(a, b span) int { return a.from.Compare(b.from) })
	var d time.Duration
	var cur *span
	for i := range spans {
		sp := spans[i]
		switch {
		case cur == nil:
			cur = &sp
		case !sp.from.After(cur.to):
			if sp.to.After(cur.to) {
				cur.to = sp.to
			}
		default:
			d += cur.to.Sub(cur.from)
			cur = &sp
		}
	}
	if cur != nil {
		d += cur.to.Sub(cur.from)
	}
	return d
}

// startRole startet die Rolle eines Schritts. share ist die Zahl der Rollen, die sich das
// Rest-Budget teilen (parallele Gruppe).
func (r *Runner) startRole(ctx context.Context, run *store.Run, f *Factory, st store.RunStep, share int) (bool, error) {
	role := f.Roles[st.Role]
	if role == nil {
		return false, fmt.Errorf("Rolle %s fehlt in der Definition", st.Role)
	}
	if run.MaxDuration > 0 && workTime(run, time.Now()) >= run.MaxDuration {
		return false, fmt.Errorf("Zeitlimit des Runs (%s) erreicht", run.MaxDuration)
	}
	var budget *float64
	if run.BudgetUSD > 0 {
		rest := run.BudgetUSD - run.CostUSD
		if rest <= 0 {
			return false, fmt.Errorf("Budget des Runs (%.2f $) ist aufgebraucht", run.BudgetUSD)
		}
		rest /= float64(max(share, 1))
		budget = &rest
	}
	prompt, err := r.prompt(ctx, run, f, st, role)
	if err != nil {
		return false, err
	}
	sess, err := r.opts.Agents.StartRole(ctx, agent.StartRequest{
		WorkspaceID: run.WorkspaceID, Adapter: role.Adapter, Task: run.Task, Model: role.Model,
		PermissionMode: role.PermissionMode, BudgetUSD: budget,
	}, agent.RoleStart{Role: st.Role, Prompt: prompt, ReadOnly: role.ReadOnly()})
	if err != nil {
		return false, fmt.Errorf("Rolle %s starten: %w", st.Role, err)
	}
	if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{
		Status: store.Ptr(store.StepRunning), SessionID: &sess.ID, Started: true,
	}); err != nil {
		return false, err
	}
	r.setStatus(ctx, run, store.RunRunning)
	r.log.Info("Rolle gestartet", "run", run.ID, "schritt", st.Name, "rolle", st.Role, "session", sess.ID)
	// Gleich prüfen: Ein schneller Agent kann schon fertig sein, bevor der Schritt die
	// Session kannte (dann fand sessionChanged den Run nicht).
	return true, nil
}

// stepGroup schreibt eine parallele Gruppe fort: offene Mitglieder starten (oder bei falscher
// Bedingung überspringen), laufende prüfen. Sind alle fertig, geht es hinter der Gruppe weiter.
func (r *Runner) stepGroup(ctx context.Context, run *store.Run, f *Factory) (bool, error) {
	members := current(run)
	open := 0
	for _, m := range members {
		if !store.StepFinished(m.Status) {
			open++
		}
	}
	if open == 0 {
		next := members[len(members)-1].Index + 1
		return true, r.opts.Store.UpdateRun(ctx, run.ID, store.RunUpdate{Current: &next, Status: store.Ptr(store.RunRunning)})
	}
	more, paused := false, false
	for _, m := range members {
		def := f.step(m.Name)
		if def == nil {
			return false, &stepError{m.Name, fmt.Errorf("Schritt %s fehlt in der Definition", m.Name)}
		}
		switch m.Status {
		case store.StepPending:
			if def.When != "" {
				ok, err := r.cond(run, f, def.When)
				if err != nil {
					return false, &stepError{m.Name, err}
				}
				if !ok {
					msg := "übersprungen: " + def.When + " ist falsch"
					if err := r.opts.Store.UpdateRunStep(ctx, run.ID, m.Row, store.StepUpdate{Status: store.Ptr(store.StepSkipped), Error: &msg, Started: true, Ended: true}); err != nil {
						return false, err
					}
					more = true
					continue
				}
			}
			if _, err := r.startRole(ctx, run, f, m, open); err != nil {
				return false, &stepError{m.Name, err}
			}
			more = true
		case store.StepRunning:
			mm, err := r.checkRole(ctx, run, f, m)
			if err != nil {
				return false, &stepError{m.Name, err}
			}
			if s, _ := r.opts.Store.GetSession(ctx, m.SessionID); s != nil && s.Status == store.SessionPaused {
				paused = true
			}
			more = more || mm
		case store.StepDone, store.StepSkipped:
		default:
			return false, &stepError{m.Name, fmt.Errorf("Schritt %s hat den Status %s", m.Name, m.Status)}
		}
	}
	if paused {
		r.setStatus(ctx, run, store.RunPaused)
	} else {
		r.setStatus(ctx, run, store.RunRunning)
	}
	return more, nil
}

func (r *Runner) checkRole(ctx context.Context, run *store.Run, f *Factory, st store.RunStep) (bool, error) {
	v, err := r.opts.Agents.Get(ctx, st.SessionID)
	if errors.Is(err, apperr.ErrNotFound) {
		return false, errors.New("die Session wurde gelöscht (Workspace verworfen?)")
	}
	if err != nil {
		return false, err
	}
	switch v.Status {
	case store.SessionStarting, store.SessionRunning:
		if run.MaxDuration > 0 && workTime(run, time.Now()) >= run.MaxDuration {
			if _, err := r.opts.Agents.Stop(ctx, st.SessionID); err != nil {
				r.log.Warn("Session stoppen", "session", st.SessionID, "err", err)
			}
			return false, fmt.Errorf("Zeitlimit des Runs (%s) überschritten", run.MaxDuration)
		}
		r.setStatus(ctx, run, store.RunRunning)
		return false, nil
	case store.SessionPaused:
		// Budget-Freigabe ist angelegt; nach „Budget erhöhen“ läuft die Session weiter.
		r.setStatus(ctx, run, store.RunPaused)
		return false, nil
	case store.SessionIdle, store.SessionDone:
		err := r.completeRole(ctx, run, f, st, v)
		if errors.Is(err, errFixRequested) {
			r.setStatus(ctx, run, store.RunRunning)
			return false, nil
		}
		return true, err
	case store.SessionInterrupted:
		if r.isClosing() {
			return false, nil
		}
		if !v.Resumable {
			return false, errors.New("die Session wurde unterbrochen und lässt sich nicht fortsetzen")
		}
		if _, err := r.opts.Agents.Input(ctx, st.SessionID, ""); err != nil {
			return false, fmt.Errorf("unterbrochene Session fortsetzen: %w", err)
		}
		r.log.Info("Rolle nach Unterbrechung fortgesetzt", "run", run.ID, "session", st.SessionID)
		return false, nil
	}
	msg := "die Session endete mit " + v.Status
	if v.Error != "" {
		msg += ": " + v.Error
	}
	return false, errors.New(msg)
}

// completeRole sichert die Antwort als Artefakt, beendet einen wartenden Agenten und
// schließt den Schritt ab.
func (r *Runner) completeRole(ctx context.Context, run *store.Run, f *Factory, st store.RunStep, v *agent.View) error {
	if st.Output != "" {
		text, err := r.opts.Agents.LastMessage(ctx, st.SessionID)
		if err != nil {
			return err
		}
		if strings.TrimSpace(text) == "" {
			return fmt.Errorf("die Rolle %s hat keine Antwort für %s geliefert", st.Role, st.Output)
		}
		if out := outputOf(f, st); out.Structured() {
			data, problem := structured(out, text)
			if problem != "" {
				return r.requestFix(ctx, run, st, out, problem)
			}
			text = string(data)
		}
		if err := r.writeArtifact(run.ID, st.Output, st.Iteration, text); err != nil {
			return err
		}
	}
	if v.Live {
		// Der Agent wartet auf eine Folgeanweisung: beenden. Dismiss wartet, bis der Stand
		// des Workspaces gesichert ist – die nächste Rolle sieht ihn.
		if err := r.opts.Agents.Dismiss(ctx, st.SessionID); err != nil {
			return err
		}
	}
	if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{Status: store.Ptr(store.StepDone), Error: store.Ptr(""), Ended: true}); err != nil {
		return err
	}
	r.log.Info("Rolle fertig", "run", run.ID, "schritt", st.Name, "rolle", st.Role)
	return nil
}

// step liefert den Schritt name der Definition (nil: gibt es nicht).
func (f *Factory) step(name string) *Step {
	for _, s := range f.Flat() {
		if s.Step.Step == name {
			return s.Step
		}
	}
	return nil
}

// cond wertet eine Bedingung über die jüngsten strukturierten Artefakte aus.
func (r *Runner) cond(run *store.Run, f *Factory, src string) (bool, error) {
	e, err := ParseExpr(src)
	if err != nil {
		return false, fmt.Errorf("Bedingung %q: %w", src, err)
	}
	var readErr error
	ok := e.Eval(func(step string) (any, bool) {
		def := f.step(step)
		if def == nil || f.Roles[def.Role] == nil {
			return nil, false
		}
		out := f.Roles[def.Role].OutputSpec()
		if !out.Structured() {
			return nil, false
		}
		done := false
		for _, s := range run.Executions() {
			if s.Name == step && s.Status == store.StepDone {
				done = true
			}
		}
		if !done {
			return nil, false
		}
		data, err := os.ReadFile(r.artifactPath(run.ID, out.Name))
		if err != nil {
			readErr = fmt.Errorf("Artefakt %s für die Bedingung: %w", out.Name, err)
			return nil, false
		}
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			readErr = fmt.Errorf("Artefakt %s für die Bedingung ist kein JSON: %w", out.Name, err)
			return nil, false
		}
		return v, true
	})
	if readErr != nil {
		return false, readErr
	}
	return ok, nil
}

// decide führt eine Entscheidung aus: Bedingung falsch → weiter, wahr → Rücksprung, solange
// max_iterations nicht erreicht ist.
func (r *Runner) decide(ctx context.Context, run *store.Run, f *Factory, st store.RunStep, def *Step) (bool, error) {
	yes, err := r.cond(run, f, def.When)
	if err != nil {
		return false, err
	}
	if !yes {
		if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{Status: store.Ptr(store.StepDone), Started: true, Ended: true}); err != nil {
			return false, err
		}
		return true, nil
	}
	if st.Iteration < def.Iterations() {
		return true, r.loop(ctx, run, st, def.Goto, def.When)
	}
	msg := fmt.Sprintf("%s ist nach %d Durchläufen noch wahr", def.When, st.Iteration)
	if def.OnExhausted == ExhaustedContinue {
		msg += " – weiter ohne Rücksprung"
		if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{Status: store.Ptr(store.StepDone), Error: &msg, Started: true, Ended: true}); err != nil {
			return false, err
		}
		r.log.Info("Schleife ausgeschöpft, weiter", "run", run.ID, "schritt", st.Name)
		return true, nil
	}
	return false, errors.New(msg)
}

// loop beendet den Schritt st als zurückgesprungen und legt ab target einen neuen Durchlauf an.
func (r *Runner) loop(ctx context.Context, run *store.Run, st store.RunStep, target, reason string) error {
	to := -1
	for _, s := range run.Steps {
		if s.Name == target {
			to = s.Index
			if s.Group != nil {
				to = *s.Group
			}
		}
	}
	if to < 0 || to > st.Index {
		return fmt.Errorf("Rücksprung nach %s: Schritt gibt es nicht vor %s", target, st.Name)
	}
	msg := fmt.Sprintf("zurück zu %s (%s)", target, reason)
	if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{Status: store.Ptr(store.StepLooped), Error: &msg, Started: st.StartedAt == nil, Ended: true}); err != nil {
		return err
	}
	if err := r.opts.Store.RepeatSteps(ctx, run.ID, to, st.Index); err != nil {
		return err
	}
	r.log.Info("Rücksprung", "run", run.ID, "von", st.Name, "nach", target, "durchlauf", st.Iteration+1)
	return nil
}

// MaxFixes ist die Zahl der Korrektur-Runden für ein ungültiges strukturiertes Artefakt.
const MaxFixes = 2

// outputOf liefert das Artefakt des Schritts aus der Definition (nil: keins).
func outputOf(f *Factory, st store.RunStep) *Output {
	if role := f.Roles[st.Role]; role != nil {
		if o := role.OutputSpec(); o != nil && o.Name == st.Output {
			return o
		}
	}
	return nil
}

// structured liest das JSON aus der Antwort und prüft es gegen das Schema; problem
// beschreibt, was nicht stimmt (leer: gültig).
func structured(out *Output, text string) (data []byte, problem string) {
	data, v, err := ExtractJSON(text)
	if err != nil {
		return nil, err.Error()
	}
	if len(out.SchemaDoc) == 0 {
		return data, ""
	}
	s, err := CompileSchema(out.SchemaDoc)
	if err != nil {
		return nil, "Schema nicht lesbar: " + err.Error()
	}
	if errs := s.Validate(v); len(errs) > 0 {
		return nil, "die Antwort passt nicht zum Schema:\n- " + strings.Join(errs, "\n- ")
	}
	return data, ""
}

// requestFix schickt der Rolle die Prüffehler als Folgeanweisung; nach MaxFixes Runden
// scheitert der Schritt.
func (r *Runner) requestFix(ctx context.Context, run *store.Run, st store.RunStep, out *Output, problem string) error {
	if st.Fixes >= MaxFixes {
		return fmt.Errorf("%s ist nach %d Korrekturen noch ungültig: %s", out.Name, st.Fixes, problem)
	}
	msg := fmt.Sprintf("Deine letzte Antwort ist als Artefakt `%s` ungültig: %s\n\nAntworte noch einmal, nur mit dem vollständigen JSON (gern in einem ```json-Block), ohne weiteren Text.", out.Name, problem)
	if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{Fixes: store.Ptr(st.Fixes + 1), Error: &problem}); err != nil {
		return err
	}
	if _, err := r.opts.Agents.Input(ctx, st.SessionID, msg); err != nil {
		return fmt.Errorf("Korrektur von %s anfordern: %w", out.Name, err)
	}
	r.log.Info("Artefakt ungültig, Rolle korrigiert", "run", run.ID, "schritt", st.Name, "artefakt", out.Name, "runde", st.Fixes+1)
	return errFixRequested
}

// errFixRequested: Die Rolle korrigiert ihr Artefakt; der Schritt läuft weiter.
var errFixRequested = errors.New("Korrektur angefordert")

type gateDetail struct {
	RunID     string   `json:"run_id"`
	Factory   string   `json:"factory"`
	Step      string   `json:"step"`
	Task      string   `json:"task,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
}

func (r *Runner) openGate(ctx context.Context, run *store.Run, st store.RunStep) error {
	d := gateDetail{RunID: run.ID, Factory: run.Factory, Step: st.Name, Task: run.Task}
	for _, s := range run.Steps[:run.Current] {
		if s.Output != "" && s.Status == store.StepDone {
			d.Artifacts = append(d.Artifacts, s.Output)
		}
	}
	detail, _ := json.Marshal(d)
	a := &store.Approval{
		ProjectID: run.ProjectID, WorkspaceID: run.WorkspaceID,
		Title:  fmt.Sprintf("Fabrik %s: Schritt %s freigeben", run.Factory, st.Name),
		Detail: detail, Sandbox: run.Sandbox,
	}
	runID := run.ID
	if err := r.opts.Gates.RequestGate(ctx, a, func(*store.Approval) { r.kick(runID) }); err != nil {
		return fmt.Errorf("Freigabe anlegen: %w", err)
	}
	if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{
		Status: store.Ptr(store.StepWaiting), ApprovalID: &a.ID, Started: st.StartedAt == nil,
	}); err != nil {
		return err
	}
	r.setStatus(ctx, run, store.RunWaiting)
	r.log.Info("Gate wartet auf Freigabe", "run", run.ID, "schritt", st.Name, "freigabe", a.ID)
	return nil
}

func (r *Runner) checkGate(ctx context.Context, run *store.Run, def *Step, st store.RunStep) (bool, error) {
	a, err := r.opts.Store.GetApproval(ctx, st.ApprovalID)
	if err != nil && !errors.Is(err, apperr.ErrNotFound) {
		return false, err
	}
	r.mu.Lock()
	reopen := r.reopen[run.ID]
	delete(r.reopen, run.ID)
	r.mu.Unlock()
	status := ""
	if a != nil {
		status = a.Status
	}
	switch status {
	case store.ApprovalPending:
		r.setStatus(ctx, run, store.RunWaiting)
		return false, nil
	case store.ApprovalApproved:
		if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{Status: store.Ptr(store.StepDone), Ended: true}); err != nil {
			return false, err
		}
		r.log.Info("Gate freigegeben", "run", run.ID, "schritt", st.Name)
		return true, nil
	case store.ApprovalDenied:
		msg := "abgelehnt"
		if a.Reason != "" {
			msg += ": " + a.Reason
		}
		if t := def.Target(); t != "" {
			if st.Iteration < def.Iterations() {
				return true, r.loop(ctx, run, st, t, msg)
			}
			msg += fmt.Sprintf(" (nach %d Durchläufen kein Rücksprung mehr)", st.Iteration)
		}
		r.endStep(ctx, run, store.StepCanceled, msg)
		r.endRun(ctx, run, store.RunCanceled, "Schritt "+st.Name+" "+msg)
		r.log.Info("Gate abgelehnt", "run", run.ID, "schritt", st.Name)
		return false, nil
	}
	// Abgelaufen, abgebrochen oder verschwunden.
	if r.isClosing() {
		return false, nil
	}
	if reopen || status == "" {
		return false, r.openGate(ctx, run, st)
	}
	return false, errors.New("die Freigabe ist abgelaufen")
}

func (r *Runner) deliver(ctx context.Context, run *store.Run, st store.RunStep) error {
	if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{Status: store.Ptr(store.StepRunning), Started: true}); err != nil {
		return err
	}
	d, err := r.opts.Workspaces.PullRequestDraft(ctx, run.WorkspaceID)
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, workspace.PushTimeout+time.Minute)
	defer cancel()
	w, err := r.opts.Workspaces.PullRequest(pctx, run.WorkspaceID, workspace.PRRequest{Title: d.Title, Body: r.prBody(run, d.Body)})
	if err != nil {
		return fmt.Errorf("Pull Request: %w", err)
	}
	if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{Status: store.Ptr(store.StepDone), Ended: true}); err != nil {
		return err
	}
	r.log.Info("Pull Request des Runs erstellt", "run", run.ID, "url", w.PRURL)
	r.endRun(ctx, run, store.RunDone, "")
	return nil
}

// prBody ergänzt die PR-Beschreibung um Fabrik und Artefakte.
func (r *Runner) prBody(run *store.Run, draft string) string {
	var b strings.Builder
	if run.Closes > 0 {
		fmt.Fprintf(&b, "Closes #%d\n\n", run.Closes)
	}
	fmt.Fprintf(&b, "## Software-Fabrik\n\nFabrik `%s` · Run `%s`", run.Factory, run.ID)
	if run.Trigger != "" {
		fmt.Fprintf(&b, " · Auslöser `%s`", run.Trigger)
	}
	b.WriteString("\n\n")
	for _, st := range run.Steps {
		if st.Output == "" {
			continue
		}
		data, err := os.ReadFile(r.artifactPath(run.ID, st.Output))
		if err != nil {
			continue
		}
		text := truncate(string(data), maxArtifactInPR)
		fmt.Fprintf(&b, "<details>\n<summary>%s (%s)</summary>\n\n%s\n\n</details>\n\n", st.Output, st.Role, artifactBlock(st.Output, text))
	}
	body := b.String() + draft
	if len(body) > 60<<10 {
		if run.Closes > 0 {
			return fmt.Sprintf("Closes #%d\n\n%s", run.Closes, draft)
		}
		return draft
	}
	return body
}

// --- Prompts und Artefakte ---------------------------------------------------------------

// artifactPath ist der jüngste Stand eines Artefakts.
func (r *Runner) artifactPath(runID, name string) string {
	return filepath.Join(r.opts.DataDir, "runs", runID, name)
}

// iterationPath ist der Stand eines Artefakts aus einem Durchlauf.
func (r *Runner) iterationPath(runID, name string, iteration int) string {
	return filepath.Join(r.opts.DataDir, "runs", runID, "iterations", strconv.Itoa(iteration), name)
}

// writeArtifact speichert ein Artefakt als jüngsten Stand und für seinen Durchlauf.
func (r *Runner) writeArtifact(runID, name string, iteration int, text string) error {
	if !artifactRE.MatchString(name) {
		return fmt.Errorf("ungültiger Artefakt-Name %q", name)
	}
	data := []byte(truncate(text, MaxArtifact))
	for _, p := range []string{r.iterationPath(runID, name, iteration), r.artifactPath(runID, name)} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// artifactBlock setzt JSON-Artefakte in einen Codeblock, Text bleibt, wie er ist.
func artifactBlock(name, text string) string {
	text = strings.TrimSpace(text)
	if strings.HasSuffix(name, ".json") {
		return "```json\n" + text + "\n```"
	}
	return text
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && cut < len(s) && s[cut]&0xC0 == 0x80 { // nicht mitten in einem UTF-8-Zeichen
		cut--
	}
	return s[:cut] + "\n\n[… gekürzt]"
}

// prompt baut die Anweisung einer Rolle: Rollen-Prompt, Aufgabe, weitere Eingaben,
// Hinweise von den Gates, Artefakte und der Rahmen (Rechte, Ausgabe).
func (r *Runner) prompt(ctx context.Context, run *store.Run, f *Factory, st store.RunStep, role *Role) (string, error) {
	var b strings.Builder
	if role.PromptText != "" {
		b.WriteString(role.PromptText)
	} else {
		fmt.Fprintf(&b, "Du bist die Rolle %s in der Software-Fabrik %s.", st.Role, f.Name)
	}
	b.WriteString("\n\n")
	if t := run.Inputs["task"]; t != "" {
		b.WriteString("## Aufgabe\n\n" + t + "\n\n")
	}
	for _, k := range sortedKeys(run.Inputs) {
		if k == "task" {
			continue
		}
		fmt.Fprintf(&b, "## Eingabe %s\n\n%s\n\n", k, run.Inputs[k])
	}
	var notes []string
	var lastLoop *store.RunStep
	execs := run.Executions()
	for i, s := range execs {
		if s.Status == store.StepLooped {
			lastLoop = &execs[i]
		}
		if s.Kind != KindGate || s.ApprovalID == "" {
			continue
		}
		a, err := r.opts.Store.GetApproval(ctx, s.ApprovalID)
		if err != nil || a.Reason == "" {
			continue
		}
		label := s.Name
		if s.Status == store.StepLooped {
			label += " (abgelehnt)"
		}
		if s.Iteration > 1 || s.Status == store.StepLooped {
			label += fmt.Sprintf(", Durchlauf %d", s.Iteration)
		}
		notes = append(notes, fmt.Sprintf("- Freigabe %s: %s", label, strings.ReplaceAll(a.Reason, "\n", "\n  ")))
	}
	if len(notes) > 0 {
		b.WriteString("## Hinweise aus den Freigaben\n\n" + strings.Join(notes, "\n") + "\n\n")
	}
	if st.Iteration > 1 {
		fmt.Fprintf(&b, "## Durchlauf %d\n\nDieser Schritt läuft zum %d. Mal.", st.Iteration, st.Iteration)
		if lastLoop != nil {
			fmt.Fprintf(&b, " Zurückgesprungen hat der Schritt %s: %s.", lastLoop.Name, lastLoop.Error)
		}
		b.WriteString(" Die Artefakte unten sind jeweils der jüngste Stand; arbeite das Feedback darin ab.\n\n")
	}
	for _, in := range role.Inputs {
		from := ""
		for _, s := range execs {
			if s.Output == in && s.Status == store.StepDone {
				from = fmt.Sprintf(" (Schritt %s, Rolle %s", s.Name, s.Role)
				if s.Iteration > 1 {
					from += fmt.Sprintf(", Durchlauf %d", s.Iteration)
				}
				from += ")"
			}
		}
		if from == "" {
			// Erzeugt erst ein späterer Schritt (Feedback nach einem Rücksprung).
			continue
		}
		data, err := os.ReadFile(r.artifactPath(run.ID, in))
		if err != nil {
			return "", fmt.Errorf("Artefakt %s fehlt: %w", in, err)
		}
		fmt.Fprintf(&b, "## Artefakt %s%s\n\n%s\n\n", in, from, artifactBlock(in, truncate(string(data), maxArtifactInPrompt)))
	}
	b.WriteString("## Rahmen\n\n")
	fmt.Fprintf(&b, "- Du bist ein Schritt (%s) im Run %s der Fabrik %s; die anderen Rollen sind eigene Agenten.\n", st.Name, run.ID, f.Name)
	if role.ReadOnly() {
		b.WriteString("- Du hast nur Lesezugriff auf das Repository. Ändere keine Dateien; Änderungen werden nicht übernommen.\n")
	} else {
		b.WriteString("- Arbeite im aktuellen Branch. Committen und pushen musst du nicht; ADE sichert den Stand.\n")
	}
	if out := role.OutputSpec(); out != nil {
		fmt.Fprintf(&b, "- Deine letzte Antwort wird als Artefakt `%s` gespeichert und an die folgenden Schritte weitergegeben. Schreibe sie vollständig in deine letzte Nachricht, ohne Rückfragen – es antwortet dir niemand.\n", out.Name)
		if out.Structured() {
			b.WriteString("- Die letzte Antwort muss genau ein JSON-Wert sein (gern in einem ```json-Block), ohne weiteren Text danach. ADE prüft sie; ist sie ungültig, bekommst du die Fehler und antwortest noch einmal.\n")
			if len(out.SchemaDoc) > 0 {
				var pretty bytes.Buffer
				if json.Indent(&pretty, out.SchemaDoc, "", "  ") == nil {
					fmt.Fprintf(&b, "\nJSON Schema für `%s`:\n\n```json\n%s\n```\n", out.Name, pretty.String())
				}
			}
		}
	} else {
		b.WriteString("- Es antwortet dir niemand auf Rückfragen; triff sinnvolle Annahmen und nenne sie am Ende.\n")
	}
	return b.String(), nil
}
