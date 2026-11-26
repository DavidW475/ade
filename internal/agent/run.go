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

// Standardgröße eines neuen Terminals; der Browser meldet danach seine echte Größe.
const (
	defaultCols = 120
	defaultRows = 32
	// maxStderr ist der Teil von stderr, der bei einem Fehler als Event erscheint.
	maxStderr = 8 << 10
)

// run ist der Prozess einer Session – vom Warten auf das Workspace-Setup bis zum Ende.
type run struct {
	m           *Manager
	id          string
	workspaceID string
	caps        adapter.Capabilities
	sandbox     bool // Prozess läuft per docker exec im Container des Workspaces
	done        chan struct{}
	cancel      context.CancelFunc // bricht das Warten vor dem Start ab

	mu         sync.Mutex
	status     string
	stopAs     string // gewünschter Endstatus nach stop()
	proc       *os.Process
	stdin      io.WriteCloser
	ptmx       *os.File
	term       *terminal
	pipes      *pipeState
	resumeID   string
	killTimer  *time.Timer
	sessionUSD float64
}

// launch registriert den Lauf und startet ihn im Hintergrund.
func (m *Manager) launch(sess *store.Session, a adapter.Adapter, ws *store.Workspace, spec adapter.Spec) error {
	ctx, cancel := context.WithCancel(m.ctx)
	r := &run{
		m: m, id: sess.ID, workspaceID: ws.ID, caps: a.Capabilities(), sandbox: ws.Sandbox, done: make(chan struct{}), cancel: cancel,
		status: store.SessionStarting, resumeID: sess.ResumeID, sessionUSD: sess.CostUSD,
	}
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		cancel()
		return apperr.Conflict("der Daemon wird gerade beendet")
	}
	if m.runs[sess.ID] != nil {
		m.mu.Unlock()
		cancel()
		return apperr.Conflict("Session %s läuft bereits", sess.ID)
	}
	m.runs[sess.ID] = r
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		defer cancel()
		status, msg, exit := r.execute(ctx, a, ws, spec)
		m.mu.Lock()
		delete(m.runs, r.id)
		m.mu.Unlock()
		m.finish(r.id, status, msg, exit)
		// Vor done: Wer auf das Ende wartet (Feedback, Löschen), sieht den gesicherten Stand.
		m.collect(ws.ID, r.id)
		close(r.done)
		m.releaseHub(r.id)
		m.log.Info("Session beendet", "id", r.id, "status", status, "grund", msg)
	}()
	return nil
}

