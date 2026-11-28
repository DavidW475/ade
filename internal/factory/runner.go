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

// Limits for artifacts.
const (
	// MaxArtifact is the largest stored answer of a role.
	MaxArtifact = 1 << 20
	// maxArtifactInPrompt is the part of an artifact that goes into the next role's prompt.
	maxArtifactInPrompt = 64 << 10
	// maxArtifactInPR is the part of an artifact in the PR description.
	maxArtifactInPR = 16 << 10
)

// DefaultTick is the interval at which the runner checks all active runs (time limits and
// missed status changes).
const DefaultTick = 30 * time.Second

// Gates creates the approvals of the gates (approval.Service).
type Gates interface {
	RequestGate(ctx context.Context, a *store.Approval, decided func(*store.Approval)) error
	Cancel(ctx context.Context, id, reason string) error
}

// Options configures the runner.
type Options struct {
	Store      *store.Store
	Projects   *project.Service
	Workspaces *workspace.Manager
	Agents     *agent.Manager
	Gates      Gates
	// DataDir takes the artifacts (<DataDir>/runs/<id>/).
	DataDir string
	Logger  *slog.Logger
	// Tick overrides DefaultTick (tests).
	Tick time.Duration
}

// Runner runs runs. Every progress of a run happens under its lock in advance; it is
// driven by status changes of its sessions, decisions at gates, the start, the daemon
// restart and a tick.
type Runner struct {
	opts Options
	log  *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	reopen  map[string]bool // recreate gates after a restart
	closing bool
}

// New creates the runner and registers it with the agent manager for status changes.
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

// Recover continues all active runs after a daemon start: interrupted sessions continue,
// gates get a new approval (the old one expired with the restart). Runs after the Recover
// of agents and approvals.
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
		r.log.Info("resuming runs", "count", len(runs))
	}
	r.wg.Add(1)
	go r.ticker()
	return nil
}

// Close stops the runner. The agent manager then ends running sessions (status
// interrupted); Recover continues them on the next start.
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

// kick runs advance for a run in the background.
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

// --- Start and queries ------------------------------------------------------------------

// StartRequest is the body of POST /api/runs.
type StartRequest struct {
	ProjectID string `json:"project_id"`
	Factory   string `json:"factory"`
	// Task is the short form for inputs.task.
	Task   string            `json:"task,omitempty"`
	Inputs map[string]string `json:"inputs,omitempty"`
	// Sandbox applies to the run's workspace (nil: defaults.sandbox).
	Sandbox *bool `json:"sandbox,omitempty"`
	// Trigger (internal only, not through the API): name, reference and the issue the pull
	// request should close. RequireBudget rejects factories without budget.max_cost_usd.
	Trigger       string `json:"-"`
	TriggerRef    string `json:"-"`
	Closes        int    `json:"-"`
	RequireBudget bool   `json:"-"`
}

// Start loads the factory, creates workspace and run and begins with the first step.
func (r *Runner) Start(ctx context.Context, req StartRequest) (*View, error) {
	if r.isClosing() {
		return nil, apperr.Conflict("the daemon is stopping")
	}
	if req.ProjectID == "" {
		return nil, apperr.Invalid("project_id missing")
	}
	p, err := r.opts.Projects.Get(ctx, req.ProjectID)
	if err != nil {
		return nil, err
	}
	f, src, err := Load(p.RepoPath, strings.TrimSpace(req.Factory))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, apperr.NotFound("factory %q exists neither in %s nor as a template", req.Factory, Dir)
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
			return nil, apperr.Invalid("factory %s has no input task", f.Name)
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
		return nil, apperr.Invalid("factory %s has no budget.max_cost_usd – automatically started runs need one (or allow_unbudgeted: true on the trigger)", f.Name)
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
	r.log.Info("run started", "id", run.ID, "factory", f.Name, "workspace", ws.ID)
	r.kick(run.ID)
	return r.view(ctx, run), nil
}

// checkRoles checks before the start whether every role can be started (adapter, model alias).
func (r *Runner) checkRoles(f *Factory) error {
	for _, name := range f.roleNames() {
		role := f.Roles[name]
		if _, err := r.opts.Agents.Adapters().Get(role.Adapter); err != nil {
			return apperr.Invalid("role %s: %v", name, err)
		}
		if role.Model == "" {
			continue
		}
		if _, prov, alias := r.opts.Agents.Catalog().Resolve(role.Model); alias {
			if want := provider.ForAdapter(role.Adapter); want != "" && prov != want {
				return apperr.Invalid("role %s: model %s belongs to %s, the adapter %s talks to %s", name, role.Model, prov, role.Adapter, want)
			}
		}
	}
	return nil
}

