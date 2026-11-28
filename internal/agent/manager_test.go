package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/agent/adapter"
	"github.com/DavidW475/ade/internal/agent/adapter/fake"
	"github.com/DavidW475/ade/internal/agent/adapter/pty"
	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/workspace"
)

type env struct {
	t       *testing.T
	ctx     context.Context
	st      *store.Store
	ps      *project.Service
	ws      *workspace.Manager
	m       *Manager
	project *project.View
	fake    *fake.Adapter
	reg     *adapter.Registry
	dataDir string
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func setup(t *testing.T, projectYAML string) *env {
	t.Helper()
	ctx := context.Background()
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if projectYAML != "" {
		if err := os.MkdirAll(filepath.Join(repo, ".ade"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, project.ConfigFile), []byte(projectYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")

	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "ade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ps := project.NewService(st)
	p, err := ps.Add(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	wm := workspace.NewManager(st, ps, t.TempDir(), log)
	t.Cleanup(wm.Close)
	e := &env{t: t, ctx: ctx, st: st, ps: ps, ws: wm, project: p, dataDir: dataDir, fake: &fake.Adapter{}}
	e.reg = adapter.NewRegistry(e.fake, pty.New())
	e.m = e.newManager()
	t.Cleanup(e.m.Close)
	wm.SetBeforeDelete(e.m.BeforeWorkspaceDelete)
	return e
}

func (e *env) newManager() *Manager {
	return NewManager(Options{
		Store: e.st, Projects: e.ps, Workspaces: e.ws, Adapters: e.reg, DataDir: e.dataDir,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), DefaultAdapter: "fake",
		FlushInterval: 10 * time.Millisecond, StopTimeout: 2 * time.Second,
	})
}

func (e *env) start(req StartRequest) *store.Session {
	e.t.Helper()
	if req.ProjectID == "" && req.WorkspaceID == "" {
		req.ProjectID = e.project.ID
	}
	s, err := e.m.Start(e.ctx, req)
	if err != nil {
		e.t.Fatalf("Start: %v", err)
	}
	return s
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *env) status(id string) string {
	s, err := e.st.GetSession(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return s.Status
}

func (e *env) waitStatus(id, want string) {
	e.t.Helper()
	waitFor(e.t, "Status "+want, func() bool { return e.status(id) == want })
}

func (e *env) events(id string) []store.Event {
	e.t.Helper()
	evs, err := e.m.Events(e.ctx, id, 0, 0, 10000)
	if err != nil {
		e.t.Fatal(err)
	}
	return evs
}

// messages returns the texts of all message events.
func messages(evs []store.Event) []string {
	var out []string
	for _, ev := range evs {
		if ev.Type == adapter.TypeMessage {
			var m adapter.Message
			_ = json.Unmarshal(ev.Payload, &m)
			out = append(out, m.Role+": "+m.Text)
		}
	}
	return out
}

func statuses(evs []store.Event) []string {
	var out []string
	for _, ev := range evs {
		if ev.Type == adapter.TypeStatus {
			var s adapter.Status
			_ = json.Unmarshal(ev.Payload, &s)
			out = append(out, s.Status)
		}
	}
	return out
}

func checkSeq(t *testing.T, evs []store.Event) {
	t.Helper()
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d – seq must be gapless from 1", i, ev.Seq)
		}
	}
}

func TestStartInputStop(t *testing.T) {
	e := setup(t, "")
	s := e.start(StartRequest{Task: "task 1"})
	if s.Status != store.SessionStarting || s.WorkspaceID == "" || s.ProjectID != e.project.ID || s.Adapter != "fake" {
		t.Fatalf("Session = %+v", s)
	}
	waitFor(t, "echo of the task", func() bool { return slices.Contains(messages(e.events(s.ID)), "assistant: echo: task 1") })

	// The process runs in the workspace with a controlled environment.
	ws, _ := e.ws.Get(e.ctx, s.WorkspaceID)
	if spec := e.fake.Specs()[len(e.fake.Specs())-1]; spec.Workdir != ws.Path || spec.SessionID != s.ID {
		t.Errorf("Spec = %+v", spec)
	}

	v, err := e.m.Input(e.ctx, s.ID, "follow-up")
	if err != nil || !v.Live {
		t.Fatalf("Input: %+v, %v", v, err)
	}
	waitFor(t, "echo of the follow-up", func() bool {
		return slices.Contains(messages(e.events(s.ID)), "assistant: echo: follow-up")
	})

	v, err = e.m.Stop(e.ctx, s.ID)
	if err != nil || v.Status != store.SessionStopped || v.Live || v.EndedAt == nil {
		t.Fatalf("Stop: %+v, %v", v, err)
	}
	evs := e.events(s.ID)
	checkSeq(t, evs)
	if got := statuses(evs); !slices.Equal(got, []string{"starting", "running", "stopped"}) {
		t.Errorf("Status-Events = %v", got)
	}
	if got := messages(evs); !slices.Equal(got, []string{"user: task 1", "assistant: echo: task 1", "user: follow-up", "assistant: echo: follow-up"}) {
		t.Errorf("Nachrichten = %v", got)
	}
	if _, err := e.m.Input(e.ctx, s.ID, "something else"); !errors.Is(err, apperr.ErrConflict) {
		t.Errorf("Input after Stop without resume ID: %v", err)
	}
}

func TestStartValidation(t *testing.T) {
	e := setup(t, "")
	cases := []StartRequest{
		{ProjectID: e.project.ID},                                       // task missing
		{ProjectID: e.project.ID, Task: "x", Adapter: "doesnotexist"},   // unknown adapter
		{ProjectID: e.project.ID, Task: "x", Command: []string{"bash"}}, // command only for pty
		{Task: "x"}, // neither project nor workspace
	}
	for _, req := range cases {
		if _, err := e.m.Start(e.ctx, req); !errors.Is(err, apperr.ErrInvalid) {
			t.Errorf("Start(%+v) = %v, want ErrInvalid", req, err)
		}
	}
	if _, err := e.m.Start(e.ctx, StartRequest{ProjectID: "gibtsnicht", Task: "x"}); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("unknown project: %v", err)
	}
}

func TestOneAgentPerWorkspace(t *testing.T) {
	e := setup(t, "")
	s := e.start(StartRequest{Task: "a"})
	_, err := e.m.Start(e.ctx, StartRequest{WorkspaceID: s.WorkspaceID, Task: "b"})
	if !errors.Is(err, apperr.ErrConflict) {
		t.Fatalf("zweiter Agent: %v", err)
	}
	// A terminal next to it is allowed.
	term := e.start(StartRequest{WorkspaceID: s.WorkspaceID, Adapter: "pty", Command: []string{"sh"}})
	if term.WorkspaceID != s.WorkspaceID {
		t.Fatalf("Terminal in anderem Workspace: %+v", term)
	}
	sessions, err := e.m.List(e.ctx, store.SessionFilter{WorkspaceID: s.WorkspaceID})
	if err != nil || len(sessions) != 2 {
		t.Fatalf("List = %d, %v", len(sessions), err)
	}
}

func TestExitStatusAndResume(t *testing.T) {
	e := setup(t, "")
	// Reports a resume ID, ends the turn and then waits for more input.
	e.fake.Argv = []string{"sh", "-c", `echo '{"type":"resume_id","payload":{"id":"cli-1"}}'; echo '{"type":"cost","payload":{"turn_usd":0.25}}'; echo '{"type":"turn_done"}'; exec cat`}
	e.fake.AdapterName = "fake"
	s := e.start(StartRequest{Task: "los"})
	e.waitStatus(s.ID, store.SessionIdle)

	v, _ := e.m.Get(e.ctx, s.ID)
	if v.ResumeID != "cli-1" || v.Resumable || !v.Live {
		t.Fatalf("laufend: %+v", v)
	}
	if _, err := e.m.Stop(e.ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = e.m.Get(e.ctx, s.ID)
	if !v.Resumable || v.Status != store.SessionStopped {
		t.Fatalf("not resumable after Stop: %+v", v)
	}

	// Resume: new process with the resume ID and the new instruction.
	if _, err := e.m.Input(e.ctx, s.ID, "continue"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "echo after resuming", func() bool { return slices.Contains(messages(e.events(s.ID)), "assistant: echo: continue") })
	specs := e.fake.Specs()
	if last := specs[len(specs)-1]; last.ResumeID != "cli-1" || last.Task != "continue" {
		t.Errorf("spec when resuming = %+v", last)
	}
	e.waitStatus(s.ID, store.SessionIdle)
	v, _ = e.m.Get(e.ctx, s.ID)
	if v.EndedAt != nil || v.CostUSD != 0.5 {
		t.Errorf("after resuming: ended=%v cost=%v", v.EndedAt, v.CostUSD)
	}
	// Cost events carry the session's total costs.
	var totals []float64
	for _, ev := range e.events(s.ID) {
		if ev.Type == adapter.TypeCost {
			var c adapter.Cost
			_ = json.Unmarshal(ev.Payload, &c)
			totals = append(totals, c.TotalUSD)
		}
	}
	if !slices.Equal(totals, []float64{0.25, 0.5}) {
		t.Errorf("total_usd = %v", totals)
	}
}

func TestFailedProcess(t *testing.T) {
	e := setup(t, "")
	e.fake.Argv = []string{"sh", "-c", "echo broken >&2; exit 3"}
	s := e.start(StartRequest{Task: "x"})
	e.waitStatus(s.ID, store.SessionFailed)
	v, _ := e.m.Get(e.ctx, s.ID)
	if v.ExitCode == nil || *v.ExitCode != 3 || v.Error == "" {
		t.Fatalf("Session = %+v", v)
	}
	var stderr bool
	for _, ev := range e.events(s.ID) {
		if ev.Type == adapter.TypeError && strings.Contains(string(ev.Payload), "broken") {
			stderr = true
		}
	}
	if !stderr {
		t.Error("stderr missing in the events")
	}

	e.fake.Argv = []string{"ade-does-not-exist"}
	s = e.start(StartRequest{Task: "x"})
	e.waitStatus(s.ID, store.SessionFailed)
}

func TestWaitsForWorkspaceSetup(t *testing.T) {
	e := setup(t, "setup:\n  - sleep 0.3\n")
	s := e.start(StartRequest{Task: "x"})
	if s.Status != store.SessionStarting {
		t.Fatalf("Status = %s", s.Status)
	}
	ws, _ := e.ws.Get(e.ctx, s.WorkspaceID)
	if ws.Status != store.WorkspaceSetup {
		t.Fatalf("Workspace-Status = %s", ws.Status)
	}
	e.waitStatus(s.ID, store.SessionRunning)
	ws, _ = e.ws.Get(e.ctx, s.WorkspaceID)
	if ws.Status != store.WorkspaceReady {
		t.Errorf("agent ran before the setup ended (workspace %s)", ws.Status)
	}
}

func TestStopWhileWaitingForSetup(t *testing.T) {
	e := setup(t, "setup:\n  - sleep 5\n")
	s := e.start(StartRequest{Task: "x"})
	v, err := e.m.Stop(e.ctx, s.ID)
	if err != nil || v.Status != store.SessionStopped {
		t.Fatalf("Stop = %+v, %v", v, err)
	}
	if len(e.fake.Specs()) != 1 { // only the pre-check in Start
		t.Errorf("process was started despite Stop")
	}
}

func TestSubscribeReplayAndFanOut(t *testing.T) {
	e := setup(t, "")
	s := e.start(StartRequest{Task: "one"})
	waitFor(t, "erstes Echo", func() bool { return len(messages(e.events(s.ID))) == 2 })

	sub1, upto, err := e.m.Subscribe(e.ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer sub1.Close()
	sub2, upto2, _ := e.m.Subscribe(e.ctx, s.ID)
	defer sub2.Close()
	if upto != upto2 {
		t.Fatalf("upto %d != %d", upto, upto2)
	}
	hist, _ := e.m.Events(e.ctx, s.ID, 0, upto, 1000)
	if int64(len(hist)) != upto {
		t.Fatalf("Historie %d Events, upto %d", len(hist), upto)
	}
	// Replay from a seq.
	part, _ := e.m.Events(e.ctx, s.ID, 2, upto, 1000)
	if len(part) != len(hist)-2 || part[0].Seq != 3 {
		t.Fatalf("Replay ab 2: %d Events, erstes %d", len(part), part[0].Seq)
	}

	if _, err := e.m.Input(e.ctx, s.ID, "two"); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []*Subscription{sub1, sub2} {
		var got []string
		timeout := time.After(5 * time.Second)
		for !slices.Contains(got, "assistant: echo: two") {
			select {
			case ev := <-sub.C():
				if ev.Seq <= upto {
					t.Fatalf("live event with seq %d <= upto %d", ev.Seq, upto)
				}
				got = append(got, messages([]store.Event{ev})...)
			case <-timeout:
				t.Fatalf("Live-Events fehlen, bisher %v", got)
			}
		}
	}
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	e := setup(t, "")
	s := e.start(StartRequest{Task: "x"})
	sub, _, err := e.m.Subscribe(e.ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	for range subBuffer + 10 {
		e.m.emit(s.ID, adapter.TypeMessage, adapter.Message{Role: "assistant", Text: "flut"})
	}
	n := 0
	for range sub.C() {
		n++
	}
	if !sub.Lagged() || n > subBuffer {
		t.Fatalf("lagged=%v after %d events", sub.Lagged(), n)
	}
}

func TestEventsAreBatched(t *testing.T) {
	e := setup(t, "")
	m := NewManager(Options{Store: e.st, Projects: e.ps, Workspaces: e.ws, Adapters: e.reg, DataDir: e.dataDir,
		DefaultAdapter: "fake", FlushInterval: time.Hour, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer m.Close()
	ws, err := e.ws.Create(e.ctx, e.project.ID, workspace.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{ID: "batch1", WorkspaceID: ws.ID, Adapter: "fake", Status: store.SessionDone}
	if err := e.st.CreateSession(e.ctx, sess); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		m.emit(sess.ID, adapter.TypeMessage, adapter.Message{Role: "assistant", Text: "x"})
	}
	if raw, _ := e.st.ListEvents(e.ctx, sess.ID, 0, 0); len(raw) != 0 {
		t.Fatalf("%d events written right away, want buffered", len(raw))
	}
	for range flushBatch {
		m.emit(sess.ID, adapter.TypeMessage, adapter.Message{Role: "assistant", Text: "x"})
	}
	if raw, _ := e.st.ListEvents(e.ctx, sess.ID, 0, 10000); len(raw) != flushBatch {
		t.Fatalf("after %d events: %d written, want %d (one batch)", flushBatch+3, len(raw), flushBatch)
	}
	evs, _ := m.Events(e.ctx, sess.ID, 0, 0, 10000)
	if len(evs) != flushBatch+3 {
		t.Fatalf("Events = %d", len(evs))
	}
	checkSeq(t, evs)
}

func TestPTYSession(t *testing.T) {
	e := setup(t, "")
	s := e.start(StartRequest{Adapter: "pty", Command: []string{"sh"}})
	e.waitStatus(s.ID, store.SessionRunning)

	term, err := e.m.AttachTerminal(e.ctx, s.ID)
	if err != nil || !term.Live {
		t.Fatalf("Attach: %+v, %v", term, err)
	}
	defer term.Close()
	if err := term.Resize(100, 40); err != nil {
		t.Fatal(err)
	}
	if err := term.Write([]byte("echo \"ws=$ADE_WORKSPACE_ID cols=$(tput cols 2>/dev/null || stty size)\"; echo result-$((40+2))\n")); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	timeout := time.After(5 * time.Second)
	for !strings.Contains(out.String(), "result-42") {
		select {
		case chunk, ok := <-term.Output:
			if !ok {
				t.Fatalf("output ended: %q", out.String())
			}
			out.Write(chunk)
		case <-timeout:
			t.Fatalf("no output: %q", out.String())
		}
	}
	if !strings.Contains(out.String(), "ws="+s.WorkspaceID) {
		t.Errorf("ADE_WORKSPACE_ID missing: %q", out.String())
	}

	// A second client sees the output so far.
	t2, _ := e.m.AttachTerminal(e.ctx, s.ID)
	if !strings.Contains(string(t2.Replay), "result-42") {
		t.Errorf("replay without the output so far: %q", t2.Replay)
	}
	t2.Close()

	if err := term.Write([]byte("exit 4\n")); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(s.ID, store.SessionFailed)
	v, _ := e.m.Get(e.ctx, s.ID)
	if v.ExitCode == nil || *v.ExitCode != 4 {
		t.Errorf("Exit-Code = %v", v.ExitCode)
	}
	waitFor(t, "Output geschlossen", func() bool {
		select {
		case _, ok := <-term.Output:
			return !ok
		default:
			return false
		}
	})
	if !term.Ended() {
		t.Error("Ended = false after the process ended")
	}

	// After the end the output comes from the log.
	t3, err := e.m.AttachTerminal(e.ctx, s.ID)
	if err != nil || t3.Live || !strings.Contains(string(t3.Replay), "result-42") {
		t.Fatalf("after the end: live=%v replay=%q err=%v", t3.Live, t3.Replay, err)
	}
	if err := t3.Write([]byte("x")); err == nil {
		t.Error("writing to an ended terminal without an error")
	}
	if _, err := e.m.AttachTerminal(e.ctx, e.start(StartRequest{Task: "x"}).ID); !errors.Is(err, apperr.ErrInvalid) {
		t.Errorf("terminal of an agent session: %v", err)
	}
}

func TestPTYStop(t *testing.T) {
	e := setup(t, "")
	s := e.start(StartRequest{Adapter: "pty", Command: []string{"sleep", "60"}})
	e.waitStatus(s.ID, store.SessionRunning)
	v, err := e.m.Stop(e.ctx, s.ID)
	if err != nil || v.Status != store.SessionStopped {
		t.Fatalf("Stop = %+v, %v", v, err)
	}
}

// A crashed daemon leaves active sessions and processes behind. The next start marks the
// sessions as interrupted and only ends its own orphaned processes.
func TestRecoverAfterCrash(t *testing.T) {
	e := setup(t, "")
	ws, err := e.ws.Create(e.ctx, e.project.ID, workspace.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(id, status, resume string) {
		if err := e.st.CreateSession(e.ctx, &store.Session{ID: id, WorkspaceID: ws.ID, Adapter: "fake", Status: status, ResumeID: resume}); err != nil {
			t.Fatal(err)
		}
	}
	mk("was-running", store.SessionRunning, "cli-9")
	mk("was-waiting", store.SessionIdle, "")
	mk("finished", store.SessionDone, "")
	// History before the crash.
	e.m.emit("was-running", adapter.TypeMessage, adapter.Message{Role: "user", Text: "before"})
	e.m.flushAll()

	spawn := func(sessionID string) *exec.Cmd {
		cmd := exec.Command("sleep", "60")
		cmd.Env = append(os.Environ(), EnvSessionID+"="+sessionID)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd
	}
	orphan := spawn("was-running")
	foreign := spawn("foreign") // does not belong to this database
	done := spawn("finished")   // the session is not active

	m2 := e.newManager()
	defer m2.Close()
	if err := m2.Recover(e.ctx); err != nil {
		t.Fatal(err)
	}

	exited := make(chan error, 1)
	go func() { exited <- orphan.Wait() }()
	select {
	case err := <-exited:
		t.Logf("orphaned process ended: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("orphaned process still running")
	}
	for _, c := range []*exec.Cmd{foreign, done} {
		if err := c.Process.Signal(syscall.Signal(0)); err != nil {
			t.Errorf("foreign process ended: %v", err)
		}
	}

	for id, want := range map[string]string{"was-running": store.SessionInterrupted, "was-waiting": store.SessionInterrupted, "finished": store.SessionDone} {
		if got := e.status(id); got != want {
			t.Errorf("%s: Status %s, want %s", id, got, want)
		}
	}
	evs, _ := m2.Events(e.ctx, "was-running", 0, 0, 100)
	checkSeq(t, evs)
	if len(evs) != 2 || statuses(evs)[0] != store.SessionInterrupted || messages(evs)[0] != "user: before" {
		t.Errorf("history after restart: %+v", evs)
	}
	v, _ := m2.Get(e.ctx, "was-running")
	if !v.Resumable {
		t.Error("interrupted session with resume ID not resumable")
	}
	if v, _ := m2.Get(e.ctx, "was-waiting"); v.Resumable {
		t.Error("session without resume ID resumable")
	}
}

func TestCloseInterruptsRunningSessions(t *testing.T) {
	e := setup(t, "")
	s := e.start(StartRequest{Task: "x"})
	e.waitStatus(s.ID, store.SessionRunning)
	e.m.Close()
	if got := e.status(s.ID); got != store.SessionInterrupted {
		t.Fatalf("status after Close = %s", got)
	}
	if _, err := e.m.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Task: "y"}); !errors.Is(err, apperr.ErrConflict) {
		t.Errorf("Start after Close: %v", err)
	}
}

func TestWorkspaceDeleteStopsSessions(t *testing.T) {
	e := setup(t, "")
	s := e.start(StartRequest{Adapter: "pty", Command: []string{"sh"}})
	e.waitStatus(s.ID, store.SessionRunning)
	if err := e.ws.Delete(e.ctx, s.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.GetSession(e.ctx, s.ID); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("session after deleting the workspace: %v", err)
	}
	if _, err := os.Stat(e.m.terminalLog(s.ID)); !os.IsNotExist(err) {
		t.Errorf("terminal log not deleted: %v", err)
	}
	waitFor(t, "no running process", func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		return len(e.m.runs) == 0
	})
}

func TestBuildEnv(t *testing.T) {
	base := []string{"HOME=/home/u", "PATH=/bin", "GH_TOKEN=secret", "SSH_AUTH_SOCK=/tmp/s", "LC_ALL=de_DE.UTF-8", "ADE_CONFIG=/x", "TERM=screen", "broken"}
	env := buildEnv(base, true, "s1", "w1", "/ws", []string{"ANTHROPIC_API_KEY=k", "HOME=/anders"})
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	for _, k := range []string{"GH_TOKEN", "SSH_AUTH_SOCK", "ADE_CONFIG"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s was passed on", k)
		}
	}
	want := map[string]string{"PATH": "/bin", "LC_ALL": "de_DE.UTF-8", "TERM": "xterm-256color", EnvSessionID: "s1",
		EnvWorkspaceID: "w1", EnvWorkspacePath: "/ws", "PWD": "/ws", "ANTHROPIC_API_KEY": "k", "HOME": "/anders"}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	if pipes := buildEnv(base, false, "s", "w", "/ws", nil); !slices.Contains(pipes, "TERM=dumb") {
		t.Errorf("without terminal: %v", pipes)
	}
}