// execute wartet auf das Workspace-Setup, startet den Prozess und wartet auf sein Ende.
// Liefert Endstatus, Grund und Exit-Code.
func (r *run) execute(ctx context.Context, a adapter.Adapter, ws *store.Workspace, spec adapter.Spec) (string, string, *int) {
	m := r.m
	if err := m.opts.Workspaces.WaitSetup(ctx, ws.ID); err != nil {
		return r.endStatus(store.SessionStopped), "vor dem Start abgebrochen", nil
	}
	if w, err := m.opts.Workspaces.Get(ctx, ws.ID); err != nil {
		return store.SessionFailed, err.Error(), nil
	} else if w.Status == store.WorkspaceSetupFailed {
		m.emit(r.id, adapter.TypeError, adapter.Error{Message: "Setup des Workspaces fehlgeschlagen – der Agent startet trotzdem: " + w.Error})
	} else if w.Status != store.WorkspaceReady {
		return store.SessionFailed, fmt.Sprintf("Workspace ist nicht bereit (Status %s)", w.Status), nil
	}

	var extra []string
	if m.opts.AdapterEnv != nil {
		extra = m.opts.AdapterEnv(a.Name())
	}
	if r.sandbox {
		spec.Workdir = sandbox.Workdir
	}
	c, err := a.Command(spec)
	if err != nil {
		return store.SessionFailed, err.Error(), nil
	}
	var cmd *exec.Cmd
	if r.sandbox {
		if err := m.opts.Workspaces.EnsureSandbox(ctx, ws.ID); err != nil {
			if ctx.Err() != nil {
				return r.endStatus(store.SessionStopped), "vor dem Start abgebrochen", nil
			}
			return store.SessionFailed, fmt.Sprintf("Sandbox-Container: %v", err), nil
		}
		cmd = m.opts.Workspaces.Sandbox().Command(context.Background(), ws.ID, sandbox.Exec{
			Argv:  append([]string{c.Path}, c.Args...),
			Env:   sandboxEnv(r.caps.Terminal, r.id, ws.ID, append(extra, c.Env...)),
			TTY:   r.caps.Terminal,
			Stdin: true,
		})
	} else {
		path, err := exec.LookPath(c.Path)
		if err != nil {
			return store.SessionFailed, fmt.Sprintf("Programm %s nicht gefunden", c.Path), nil
		}
		cmd = exec.Command(path, c.Args...) //nolint:gosec // G204: Befehl kommt vom Adapter bzw. vom angemeldeten Nutzer
		cmd.Dir = ws.Path
		cmd.Env = buildEnv(daemonEnv(), r.caps.Terminal, r.id, ws.ID, ws.Path, append(extra, c.Env...))
	}

	r.mu.Lock()
	if r.stopAs != "" {
		r.mu.Unlock()
		return r.stopAs, "vor dem Start gestoppt", nil
	}
	if r.caps.Terminal {
		err = r.startPTY(cmd)
	} else {
		err = r.startPipes(cmd, a)
	}
	if err != nil {
		r.mu.Unlock()
		return store.SessionFailed, fmt.Sprintf("Start fehlgeschlagen: %v", err), nil
	}
	r.proc = cmd.Process
	r.status = store.SessionRunning
	r.mu.Unlock()

	pid := cmd.Process.Pid
	if err := m.opts.Store.UpdateSession(context.Background(), r.id, store.SessionUpdate{Status: store.Ptr(store.SessionRunning), PID: &pid}); err != nil {
		m.log.Error("Session-Status speichern", "session", r.id, "err", err)
	}
	m.emit(r.id, adapter.TypeStatus, adapter.Status{Status: store.SessionRunning})

	if len(c.Input) > 0 {
		if err := r.write(c.Input); err != nil {
			m.emit(r.id, adapter.TypeError, adapter.Error{Message: "Anweisung konnte nicht übergeben werden: " + err.Error()})
		}
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
	// Hintergrundprozesse können das PTY offen halten: kurz Restausgabe lesen, dann schließen.
	select {
	case <-copied:
	case <-time.After(500 * time.Millisecond):
	}
	_ = r.ptmx.Close()
	<-copied
	r.term.close()
	return r.exitStatus(cmd, waitErr, "")
}

// --- Pipes (strukturierte Adapter) ------------------------------------------------------

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
	// Eigene Pipe statt StdoutPipe: Wait wartet dann nicht auf Hintergrundprozesse, die
	// stdout geerbt haben.
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
		r.m.emit(r.id, adapter.TypeError, adapter.Error{Message: "Ausgabe nicht lesbar: " + parseErr.Error()})
	}
	return r.exitStatus(cmd, waitErr, ps.stderr.String())
}

// exitStatus bestimmt Endstatus und Grund aus dem Prozessende.
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
		reason := "vom Nutzer gestoppt"
		switch stopAs {
		case store.SessionInterrupted:
			reason = "Daemon wurde beendet"
		case store.SessionDone:
			reason = "beendet – Feedback läuft in einer neuen Session"
		}
		return stopAs, reason, exit
	}
	if waitErr == nil {
		return store.SessionDone, "", exit
	}
	msg := fmt.Sprintf("Prozess beendet: %v", waitErr)
	if s := strings.TrimSpace(stderr); s != "" {
		r.m.emit(r.id, adapter.TypeError, adapter.Error{Message: s})
	}
	return store.SessionFailed, msg, exit
}

// --- Steuerung ---------------------------------------------------------------------------

