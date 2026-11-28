package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/DavidW475/ade/internal/agent/adapter"
	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/sandbox"
	"github.com/DavidW475/ade/internal/store"
)

// Default size of a new terminal; the browser then reports its real size.
const (
	defaultCols = 120
	defaultRows = 32
	// maxStderr is the part of stderr that appears as an event on an error.
	maxStderr = 8 << 10
)

// run is a session's process – from waiting for the workspace setup to the end.
type run struct {
	m           *Manager
	id          string
	workspaceID string
	caps        adapter.Capabilities
	sandbox     bool // the process runs via docker exec in the workspace's container
	readOnly    bool // read-only role: reader container or snapshot (snapshot.go)
	done        chan struct{}
	cancel      context.CancelFunc // aborts waiting before the start
	// statusMu orders status changes in database and events: if the agent already reports
	// “done” before the start has stored “running”, “running” must not overwrite it.
	statusMu sync.Mutex

	mu         sync.Mutex
	status     string
	stopAs     string // desired final status after stop()
	proc       *os.Process
	stdin      io.WriteCloser
	ptmx       *os.File
	term       *terminal
	pipes      *pipeState
	resumeID   string
	killTimer  *time.Timer
	sessionUSD float64

	// Costs and budget (budget.go)
	projectID    string
	adapter      string
	model        string // model ID for the price table
	metered      bool   // the egress proxy measures the usage
	turnBaseUSD  float64
	base         adapter.Tokens // tokens of the session when the process started
	budgetUSD    float64
	monthlyUSD   float64
	monthlyAllow float64
	pause        *budgetPause // set if a budget ends the run
}

// launch registers the run and starts it in the background.
func (m *Manager) launch(sess *store.Session, a adapter.Adapter, ws *store.Workspace, spec adapter.Spec) error {
	ctx, cancel := context.WithCancel(m.ctx)
	r := &run{
		m: m, id: sess.ID, workspaceID: ws.ID, caps: a.Capabilities(), sandbox: ws.Sandbox, readOnly: spec.ReadOnly, done: make(chan struct{}), cancel: cancel,
		status: store.SessionStarting, resumeID: sess.ResumeID, sessionUSD: sess.CostUSD,
		projectID: ws.ProjectID, adapter: a.Name(), model: spec.Model, turnBaseUSD: sess.CostUSD,
		base:      adapter.Tokens{Input: sess.InputTokens, Output: sess.OutputTokens, CacheRead: sess.CacheReadTokens, CacheWrite: sess.CacheWriteTokens},
		budgetUSD: sess.BudgetUSD, monthlyAllow: sess.MonthlyAllowUSD,
	}
	if p, err := m.opts.Projects.Get(m.ctx, ws.ProjectID); err == nil {
		r.monthlyUSD = p.Config.MonthlyBudget()
	}
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		cancel()
		return apperr.Conflict("the daemon is stopping")
	}
	if m.runs[sess.ID] != nil {
		m.mu.Unlock()
		cancel()
		return apperr.Conflict("session %s is already running", sess.ID)
	}
	m.runs[sess.ID] = r
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		defer cancel()
		status, msg, exit := r.execute(ctx, a, ws, spec)
		if m.opts.Hooks != nil {
			m.opts.Hooks.Release(r.id)
		}
		m.mu.Lock()
		delete(m.runs, r.id)
		m.mu.Unlock()
		if r.readOnly && !r.sandbox && status != store.SessionPaused && status != store.SessionInterrupted {
			// Paused and interrupted can be resumed; the CLI then finds its conversation in the
			// same directory again.
			m.removeSnapshot(r.id)
		}
		m.finish(r.id, status, msg, exit)
		if status == store.SessionPaused {
			m.requestBudget(r)
		}
		// Before done: whoever waits for the end (feedback, deleting) sees the saved state.
		// Read-only roles change nothing.
		if !r.readOnly {
			m.collect(ws.ID, r.id)
		}
		close(r.done)
		m.releaseHub(r.id)
		m.log.Info("session ended", "id", r.id, "status", status, "reason", msg)
	}()
	return nil
}