// View is a run with what the UI needs in addition.
type View struct {
	*store.Run
	// Artifacts are the existing artifacts.
	Artifacts []Artifact `json:"artifacts"`
	// PRURL is the workspace's pull request.
	PRURL string `json:"pr_url,omitempty"`
	// WorkspaceGone: the workspace was discarded.
	WorkspaceGone bool `json:"workspace_gone,omitempty"`
	// Plan describes condition and jump back from the definition per position (like Steps).
	Plan []StepPlan `json:"plan"`
}

// StepPlan is a position in the flow from the definition, for the pipeline view.
type StepPlan struct {
	// When is the condition (role, gate, action: otherwise skipped; decision: jump back).
	When string `json:"when,omitempty"`
	// Goto is the target of a jump back (decision: goto, gate: on_deny.goto).
	Goto string `json:"goto,omitempty"`
	// MaxIterations are the allowed passes of the loop (only with Goto).
	MaxIterations int `json:"max_iterations,omitempty"`
	// OnExhausted: fail or continue when a decision has no pass left.
	OnExhausted string `json:"on_exhausted,omitempty"`
}

// plan reads conditions and jumps back from the run's definition.
func plan(run *store.Run) []StepPlan {
	out := make([]StepPlan, len(run.Steps))
	var f Factory
	if json.Unmarshal(run.Definition, &f) != nil {
		return out
	}
	for _, s := range f.Flat() {
		if s.Pos >= len(out) {
			break
		}
		p := StepPlan{When: s.When, Goto: s.Target()}
		if p.Goto != "" {
			p.MaxIterations = s.Iterations()
			if s.Kind() == KindDecision {
				p.OnExhausted = s.OnExhausted
			}
		}
		out[s.Pos] = p
	}
	return out
}

// Artifact is an artifact of a run.
type Artifact struct {
	Name string `json:"name"`
	Step string `json:"step"`
	Role string `json:"role"`
	Size int64  `json:"size"`
	// Iterations are the passes for which a version exists (ascending).
	Iterations []int `json:"iterations,omitempty"`
}

func (r *Runner) view(ctx context.Context, run *store.Run) *View {
	v := &View{Run: run, Artifacts: []Artifact{}, Plan: plan(run)}
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

// Get returns a run.
func (r *Runner) Get(ctx context.Context, id string) (*View, error) {
	run, err := r.opts.Store.GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	return r.view(ctx, run), nil
}

// List returns runs.
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

// Factories returns the factories of a project.
func (r *Runner) Factories(ctx context.Context, projectID string) ([]Summary, error) {
	p, err := r.opts.Projects.Get(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return List(p.RepoPath), nil
}

// ReadArtifact returns the latest version of an artifact.
func (r *Runner) ReadArtifact(ctx context.Context, id, name string) ([]byte, error) {
	return r.ReadArtifactIteration(ctx, id, name, 0)
}

// ReadArtifactIteration returns an artifact from a pass (0: latest version).
func (r *Runner) ReadArtifactIteration(ctx context.Context, id, name string, iteration int) ([]byte, error) {
	run, err := r.opts.Store.GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(run.Steps, func(s store.RunStep) bool { return s.Output == name }) {
		return nil, apperr.NotFound("artifact %s does not exist in run %s", name, id)
	}
	p := r.artifactPath(id, name)
	if iteration > 0 {
		p = r.iterationPath(id, name, iteration)
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, apperr.NotFound("artifact %s has not been produced yet", name)
	}
	return b, err
}

// Cancel cancels a run: the running session is stopped, an open gate withdrawn.
func (r *Runner) Cancel(ctx context.Context, id string) (*View, error) {
	unlock := r.lock(id)
	defer unlock()
	run, err := r.opts.Store.GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	if !store.RunActive(run.Status) {
		return r.view(ctx, run), apperr.Conflict("run %s has already ended (%s)", id, run.Status)
	}
	for _, st := range current(run) {
		switch {
		case st.SessionID != "" && st.Status == store.StepRunning:
			if _, err := r.opts.Agents.Stop(ctx, st.SessionID); err != nil && !errors.Is(err, apperr.ErrNotFound) {
				return nil, err
			}
		case st.ApprovalID != "" && st.Status == store.StepWaiting:
			if err := r.opts.Gates.Cancel(ctx, st.ApprovalID, "run canceled"); err != nil && !errors.Is(err, apperr.ErrNotFound) {
				return nil, err
			}
		}
		if st.Status == store.StepRunning || st.Status == store.StepWaiting {
			r.endRow(ctx, run, st, store.StepCanceled, "canceled")
		}
	}
	r.endRun(ctx, run, store.RunCanceled, "canceled by hand")
	return r.Get(ctx, id)
}

// --- State machine ----------------------------------------------------------------------

// advance advances a run until it has to wait for something.
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
				r.log.Error("read run", "run", id, "err", err)
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
			r.log.Warn("run failed", "run", id, "err", err)
			r.fail(ctx, run, err)
			return
		}
		if !more {
			return
		}
	}
}

