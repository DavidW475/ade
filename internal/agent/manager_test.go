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
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hallo\n"), 0o600); err != nil {
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
			t.Fatalf("Zeitüberschreitung: %s", what)
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

// messages liefert die Texte aller message-Events.
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
			t.Fatalf("Event %d hat seq %d – seq muss lückenlos ab 1 laufen", i, ev.Seq)
		}
	}
}

func TestStartInputStop(t *testing.T) {
	e := setup(t, "")
	s := e.start(StartRequest{Task: "Aufgabe 1"})
	if s.Status != store.SessionStarting || s.WorkspaceID == "" || s.ProjectID != e.project.ID || s.Adapter != "fake" {
		t.Fatalf("Session = %+v", s)
	}
	waitFor(t, "Echo der Aufgabe", func() bool { return slices.Contains(messages(e.events(s.ID)), "assistant: echo: Aufgabe 1") })

	// Prozess läuft im Workspace mit kontrollierter Umgebung.
	ws, _ := e.ws.Get(e.ctx, s.WorkspaceID)
	if spec := e.fake.Specs()[len(e.fake.Specs())-1]; spec.Workdir != ws.Path || spec.SessionID != s.ID {
		t.Errorf("Spec = %+v", spec)
	}

	v, err := e.m.Input(e.ctx, s.ID, "Folgeanweisung")
	if err != nil || !v.Live {
		t.Fatalf("Input: %+v, %v", v, err)
	}
	waitFor(t, "Echo der Folgeanweisung", func() bool {
		return slices.Contains(messages(e.events(s.ID)), "assistant: echo: Folgeanweisung")
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
	if got := messages(evs); !slices.Equal(got, []string{"user: Aufgabe 1", "assistant: echo: Aufgabe 1", "user: Folgeanweisung", "assistant: echo: Folgeanweisung"}) {
		t.Errorf("Nachrichten = %v", got)
	}
	if _, err := e.m.Input(e.ctx, s.ID, "noch was"); !errors.Is(err, apperr.ErrConflict) {
		t.Errorf("Input nach Stop ohne Resume-ID: %v", err)
	}
}

func TestStartValidation(t *testing.T) {
	e := setup(t, "")
	cases := []StartRequest{
		{ProjectID: e.project.ID},                                       // Aufgabe fehlt
		{ProjectID: e.project.ID, Task: "x", Adapter: "gibtsnicht"},     // unbekannter Adapter
		{ProjectID: e.project.ID, Task: "x", Command: []string{"bash"}}, // Befehl nur bei pty
		{Task: "x"}, // weder Projekt noch Workspace
	}
	for _, req := range cases {
		if _, err := e.m.Start(e.ctx, req); !errors.Is(err, apperr.ErrInvalid) {
			t.Errorf("Start(%+v) = %v, want ErrInvalid", req, err)
		}
	}
	if _, err := e.m.Start(e.ctx, StartRequest{ProjectID: "gibtsnicht", Task: "x"}); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("unbekanntes Projekt: %v", err)
	}
}

func TestOneAgentPerWorkspace(t *testing.T) {
	e := setup(t, "")
	s := e.start(StartRequest{Task: "a"})
	_, err := e.m.Start(e.ctx, StartRequest{WorkspaceID: s.WorkspaceID, Task: "b"})
	if !errors.Is(err, apperr.ErrConflict) {
		t.Fatalf("zweiter Agent: %v", err)
	}
	// Ein Terminal daneben ist erlaubt.
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
	// Meldet eine Resume-ID, beendet den Durchgang und wartet dann auf weitere Eingaben.
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
		t.Fatalf("nach Stop nicht fortsetzbar: %+v", v)
	}

	// Fortsetzen: neuer Prozess mit Resume-ID und der neuen Anweisung.
	if _, err := e.m.Input(e.ctx, s.ID, "weiter"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Echo nach Fortsetzen", func() bool { return slices.Contains(messages(e.events(s.ID)), "assistant: echo: weiter") })
	specs := e.fake.Specs()
	if last := specs[len(specs)-1]; last.ResumeID != "cli-1" || last.Task != "weiter" {
		t.Errorf("Spec beim Fortsetzen = %+v", last)
	}
	e.waitStatus(s.ID, store.SessionIdle)
	v, _ = e.m.Get(e.ctx, s.ID)
	if v.EndedAt != nil || v.CostUSD != 0.5 {
		t.Errorf("nach Fortsetzen: ended=%v cost=%v", v.EndedAt, v.CostUSD)
	}
	// Kosten-Events tragen die Gesamtkosten der Session.
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
	e.fake.Argv = []string{"sh", "-c", "echo kaputt >&2; exit 3"}
	s := e.start(StartRequest{Task: "x"})
	e.waitStatus(s.ID, store.SessionFailed)
	v, _ := e.m.Get(e.ctx, s.ID)
	if v.ExitCode == nil || *v.ExitCode != 3 || v.Error == "" {
		t.Fatalf("Session = %+v", v)
	}
	var stderr bool
	for _, ev := range e.events(s.ID) {
		if ev.Type == adapter.TypeError && strings.Contains(string(ev.Payload), "kaputt") {
			stderr = true
		}
	}
	if !stderr {
		t.Error("stderr fehlt in den Events")
	}

	e.fake.Argv = []string{"gibt-es-nicht-ade"}
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
		t.Errorf("Agent lief vor Ende des Setups (Workspace %s)", ws.Status)
	}
}