// execute waits for the workspace setup, starts the process and waits for it to end.
// Returns the final status, the reason and the exit code.
func (r *run) execute(ctx context.Context, a adapter.Adapter, ws *store.Workspace, spec adapter.Spec) (string, string, *int) {
	m := r.m
	if err := m.opts.Workspaces.WaitSetup(ctx, ws.ID); err != nil {
		return r.endStatus(store.SessionStopped), "aborted before the start", nil
	}
	if w, err := m.opts.Workspaces.Get(ctx, ws.ID); err != nil {
		return store.SessionFailed, err.Error(), nil
	} else if w.Status == store.WorkspaceSetupFailed {
		m.emit(r.id, adapter.TypeError, adapter.Error{Message: "workspace setup failed – the agent starts anyway: " + w.Error})
	} else if w.Status != store.WorkspaceReady {
		return store.SessionFailed, fmt.Sprintf("workspace is not ready (status %s)", w.Status), nil
	}

	var extra []string
	switch {
	case r.sandbox && m.opts.SandboxEnv != nil:
		extra = m.opts.SandboxEnv(a.Name())
	case !r.sandbox && m.opts.AdapterEnv != nil:
		extra = m.opts.AdapterEnv(a.Name())
	}
	workdir := ws.Path
	switch {
	case r.sandbox:
		spec.Workdir = sandbox.Workdir
	case r.readOnly:
		snap, err := m.prepareSnapshot(ctx, ws, r.id)
		if err != nil {
			if ctx.Err() != nil {
				// Stopped while the snapshot was being created.
				return r.endStatus(store.SessionStopped), "aborted before the start", nil
			}
			return store.SessionFailed, err.Error(), nil
		}
		workdir, spec.Workdir = snap, snap
	}
	if m.opts.Access != nil {
		access, metered := m.opts.Access(a.Name(), r.sandbox)
		spec.Provider = access
		r.mu.Lock()
		r.metered = metered
		r.mu.Unlock()
		if err := m.opts.Store.UpdateSession(ctx, r.id, store.SessionUpdate{Metered: &metered}); err != nil {
			m.log.Error("store session", "session", r.id, "err", err)
		}
	}
	if reason := r.checkBudget(ctx, false); reason != "" {
		return store.SessionPaused, reason, nil
	}
	if r.caps.Approvals && m.opts.Hooks != nil {
		hook, env := m.opts.Hooks.Prepare(r.id, r.sandbox)
		spec.Hook = hook
		extra = append(extra, env...)
	}
	c, err := a.Command(spec)
	if err != nil {
		return store.SessionFailed, err.Error(), nil
	}
	var cmd *exec.Cmd
	if r.sandbox {
		ensure := m.opts.Workspaces.EnsureSandbox
		if r.readOnly {
			ensure = m.opts.Workspaces.EnsureSandboxReader
		}
		if err := ensure(ctx, ws.ID); err != nil {
			if ctx.Err() != nil {
				return r.endStatus(store.SessionStopped), "aborted before the start", nil
			}
			return store.SessionFailed, fmt.Sprintf("sandbox container: %v", err), nil
		}
		cmd = m.opts.Workspaces.Sandbox().Command(context.Background(), ws.ID, sandbox.Exec{
			Argv:   append([]string{c.Path}, c.Args...),
			Env:    sandboxEnv(r.caps.Terminal, r.id, ws.ID, append(extra, c.Env...)),
			TTY:    r.caps.Terminal,
			Stdin:  true,
			Reader: r.readOnly,
		})
	} else {
		path, err := exec.LookPath(c.Path)
		if err != nil {
			return store.SessionFailed, fmt.Sprintf("program %s not found", c.Path), nil
		}
		cmd = exec.Command(path, c.Args...) //nolint:gosec // G204: the command comes from the adapter or the logged-in user
		cmd.Dir = workdir
		cmd.Env = buildEnv(daemonEnv(), r.caps.Terminal, r.id, ws.ID, workdir, append(extra, c.Env...))
	}

	r.mu.Lock()
	if r.stopAs != "" {
		r.mu.Unlock()
		return r.stopAs, "stopped before the start", nil
	}
	if r.caps.Terminal {
		err = r.startPTY(cmd)
	} else {
		err = r.startPipes(cmd, a)
	}
	if err != nil {
		r.mu.Unlock()
		return store.SessionFailed, fmt.Sprintf("start failed: %v", err), nil
	}
	r.proc = cmd.Process
	r.status = store.SessionRunning
	r.mu.Unlock()

	pid := cmd.Process.Pid
	r.statusMu.Lock()
	r.mu.Lock()
	cur := r.status // a fast agent may already have reported idle
	r.mu.Unlock()
	if err := m.opts.Store.UpdateSession(context.Background(), r.id, store.SessionUpdate{Status: &cur, PID: &pid}); err != nil {
		m.log.Error("store session status", "session", r.id, "err", err)
	}
	if cur == store.SessionRunning {
		m.emit(r.id, adapter.TypeStatus, adapter.Status{Status: store.SessionRunning})
	}
	r.statusMu.Unlock()

	if len(c.Input) > 0 {
		if err := r.write(c.Input); err != nil {
			m.emit(r.id, adapter.TypeError, adapter.Error{Message: "instruction could not be passed on: " + err.Error()})
		}
	}
	if c.CloseStdin && !r.caps.Terminal {
		r.mu.Lock()
		if r.stdin != nil {
			_ = r.stdin.Close()
		}
		r.mu.Unlock()
	}
	if r.caps.Terminal {
		return r.waitPTY(cmd)
	}
	return r.waitPipes(cmd)
}