// step runs the next transition; more means it continues right away.
func (r *Runner) step(ctx context.Context, run *store.Run) (more bool, err error) {
	if run.Current >= len(run.Steps) {
		r.endRun(ctx, run, store.RunDone, "")
		r.log.Info("run done", "run", run.ID)
		return false, nil
	}
	var f Factory
	if err := json.Unmarshal(run.Definition, &f); err != nil {
		return false, fmt.Errorf("definition of the run not readable: %w", err)
	}
	st := run.Steps[run.Current]
	if st.Group != nil {
		return r.stepGroup(ctx, run, &f)
	}
	def := f.step(st.Name)
	if def == nil {
		return false, fmt.Errorf("step %s missing in the definition", st.Name)
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
				msg := "skipped: " + def.When + " is false"
				if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{Status: store.Ptr(store.StepSkipped), Error: &msg, Started: true, Ended: true}); err != nil {
					return false, err
				}
				r.log.Info("step skipped", "run", run.ID, "step", st.Name)
				return true, nil
			}
		}
		switch st.Kind {
		case KindRole:
			return r.startRole(ctx, run, &f, st, 1)
		case KindGate:
			return false, r.openGate(ctx, run, def, st)
		case KindAction:
			return false, r.deliver(ctx, run, st)
		case KindDecision:
			return r.decide(ctx, run, &f, st, def)
		}
		return false, fmt.Errorf("step %s: unknown kind %q", st.Name, st.Kind)
	}
	switch st.Status {
	case store.StepRunning:
		if st.Kind == KindAction {
			// Interrupted by a restart: try again (an existing PR is only
			// updated).
			return false, r.deliver(ctx, run, st)
		}
		return r.checkRole(ctx, run, &f, st)
	case store.StepWaiting:
		return r.checkGate(ctx, run, def, st)
	}
	return false, fmt.Errorf("step %s has status %s", st.Name, st.Status)
}

func (r *Runner) next(ctx context.Context, run *store.Run) error {
	return r.opts.Store.UpdateRun(ctx, run.ID, store.RunUpdate{Current: store.Ptr(run.Current + 1), Status: store.Ptr(store.RunRunning)})
}

func (r *Runner) setStatus(ctx context.Context, run *store.Run, status string) {
	if run.Status == status {
		return
	}
	if err := r.opts.Store.UpdateRun(ctx, run.ID, store.RunUpdate{Status: &status}); err != nil {
		r.log.Error("store run", "run", run.ID, "err", err)
		return
	}
	run.Status = status
}

// endStep ends the running executions at the current position (for a group all members
// that are still open).
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
		r.log.Error("store step", "run", run.ID, "err", err)
	}
}

// current returns the executions at the current position: the step or all members of its
// parallel group.
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
		r.log.Error("store run", "run", run.ID, "err", err)
	}
	r.mu.Lock()
	delete(r.reopen, run.ID)
	r.mu.Unlock()
}

// stepError is an error of a specific step (member of a group).
type stepError struct {
	step string
	err  error
}

func (e *stepError) Error() string { return e.err.Error() }
func (e *stepError) Unwrap() error { return e.err }