// endStatus liefert den gewünschten Endstatus nach stop() oder fallback.
func (r *run) endStatus(fallback string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopAs != "" {
		return r.stopAs
	}
	return fallback
}

// stop beendet den Prozess: stdin schließen, SIGTERM an die Prozessgruppe, nach
// StopTimeout SIGKILL. Vor dem Start bricht es das Warten ab.
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
		// Die Docker-CLI reicht Signale nicht an docker exec weiter: Prozesse im Container
		// über ihre Session-ID beenden; die CLI endet mit ihnen.
		sb, wsID, marker := r.m.opts.Workspaces.Sandbox(), r.workspaceID, EnvSessionID+"="+r.id
		go func() { _ = sb.Signal(context.Background(), wsID, marker, syscall.SIGTERM) }()
		if r.killTimer == nil {
			r.killTimer = time.AfterFunc(r.m.opts.StopTimeout, func() {
				_ = sb.Signal(context.Background(), wsID, marker, syscall.SIGKILL)
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

// send schreibt eine Folgeanweisung auf stdin.
func (r *run) send(a adapter.Adapter, text string) error {
	data, err := a.EncodeInput(text)
	if err != nil {
		return apperr.Invalid("%v", err)
	}
	r.mu.Lock()
	started := r.proc != nil
	r.mu.Unlock()
	if !started {
		return apperr.Conflict("der Agent startet noch – gleich noch einmal versuchen")
	}
	return r.write(data)
}

func (r *run) write(p []byte) error {
	r.mu.Lock()
	w := r.stdin
	r.mu.Unlock()
	if w == nil {
		return errors.New("kein Eingabekanal")
	}
	_, err := w.Write(p)
	return err
}

func (r *run) resize(cols, rows uint16) error {
	r.mu.Lock()
	ptmx := r.ptmx
	r.mu.Unlock()
	if ptmx == nil {
		return errors.New("kein Terminal")
	}
	return pty.Setsize(ptmx, &pty.Winsize{Cols: cols, Rows: rows})
}

// setStatus ändert den Zwischenstatus (running/idle) eines laufenden Prozesses.
func (r *run) setStatus(status, reason string) {
	r.mu.Lock()
	if r.status == status || r.proc == nil || r.stopAs != "" {
		r.mu.Unlock()
		return
	}
	r.status = status
	r.mu.Unlock()
	if err := r.m.opts.Store.UpdateSession(context.Background(), r.id, store.SessionUpdate{Status: &status}); err != nil {
		r.m.log.Error("Session-Status speichern", "session", r.id, "err", err)
	}
	r.m.emit(r.id, adapter.TypeStatus, adapter.Status{Status: status, Reason: reason})
	if status == store.SessionIdle {
		// Der Agent ist mit seiner Runde fertig: Stand sichern. Der Lauf hält den WaitGroup-
		// Zähler, Add ist daher hier erlaubt.
		r.m.wg.Add(1)
		go func() {
			defer r.m.wg.Done()
			r.m.collect(r.workspaceID, r.id)
		}()
	}
}

// sink nimmt die Events des Adapters entgegen.
type sink struct{ r *run }

func (s *sink) Emit(typ string, payload any) {
	r := s.r
	if c, ok := payload.(adapter.Cost); ok {
		r.mu.Lock()
		r.sessionUSD += c.TurnUSD
		c.TotalUSD = r.sessionUSD
		r.mu.Unlock()
		if err := r.m.opts.Store.UpdateSession(context.Background(), r.id, store.SessionUpdate{AddCostUSD: c.TurnUSD}); err != nil {
			r.m.log.Error("Kosten speichern", "session", r.id, "err", err)
		}
		payload = c
	}
	if typ == adapter.TypeStatus {
		// Statusmeldungen der CLI (z. B. Modell beim Start) als Information weitergeben,
		// den Session-Status aber nur über den Manager ändern.
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
		r.m.log.Error("Resume-ID speichern", "session", r.id, "err", err)
	}
}

func (s *sink) TurnDone() { s.r.setStatus(store.SessionIdle, "") }

// tailWriter behält die letzten max Bytes.
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