// --- PTY ---------------------------------------------------------------------------------

func (r *run) startPTY(cmd *exec.Cmd) error {
	if err := os.MkdirAll(filepath.Dir(r.m.terminalLog(r.id)), 0o700); err != nil {
		return err
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: defaultCols, Rows: defaultRows})
	if err != nil {
		return err
	}
	r.ptmx = ptmx
	r.stdin = ptmx
	r.term = newTerminal(r.m.terminalLog(r.id))
	return nil
}

func (r *run) waitPTY(cmd *exec.Cmd) (string, string, *int) {
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(r.term, r.ptmx)
	}()
	waitErr := cmd.Wait()
	// Background processes can keep the PTY open: read the remaining output briefly, then close.
	select {
	case <-copied:
	case <-time.After(500 * time.Millisecond):
	}
	_ = r.ptmx.Close()
	<-copied
	r.term.close()
	return r.exitStatus(cmd, waitErr, "")
}

// --- Pipes (structured adapters) -------------------------------------------------------

type pipeState struct {
	stdout *os.File
	stderr *tailWriter
	parsed chan error
}

func (r *run) startPipes(cmd *exec.Cmd, a adapter.Adapter) error {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	// Own pipe instead of StdoutPipe: Wait then does not wait for background processes that
	// inherited stdout.
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	stderr := &tailWriter{max: maxStderr}
	cmd.Stdout = pw
	cmd.Stderr = stderr
	cmd.WaitDelay = 2 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return err
	}
	_ = pw.Close()
	r.stdin = stdin
	ps := &pipeState{stdout: pr, stderr: stderr, parsed: make(chan error, 1)}
	go func() { ps.parsed <- a.Parse(pr, &sink{r: r}) }()
	r.pipes = ps
	return nil
}

func (r *run) waitPipes(cmd *exec.Cmd) (string, string, *int) {
	ps := r.pipes
	waitErr := cmd.Wait()
	var parseErr error
	select {
	case parseErr = <-ps.parsed:
	case <-time.After(2 * time.Second):
		_ = ps.stdout.Close()
		parseErr = <-ps.parsed
	}
	_ = ps.stdout.Close()
	if parseErr != nil && !errors.Is(parseErr, os.ErrClosed) {
		r.m.emit(r.id, adapter.TypeError, adapter.Error{Message: "output not readable: " + parseErr.Error()})
	}
	return r.exitStatus(cmd, waitErr, ps.stderr.String())
}

// exitStatus determines the final status and reason from the process end.
func (r *run) exitStatus(cmd *exec.Cmd, waitErr error, stderr string) (string, string, *int) {
	r.mu.Lock()
	if r.killTimer != nil {
		r.killTimer.Stop()
	}
	stopAs := r.stopAs
	r.mu.Unlock()

	var exit *int
	if cmd.ProcessState != nil {
		if c := cmd.ProcessState.ExitCode(); c >= 0 {
			exit = &c
		}
	}
	if stopAs != "" {
		reason := "stopped by the user"
		switch stopAs {
		case store.SessionInterrupted:
			reason = "daemon was stopped"
		case store.SessionDone:
			reason = "ended – feedback runs in a new session"
		case store.SessionPaused:
			reason = "budget exceeded"
			r.mu.Lock()
			if r.pause != nil {
				reason = r.pause.reason()
			}
			r.mu.Unlock()
		}
		return stopAs, reason, exit
	}
	if waitErr == nil {
		return store.SessionDone, "", exit
	}
	msg := fmt.Sprintf("process ended: %v", waitErr)
	if s := strings.TrimSpace(stderr); s != "" {
		r.m.emit(r.id, adapter.TypeError, adapter.Error{Message: s})
	}
	return store.SessionFailed, msg, exit
}

// --- Control ----------------------------------------------------------------------------

// endStatus returns the desired final status after stop() or fallback.
func (r *run) endStatus(fallback string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopAs != "" {
		return r.stopAs
	}
	return fallback
}