// fail ends the current step (for a group all open members, running sessions are stopped)
// and the run as failed.
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
			status, smsg = store.StepCanceled, "canceled: step "+name+" failed"
			if st.Status == store.StepRunning && st.SessionID != "" {
				if _, err := r.opts.Agents.Stop(ctx, st.SessionID); err != nil && !errors.Is(err, apperr.ErrNotFound) {
					r.log.Warn("stop session", "session", st.SessionID, "err", err)
				}
			}
		}
		r.endRow(ctx, run, st, status, smsg)
	}
	if name != "" {
		msg = "step " + name + ": " + msg
	}
	r.endRun(ctx, run, store.RunFailed, msg)
}

// workTime is the roles' working time so far: the time in which at least one role ran
// (parallel roles count once, waiting at gates does not count).
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

// startRole starts the role of a step. share is the number of roles that share the
// remaining budget (parallel group).
func (r *Runner) startRole(ctx context.Context, run *store.Run, f *Factory, st store.RunStep, share int) (bool, error) {
	role := f.Roles[st.Role]
	if role == nil {
		return false, fmt.Errorf("role %s missing in the definition", st.Role)
	}
	if run.MaxDuration > 0 && workTime(run, time.Now()) >= run.MaxDuration {
		return false, fmt.Errorf("time limit of the run (%s) reached", run.MaxDuration)
	}
	var budget *float64
	if run.BudgetUSD > 0 {
		rest := run.BudgetUSD - run.CostUSD
		if rest <= 0 {
			return false, fmt.Errorf("budget of the run ($%.2f) is used up", run.BudgetUSD)
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
		return false, fmt.Errorf("start role %s: %w", st.Role, err)
	}
	if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{
		Status: store.Ptr(store.StepRunning), SessionID: &sess.ID, Started: true,
	}); err != nil {
		return false, err
	}
	r.setStatus(ctx, run, store.RunRunning)
	r.log.Info("role started", "run", run.ID, "step", st.Name, "role", st.Role, "session", sess.ID)
	// Check right away: a fast agent can already be done before the step knew the session
	// (then sessionChanged did not find the run).
	return true, nil
}

// stepGroup advances a parallel group: start open members (or skip them on a false
// condition), check running ones. When all are done, it continues after the group.
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
			return false, &stepError{m.Name, fmt.Errorf("step %s missing in the definition", m.Name)}
		}
		switch m.Status {
		case store.StepPending:
			if def.When != "" {
				ok, err := r.cond(run, f, def.When)
				if err != nil {
					return false, &stepError{m.Name, err}
				}
				if !ok {
					msg := "skipped: " + def.When + " is false"
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
			return false, &stepError{m.Name, fmt.Errorf("step %s has status %s", m.Name, m.Status)}
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
		return false, errors.New("the session was deleted (workspace discarded?)")
	}
	if err != nil {
		return false, err
	}
	switch v.Status {
	case store.SessionStarting, store.SessionRunning:
		if run.MaxDuration > 0 && workTime(run, time.Now()) >= run.MaxDuration {
			if _, err := r.opts.Agents.Stop(ctx, st.SessionID); err != nil {
				r.log.Warn("stop session", "session", st.SessionID, "err", err)
			}
			return false, fmt.Errorf("time limit of the run (%s) exceeded", run.MaxDuration)
		}
		r.setStatus(ctx, run, store.RunRunning)
		return false, nil
	case store.SessionPaused:
		// The budget approval is created; after “Raise budget” the session continues.
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
			return false, errors.New("the session was interrupted and cannot be resumed")
		}
		if _, err := r.opts.Agents.Input(ctx, st.SessionID, ""); err != nil {
			return false, fmt.Errorf("resume interrupted session: %w", err)
		}
		r.log.Info("role resumed after interruption", "run", run.ID, "session", st.SessionID)
		return false, nil
	}
	msg := "the session ended with " + v.Status
	if v.Error != "" {
		msg += ": " + v.Error
	}
	return false, errors.New(msg)
}

// completeRole saves the answer as an artifact, ends a waiting agent and completes the
// step.
func (r *Runner) completeRole(ctx context.Context, run *store.Run, f *Factory, st store.RunStep, v *agent.View) error {
	if st.Output != "" {
		text, err := r.opts.Agents.LastMessage(ctx, st.SessionID)
		if err != nil {
			return err
		}
		if strings.TrimSpace(text) == "" {
			return fmt.Errorf("role %s delivered no answer for %s", st.Role, st.Output)
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
		// The agent waits for a follow-up instruction: end it. Dismiss waits until the
		// workspace state is saved – the next role sees it.
		if err := r.opts.Agents.Dismiss(ctx, st.SessionID); err != nil {
			return err
		}
	}
	if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{Status: store.Ptr(store.StepDone), Error: store.Ptr(""), Ended: true}); err != nil {
		return err
	}
	r.log.Info("role done", "run", run.ID, "step", st.Name, "role", st.Role)
	return nil
}