func TestStopWhileWaitingForSetup(t *testing.T) {
	e := setup(t, "setup:\n  - sleep 5\n")
	s := e.start(StartRequest{Task: "x"})
	v, err := e.m.Stop(e.ctx, s.ID)
	if err != nil || v.Status != store.SessionStopped {
		t.Fatalf("Stop = %+v, %v", v, err)
	}
	if len(e.fake.Specs()) != 1 { // nur die Vorabprüfung in Start
		t.Errorf("Prozess wurde trotz Stop gestartet")
	}
}

func TestSubscribeReplayAndFanOut(t *testing.T) {
	e := setup(t, "")
	s := e.start(StartRequest{Task: "eins"})
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
	// Replay ab einer seq.
	part, _ := e.m.Events(e.ctx, s.ID, 2, upto, 1000)
	if len(part) != len(hist)-2 || part[0].Seq != 3 {
		t.Fatalf("Replay ab 2: %d Events, erstes %d", len(part), part[0].Seq)
	}

	if _, err := e.m.Input(e.ctx, s.ID, "zwei"); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []*Subscription{sub1, sub2} {
		var got []string
		timeout := time.After(5 * time.Second)
		for !slices.Contains(got, "assistant: echo: zwei") {
			select {
			case ev := <-sub.C():
				if ev.Seq <= upto {
					t.Fatalf("Live-Event mit seq %d <= upto %d", ev.Seq, upto)
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
		t.Fatalf("lagged=%v nach %d Events", sub.Lagged(), n)
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
		t.Fatalf("%d Events sofort geschrieben, erwartet gepuffert", len(raw))
	}
	for range flushBatch {
		m.emit(sess.ID, adapter.TypeMessage, adapter.Message{Role: "assistant", Text: "x"})
	}
	if raw, _ := e.st.ListEvents(e.ctx, sess.ID, 0, 10000); len(raw) != flushBatch {
		t.Fatalf("nach %d Events: %d geschrieben, erwartet %d (ein Bündel)", flushBatch+3, len(raw), flushBatch)
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
	if err := term.Write([]byte("echo \"ws=$ADE_WORKSPACE_ID cols=$(tput cols 2>/dev/null || stty size)\"; echo ergebnis-$((40+2))\n")); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	timeout := time.After(5 * time.Second)
	for !strings.Contains(out.String(), "ergebnis-42") {
		select {
		case chunk, ok := <-term.Output:
			if !ok {
				t.Fatalf("Ausgabe endete: %q", out.String())
			}
			out.Write(chunk)
		case <-timeout:
			t.Fatalf("keine Ausgabe: %q", out.String())
		}
	}
	if !strings.Contains(out.String(), "ws="+s.WorkspaceID) {
		t.Errorf("ADE_WORKSPACE_ID fehlt: %q", out.String())
	}

	// Ein zweiter Client sieht die bisherige Ausgabe.
	t2, _ := e.m.AttachTerminal(e.ctx, s.ID)
	if !strings.Contains(string(t2.Replay), "ergebnis-42") {
		t.Errorf("Replay ohne bisherige Ausgabe: %q", t2.Replay)
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
		t.Error("Ended = false nach Prozessende")
	}

	// Nach dem Ende kommt die Ausgabe aus dem Log.
	t3, err := e.m.AttachTerminal(e.ctx, s.ID)
	if err != nil || t3.Live || !strings.Contains(string(t3.Replay), "ergebnis-42") {
		t.Fatalf("nach Ende: live=%v replay=%q err=%v", t3.Live, t3.Replay, err)
	}
	if err := t3.Write([]byte("x")); err == nil {
		t.Error("Schreiben in beendetes Terminal ohne Fehler")
	}
	if _, err := e.m.AttachTerminal(e.ctx, e.start(StartRequest{Task: "x"}).ID); !errors.Is(err, apperr.ErrInvalid) {
		t.Errorf("Terminal einer Agent-Session: %v", err)
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

// Ein abgestürzter Daemon hinterlässt aktive Sessions und Prozesse. Der nächste Start
// markiert die Sessions als unterbrochen und beendet nur die eigenen verwaisten Prozesse.
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
	mk("lief", store.SessionRunning, "cli-9")
	mk("wartete", store.SessionIdle, "")
	mk("fertig", store.SessionDone, "")
	// Historie vor dem Absturz.
	e.m.emit("lief", adapter.TypeMessage, adapter.Message{Role: "user", Text: "vorher"})
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
	orphan := spawn("lief")
	foreign := spawn("fremd") // gehört nicht zu dieser Datenbank
	done := spawn("fertig")   // Session ist nicht aktiv

	m2 := e.newManager()
	defer m2.Close()
	if err := m2.Recover(e.ctx); err != nil {
		t.Fatal(err)
	}

	exited := make(chan error, 1)
	go func() { exited <- orphan.Wait() }()
	select {
	case err := <-exited:
		t.Logf("verwaister Prozess beendet: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("verwaister Prozess läuft noch")
	}
	for _, c := range []*exec.Cmd{foreign, done} {
		if err := c.Process.Signal(syscall.Signal(0)); err != nil {
			t.Errorf("fremder Prozess beendet: %v", err)
		}
	}

	for id, want := range map[string]string{"lief": store.SessionInterrupted, "wartete": store.SessionInterrupted, "fertig": store.SessionDone} {
		if got := e.status(id); got != want {
			t.Errorf("%s: Status %s, want %s", id, got, want)
		}
	}
	evs, _ := m2.Events(e.ctx, "lief", 0, 0, 100)
	checkSeq(t, evs)
	if len(evs) != 2 || statuses(evs)[0] != store.SessionInterrupted || messages(evs)[0] != "user: vorher" {
		t.Errorf("Historie nach Neustart: %+v", evs)
	}
	v, _ := m2.Get(e.ctx, "lief")
	if !v.Resumable {
		t.Error("unterbrochene Session mit Resume-ID nicht fortsetzbar")
	}
	if v, _ := m2.Get(e.ctx, "wartete"); v.Resumable {
		t.Error("Session ohne Resume-ID fortsetzbar")
	}
}

func TestCloseInterruptsRunningSessions(t *testing.T) {
	e := setup(t, "")
	s := e.start(StartRequest{Task: "x"})
	e.waitStatus(s.ID, store.SessionRunning)
	e.m.Close()
	if got := e.status(s.ID); got != store.SessionInterrupted {
		t.Fatalf("Status nach Close = %s", got)
	}
	if _, err := e.m.Start(e.ctx, StartRequest{ProjectID: e.project.ID, Task: "y"}); !errors.Is(err, apperr.ErrConflict) {
		t.Errorf("Start nach Close: %v", err)
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
		t.Errorf("Session nach Löschen des Workspaces: %v", err)
	}
	if _, err := os.Stat(e.m.terminalLog(s.ID)); !os.IsNotExist(err) {
		t.Errorf("Terminal-Log nicht gelöscht: %v", err)
	}
	waitFor(t, "kein laufender Prozess", func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		return len(e.m.runs) == 0
	})
}

func TestBuildEnv(t *testing.T) {
	base := []string{"HOME=/home/u", "PATH=/bin", "GH_TOKEN=geheim", "SSH_AUTH_SOCK=/tmp/s", "LC_ALL=de_DE.UTF-8", "ADE_CONFIG=/x", "TERM=screen", "kaputt"}
	env := buildEnv(base, true, "s1", "w1", "/ws", []string{"ANTHROPIC_API_KEY=k", "HOME=/anders"})
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	for _, k := range []string{"GH_TOKEN", "SSH_AUTH_SOCK", "ADE_CONFIG"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s wurde durchgereicht", k)
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
		t.Errorf("ohne Terminal: %v", pipes)
	}
}