// stop ends the process: close stdin, SIGTERM to the process group, SIGKILL after
// StopTimeout. Before the start it aborts the waiting.
func (r *run) stop(as string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopAs == "" {
		r.stopAs = as
	}
	r.cancel()
	if r.proc == nil {
		return
	}
	if r.stdin != nil && !r.caps.Terminal {
		_ = r.stdin.Close()
	}
	pid := r.proc.Pid
	if r.sandbox {
		// The Docker CLI does not pass signals to docker exec: end processes in the container
		// by their session ID; the CLI ends with them.
		sb, wsID, marker := r.m.opts.Workspaces.Sandbox(), r.workspaceID, EnvSessionID+"="+r.id
		signal := sb.Signal
		if r.readOnly {
			signal = sb.SignalReader
		}
		go func() { _ = signal(context.Background(), wsID, marker, syscall.SIGTERM) }()
		if r.killTimer == nil {
			r.killTimer = time.AfterFunc(r.m.opts.StopTimeout, func() {
				_ = signal(context.Background(), wsID, marker, syscall.SIGKILL)
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			})
		}
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	if r.killTimer == nil {
		r.killTimer = time.AfterFunc(r.m.opts.StopTimeout, func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	}
}

// send writes a follow-up instruction to stdin.
func (r *run) send(a adapter.Adapter, text string) error {
	data, err := a.EncodeInput(text)
	if err != nil {
		return apperr.Invalid("%v", err)
	}
	r.mu.Lock()
	started := r.proc != nil
	r.mu.Unlock()
	if !started {
		return apperr.Conflict("the agent is still starting – try again in a moment")
	}
	return r.write(data)
}

func (r *run) write(p []byte) error {
	r.mu.Lock()
	w := r.stdin
	r.mu.Unlock()
	if w == nil {
		return errors.New("no input channel")
	}
	_, err := w.Write(p)
	return err
}

func (r *run) resize(cols, rows uint16) error {
	r.mu.Lock()
	ptmx := r.ptmx
	r.mu.Unlock()
	if ptmx == nil {
		return errors.New("no terminal")
	}
	return pty.Setsize(ptmx, &pty.Winsize{Cols: cols, Rows: rows})
}

// setStatus changes the intermediate status (running/idle) of a running process.
func (r *run) setStatus(status, reason string) {
	r.statusMu.Lock()
	r.mu.Lock()
	if r.status == status || r.proc == nil || r.stopAs != "" {
		r.mu.Unlock()
		r.statusMu.Unlock()
		return
	}
	r.status = status
	r.mu.Unlock()
	if err := r.m.opts.Store.UpdateSession(context.Background(), r.id, store.SessionUpdate{Status: &status}); err != nil {
		r.m.log.Error("store session status", "session", r.id, "err", err)
	}
	r.m.emit(r.id, adapter.TypeStatus, adapter.Status{Status: status, Reason: reason})
	r.statusMu.Unlock()
	r.m.notify(r.id, status)
	if status == store.SessionIdle && !r.readOnly {
		// The agent has finished its turn: save the state. The run holds the WaitGroup
		// counter, so Add is allowed here.
		r.m.wg.Add(1)
		go func() {
			defer r.m.wg.Done()
			r.m.collect(r.workspaceID, r.id)
		}()
	}
}

// sink receives the adapter's events.
type sink struct{ r *run }

func (s *sink) Emit(typ string, payload any) {
	r := s.r
	if c, ok := payload.(adapter.Cost); ok {
		payload = r.turnCost(c)
		defer r.checkBudget(context.Background(), true)
	}
	if typ == adapter.TypeStatus {
		// Pass on the CLI's status messages (e.g. the model at start) as information, but
		// change the session status only through the manager.
		if st, ok := payload.(adapter.Status); ok {
			r.mu.Lock()
			st.Status = r.status
			r.mu.Unlock()
			payload = st
		}
	}
	r.m.emit(r.id, typ, payload)
}

func (s *sink) SetResumeID(id string) {
	r := s.r
	r.mu.Lock()
	same := r.resumeID == id
	r.resumeID = id
	r.mu.Unlock()
	if same {
		return
	}
	if err := r.m.opts.Store.UpdateSession(context.Background(), r.id, store.SessionUpdate{ResumeID: &id}); err != nil {
		r.m.log.Error("store resume ID", "session", r.id, "err", err)
	}
}

func (s *sink) TurnDone() { s.r.setStatus(store.SessionIdle, "") }

func (s *sink) Baseline() adapter.Tokens { return s.r.base }

// tailWriter keeps the last max bytes.
type tailWriter struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