// step returns the step name of the definition (nil: does not exist).
func (f *Factory) step(name string) *Step {
	for _, s := range f.Flat() {
		if s.Step.Step == name {
			return s.Step
		}
	}
	return nil
}

// cond evaluates a condition over the latest structured artifacts.
func (r *Runner) cond(run *store.Run, f *Factory, src string) (bool, error) {
	e, err := ParseExpr(src)
	if err != nil {
		return false, fmt.Errorf("condition %q: %w", src, err)
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
			readErr = fmt.Errorf("artifact %s for the condition: %w", out.Name, err)
			return nil, false
		}
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			readErr = fmt.Errorf("artifact %s for the condition is not JSON: %w", out.Name, err)
			return nil, false
		}
		return v, true
	})
	if readErr != nil {
		return false, readErr
	}
	return ok, nil
}

// decide runs a decision: condition false → continue, true → jump back, as long as
// max_iterations is not reached.
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
	msg := fmt.Sprintf("%s is still true after %d passes", def.When, st.Iteration)
	if def.OnExhausted == ExhaustedContinue {
		msg += " – continuing without jumping back"
		if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{Status: store.Ptr(store.StepDone), Error: &msg, Started: true, Ended: true}); err != nil {
			return false, err
		}
		r.log.Info("loop exhausted, continuing", "run", run.ID, "step", st.Name)
		return true, nil
	}
	return false, errors.New(msg)
}

// loop ends the step st as jumped back and creates a new pass from target on.
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
		return fmt.Errorf("jump back to %s: step does not exist before %s", target, st.Name)
	}
	msg := fmt.Sprintf("back to %s (%s)", target, reason)
	if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{Status: store.Ptr(store.StepLooped), Error: &msg, Started: st.StartedAt == nil, Ended: true}); err != nil {
		return err
	}
	if err := r.opts.Store.RepeatSteps(ctx, run.ID, to, st.Index); err != nil {
		return err
	}
	r.log.Info("jump back", "run", run.ID, "from", st.Name, "to", target, "pass", st.Iteration+1)
	return nil
}

// MaxFixes is the number of correction rounds for an invalid structured artifact.
const MaxFixes = 2

// outputOf returns the step's artifact from the definition (nil: none).
func outputOf(f *Factory, st store.RunStep) *Output {
	if role := f.Roles[st.Role]; role != nil {
		if o := role.OutputSpec(); o != nil && o.Name == st.Output {
			return o
		}
	}
	return nil
}

// structured reads the JSON from the answer and checks it against the schema; problem
// describes what is wrong (empty: valid).
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
		return nil, "schema not readable: " + err.Error()
	}
	if errs := s.Validate(v); len(errs) > 0 {
		return nil, "the answer does not match the schema:\n- " + strings.Join(errs, "\n- ")
	}
	return data, ""
}

// requestFix sends the role the check errors as a follow-up instruction; after MaxFixes
// rounds the step fails.
func (r *Runner) requestFix(ctx context.Context, run *store.Run, st store.RunStep, out *Output, problem string) error {
	if st.Fixes >= MaxFixes {
		return fmt.Errorf("%s is still invalid after %d corrections: %s", out.Name, st.Fixes, problem)
	}
	msg := fmt.Sprintf("Your last answer is invalid as artifact `%s`: %s\n\nAnswer again, only with the complete JSON (preferably in a ```json block), without any other text.", out.Name, problem)
	if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{Fixes: store.Ptr(st.Fixes + 1), Error: &problem}); err != nil {
		return err
	}
	if _, err := r.opts.Agents.Input(ctx, st.SessionID, msg); err != nil {
		return fmt.Errorf("request correction of %s: %w", out.Name, err)
	}
	r.log.Info("artifact invalid, role is correcting it", "run", run.ID, "step", st.Name, "artifact", out.Name, "round", st.Fixes+1)
	return errFixRequested
}

// errFixRequested: the role is correcting its artifact; the step keeps running.
var errFixRequested = errors.New("correction requested")

type gateDetail struct {
	RunID     string   `json:"run_id"`
	Factory   string   `json:"factory"`
	Step      string   `json:"step"`
	Task      string   `json:"task,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
	// OnDeny is the target of the jump back on denial (empty: denying cancels the run),
	// Iteration the gate's pass, MaxIterations the allowed passes.
	OnDeny        string `json:"on_deny,omitempty"`
	Iteration     int    `json:"iteration,omitempty"`
	MaxIterations int    `json:"max_iterations,omitempty"`
	// Sensitive are changed files under workspace.SensitivePaths (workflows, .ade/).
	Sensitive []string `json:"sensitive,omitempty"`
	// Unattended: the approval belongs to pull_request without a gate and is only required
	// because of Sensitive (#103).
	Unattended bool `json:"unattended,omitempty"`
}

// sensitive returns the changed sensitive files in the run's workspace.
func (r *Runner) sensitive(ctx context.Context, run *store.Run) ([]string, error) {
	d, err := r.opts.Workspaces.Diff(ctx, run.WorkspaceID, false)
	if err != nil {
		return nil, fmt.Errorf("check changes: %w", err)
	}
	return workspace.Sensitive(d), nil
}

func (r *Runner) openGate(ctx context.Context, run *store.Run, def *Step, st store.RunStep) error {
	d := gateDetail{RunID: run.ID, Factory: run.Factory, Step: st.Name, Task: run.Task, Iteration: st.Iteration}
	title := fmt.Sprintf("Factory %s: approve step %s", run.Factory, st.Name)
	if st.Kind == KindAction {
		// Approval before pull_request without a gate (deliver): Sensitive is already checked.
		d.Unattended = true
		title = fmt.Sprintf("Factory %s: approve pull request with changes to workflows or .ade/", run.Factory)
	} else if t := def.Target(); t != "" && st.Iteration < def.Iterations() {
		d.OnDeny, d.MaxIterations = t, def.Iterations()
	}
	for _, s := range run.Steps[:run.Current] {
		if s.Output != "" && s.Status == store.StepDone {
			d.Artifacts = append(d.Artifacts, s.Output)
		}
	}
	// For display only: without a diff (e.g. container gone) the human decides anyway.
	if paths, err := r.sensitive(ctx, run); err != nil {
		r.log.Warn("gate: changes not checkable", "run", run.ID, "err", err)
	} else {
		d.Sensitive = paths
	}
	detail, _ := json.Marshal(d)
	a := &store.Approval{
		ProjectID: run.ProjectID, WorkspaceID: run.WorkspaceID,
		Title: title, Detail: detail, Sandbox: run.Sandbox,
	}
	runID := run.ID
	if err := r.opts.Gates.RequestGate(ctx, a, func(*store.Approval) { r.kick(runID) }); err != nil {
		return fmt.Errorf("create approval: %w", err)
	}
	if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{
		Status: store.Ptr(store.StepWaiting), ApprovalID: &a.ID, Started: st.StartedAt == nil,
	}); err != nil {
		return err
	}
	r.setStatus(ctx, run, store.RunWaiting)
	r.log.Info("gate waiting for approval", "run", run.ID, "step", st.Name, "approval", a.ID)
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
		if st.Kind == KindAction {
			r.log.Info("pull request approved", "run", run.ID, "step", st.Name)
			return false, r.deliver(ctx, run, st)
		}
		if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{Status: store.Ptr(store.StepDone), Ended: true}); err != nil {
			return false, err
		}
		r.log.Info("gate approved", "run", run.ID, "step", st.Name)
		return true, nil
	case store.ApprovalDenied:
		msg := "denied"
		if a.Reason != "" {
			msg += ": " + a.Reason
		}
		if t := def.Target(); t != "" {
			if st.Iteration < def.Iterations() {
				return true, r.loop(ctx, run, st, t, msg)
			}
			msg += fmt.Sprintf(" (no more jump back after %d passes)", st.Iteration)
		}
		r.endStep(ctx, run, store.StepCanceled, msg)
		r.endRun(ctx, run, store.RunCanceled, "step "+st.Name+" "+msg)
		r.log.Info("gate denied", "run", run.ID, "step", st.Name)
		return false, nil
	}
	// Expired, canceled or gone.
	if r.isClosing() {
		return false, nil
	}
	if reopen || status == "" {
		return false, r.openGate(ctx, run, def, st)
	}
	return false, errors.New("the approval has expired")
}

// deliver creates the pull request. If no human approved at a gate since the last writing
// role (pull_request with unattended: true) and the diff changes workflows or .ade/, the
// step first waits for its own approval (#103).
func (r *Runner) deliver(ctx context.Context, run *store.Run, st store.RunStep) error {
	if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{Status: store.Ptr(store.StepRunning), Started: st.StartedAt == nil}); err != nil {
		return err
	}
	ok, err := r.approvedAction(ctx, st)
	if err != nil {
		return err
	}
	if !ok && !reviewed(run) {
		paths, err := r.sensitive(ctx, run)
		if err != nil {
			return err
		}
		if len(paths) > 0 {
			r.log.Info("pull request without gate changes sensitive paths, approval needed", "run", run.ID, "files", len(paths))
			return r.openGate(ctx, run, nil, st)
		}
	}
	d, err := r.opts.Workspaces.PullRequestDraft(ctx, run.WorkspaceID)
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, workspace.PushTimeout+time.Minute)
	defer cancel()
	w, err := r.opts.Workspaces.PullRequest(pctx, run.WorkspaceID, workspace.PRRequest{Title: d.Title, Body: r.prBody(run, d.Body)})
	if err != nil {
		return fmt.Errorf("pull request: %w", err)
	}
	if err := r.opts.Store.UpdateRunStep(ctx, run.ID, st.Row, store.StepUpdate{Status: store.Ptr(store.StepDone), Ended: true}); err != nil {
		return err
	}
	r.log.Info("pull request of the run created", "run", run.ID, "url", w.PRURL)
	r.endRun(ctx, run, store.RunDone, "")
	return nil
}

// approvedAction reports whether the approval of the action step is granted.
func (r *Runner) approvedAction(ctx context.Context, st store.RunStep) (bool, error) {
	if st.ApprovalID == "" {
		return false, nil
	}
	a, err := r.opts.Store.GetApproval(ctx, st.ApprovalID)
	if errors.Is(err, apperr.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return a.Status == store.ApprovalApproved, nil
}

// reviewed reports whether a gate was approved after the last execution of a role with
// write permission. Executions run in the order of their rows.
func reviewed(run *store.Run) bool {
	ok := false
	for _, s := range run.Executions() {
		switch {
		case s.Kind == KindGate && s.Status == store.StepDone:
			ok = true
		case s.Kind == KindRole && !s.ReadOnly && s.Status != store.StepPending && s.Status != store.StepSkipped:
			ok = false
		}
	}
	return ok
}

// prBody adds factory and artifacts to the PR description.
func (r *Runner) prBody(run *store.Run, draft string) string {
	var b strings.Builder
	if run.Closes > 0 {
		fmt.Fprintf(&b, "Closes #%d\n\n", run.Closes)
	}
	fmt.Fprintf(&b, "## Software factory\n\nFactory `%s` · run `%s`", run.Factory, run.ID)
	if run.Trigger != "" {
		fmt.Fprintf(&b, " · trigger `%s`", run.Trigger)
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

// --- Prompts and artifacts -------------------------------------------------------------

// artifactPath is the latest version of an artifact.
func (r *Runner) artifactPath(runID, name string) string {
	return filepath.Join(r.opts.DataDir, "runs", runID, name)
}

// iterationPath is the version of an artifact from a pass.
func (r *Runner) iterationPath(runID, name string, iteration int) string {
	return filepath.Join(r.opts.DataDir, "runs", runID, "iterations", strconv.Itoa(iteration), name)
}

// writeArtifact stores an artifact as the latest version and for its pass.
func (r *Runner) writeArtifact(runID, name string, iteration int, text string) error {
	if !artifactRE.MatchString(name) {
		return fmt.Errorf("invalid artifact name %q", name)
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

// artifactBlock puts JSON artifacts in a code block, text stays as it is.
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
	for cut > 0 && cut < len(s) && s[cut]&0xC0 == 0x80 { // not in the middle of a UTF-8 character
		cut--
	}
	return s[:cut] + "\n\n[… truncated]"
}

// prompt builds a role's instruction: role prompt, task, further inputs, notes from the
// gates, artifacts and the frame (permissions, output).
func (r *Runner) prompt(ctx context.Context, run *store.Run, f *Factory, st store.RunStep, role *Role) (string, error) {
	var b strings.Builder
	if role.PromptText != "" {
		b.WriteString(role.PromptText)
	} else {
		fmt.Fprintf(&b, "You are the role %s in the software factory %s.", st.Role, f.Name)
	}
	b.WriteString("\n\n")
	if t := run.Inputs["task"]; t != "" {
		b.WriteString("## Task\n\n" + t + "\n\n")
	}
	for _, k := range sortedKeys(run.Inputs) {
		if k == "task" {
			continue
		}
		fmt.Fprintf(&b, "## Input %s\n\n%s\n\n", k, run.Inputs[k])
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
			label += " (denied)"
		}
		if s.Iteration > 1 || s.Status == store.StepLooped {
			label += fmt.Sprintf(", pass %d", s.Iteration)
		}
		notes = append(notes, fmt.Sprintf("- Approval %s: %s", label, strings.ReplaceAll(a.Reason, "\n", "\n  ")))
	}
	if len(notes) > 0 {
		b.WriteString("## Notes from the approvals\n\n" + strings.Join(notes, "\n") + "\n\n")
	}
	if st.Iteration > 1 {
		fmt.Fprintf(&b, "## Pass %d\n\nThis step runs for the %s time.", st.Iteration, ordinal(st.Iteration))
		if lastLoop != nil {
			fmt.Fprintf(&b, " Step %s jumped back: %s.", lastLoop.Name, lastLoop.Error)
		}
		b.WriteString(" The artifacts below are each the latest version; work through the feedback in them.\n\n")
	}
	for _, in := range role.Inputs {
		from := ""
		for _, s := range execs {
			if s.Output == in && s.Status == store.StepDone {
				from = fmt.Sprintf(" (step %s, role %s", s.Name, s.Role)
				if s.Iteration > 1 {
					from += fmt.Sprintf(", pass %d", s.Iteration)
				}
				from += ")"
			}
		}
		if from == "" {
			// Only a later step produces it (feedback after a jump back).
			continue
		}
		data, err := os.ReadFile(r.artifactPath(run.ID, in))
		if err != nil {
			return "", fmt.Errorf("artifact %s missing: %w", in, err)
		}
		fmt.Fprintf(&b, "## Artifact %s%s\n\n%s\n\n", in, from, artifactBlock(in, truncate(string(data), maxArtifactInPrompt)))
	}
	b.WriteString("## Frame\n\n")
	fmt.Fprintf(&b, "- You are one step (%s) in run %s of factory %s; the other roles are separate agents.\n", st.Name, run.ID, f.Name)
	if role.ReadOnly() {
		b.WriteString("- You only have read access to the repository. Do not change any files; changes are not taken over.\n")
	} else {
		b.WriteString("- Work on the current branch. You do not need to commit or push; ADE saves the state.\n")
	}
	if out := role.OutputSpec(); out != nil {
		fmt.Fprintf(&b, "- Your last answer is stored as artifact `%s` and passed on to the following steps. Write it completely in your last message, without asking questions – nobody will answer you.\n", out.Name)
		if out.Structured() {
			b.WriteString("- The last answer must be exactly one JSON value (preferably in a ```json block), with no further text after it. ADE checks it; if it is invalid, you get the errors and answer again.\n")
			if len(out.SchemaDoc) > 0 {
				var pretty bytes.Buffer
				if json.Indent(&pretty, out.SchemaDoc, "", "  ") == nil {
					fmt.Fprintf(&b, "\nJSON Schema for `%s`:\n\n```json\n%s\n```\n", out.Name, pretty.String())
				}
			}
		}
	} else {
		b.WriteString("- Nobody answers your questions; make sensible assumptions and name them at the end.\n")
	}
	return b.String(), nil
}

// ordinal returns n as an English ordinal number (1st, 2nd, 3rd, 4th, …, 11th, 21st).
func ordinal(n int) string {
	suffix := "th"
	switch n % 10 {
	case 1:
		suffix = "st"
	case 2:
		suffix = "nd"
	case 3:
		suffix = "rd"
	}
	if n%100 >= 11 && n%100 <= 13 {
		suffix = "th"
	}
	return fmt.Sprintf("%d%s", n, suffix)
}
