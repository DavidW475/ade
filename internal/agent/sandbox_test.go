package agent

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/sandbox"
	"github.com/DavidW475/ade/internal/sandbox/sandboxtest"
	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/workspace"
)

// Ende-zu-Ende mit echtem Docker (nur mit ADE_TEST_DOCKER=1): Agent und Terminal laufen im
// Container, das Ergebnis landet im Workspace und per WIP-Commit im Projekt-Repo.

func sandboxSetup(t *testing.T, projectYAML string) *env {
	t.Helper()
	img := sandboxtest.Image(t)
	e := setup(t, projectYAML)
	sb := sandbox.New(sandbox.Options{Docker: sandboxtest.Docker(), DataDir: e.dataDir, Image: img, ProxyBinary: sandboxtest.ProxyBinary(t), Logger: slog.Default()})
	e.ws.SetSandbox(sb, true)
	t.Cleanup(func() {
		_, _ = sb.GC(context.Background(), func(string) bool { return false }, false)
	})
	return e
}

func TestSandboxAgentSession(t *testing.T) {
	e := sandboxSetup(t, "setup:\n  - echo setup-im-container > setup.txt; id -u >> setup.txt\n")
	e.fake.Argv = []string{"sh", "-c", `echo "$ADE_SESSION_ID $(pwd) $HOME" > arbeit.txt; test -e /etc/hostname && hostname >> arbeit.txt; echo "env:${SBX:-} ${HOSTKEY:-}" >> arbeit.txt; exec cat`}
	// Echte Keys (AdapterEnv) bleiben draußen; in der Sandbox gilt SandboxEnv (M6).
	e.m.opts.AdapterEnv = func(string) []string { return []string{"HOSTKEY=geheim"} }
	e.m.opts.SandboxEnv = func(string) []string { return []string{"SBX=proxy"} }
	s := e.start(StartRequest{Task: "im Container arbeiten"})
	w, err := e.ws.Get(e.ctx, s.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Sandbox || w.SandboxImage == "" {
		t.Fatalf("Workspace = %+v", w)
	}
	e.waitStatusTimeout(s.ID, store.SessionRunning, 2*time.Minute)
	if _, err := e.m.Input(e.ctx, s.ID, "weiter"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Echo der Anweisung", func() bool {
		for _, m := range messages(e.events(s.ID)) {
			if strings.Contains(m, "weiter") && strings.HasPrefix(m, "assistant") {
				return true
			}
		}
		return false
	})
	v, err := e.m.Stop(e.ctx, s.ID)
	if err != nil || v.Status != store.SessionStopped {
		t.Fatalf("Stop = %+v, %v", v, err)
	}
	data, err := os.ReadFile(w.Path + "/arbeit.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, s.ID+" /workspace /home/ade") || !strings.Contains(got, "ws-"+w.ID) {
		t.Errorf("arbeit.txt = %q (erwartet: Session, /workspace, Container-Hostname)", got)
	}
	if got := string(data); !strings.Contains(got, "env:proxy ") || strings.Contains(got, "geheim") {
		t.Errorf("arbeit.txt = %q (erwartet SandboxEnv ohne AdapterEnv)", got)
	}
	setupOut, _ := os.ReadFile(w.Path + "/setup.txt")
	if !strings.Contains(string(setupOut), "setup-im-container") {
		t.Errorf("setup.txt = %q", setupOut)
	}
	waitFor(t, "WIP-Commit im Projekt-Repo", func() bool { return len(e.commits(w)) == 1 })
	if files := e.gitOut(e.project.RepoPath, "show", "--name-only", "--format=", w.Branch); !strings.Contains(files, "arbeit.txt") {
		t.Errorf("Commit enthält: %s", files)
	}
	// Der Diff (über git im Container) zeigt die Arbeit gegenüber dem Basis-Commit.
	d, err := e.ws.Diff(e.ctx, w.ID, false)
	if err != nil || len(d.Files) != 2 || d.Head != e.gitOut(e.project.RepoPath, "rev-parse", w.Branch) {
		t.Errorf("Diff: %+v, %v", d, err)
	}
	// Verwerfen entfernt auch den Container.
	if err := e.ws.Delete(e.ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if c, _ := e.ws.Sandbox().Get(e.ctx, w.ID); c != nil {
		t.Errorf("Container nach Verwerfen: %+v", c)
	}
}

func TestSandboxTerminalAndStop(t *testing.T) {
	e := sandboxSetup(t, "")
	s := e.start(StartRequest{Adapter: "pty"})
	e.waitStatusTimeout(s.ID, store.SessionRunning, 2*time.Minute)
	term, err := e.m.AttachTerminal(e.ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if err := term.Write([]byte("echo \"pwd=$(pwd) term=$TERM\"; echo ergebnis-$((40+2))\n")); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	timeout := time.After(20 * time.Second)
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
	if !strings.Contains(out.String(), "pwd=/workspace term=xterm-256color") {
		t.Errorf("Ausgabe: %q", out.String())
	}
	// Ein langlaufender Befehl im Container wird beim Stoppen beendet.
	if err := term.Write([]byte("sleep 600\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	v, err := e.m.Stop(e.ctx, s.ID)
	if err != nil || v.Status != store.SessionStopped {
		t.Fatalf("Stop = %+v, %v", v, err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("Stop dauerte %s", time.Since(start))
	}
	cmd := e.ws.Sandbox().Command(e.ctx, s.WorkspaceID, sandbox.Exec{Argv: []string{"sh", "-c",
		`n=0; for d in /proc/[0-9]*; do tr '\0' '\n' < $d/environ 2>/dev/null | grep -qx "ADE_SESSION_ID=` + s.ID + `" && n=$((n+1)); done; echo $n`}})
	if got, err := cmd.Output(); err != nil || strings.TrimSpace(string(got)) != "0" {
		t.Errorf("Prozesse der Session im Container: %q, %v", got, err)
	}
}

func (e *env) waitStatusTimeout(id, want string, d time.Duration) {
	e.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if got := e.status(id); got == want {
			return
		} else if got == store.SessionFailed {
			v, _ := e.m.Get(e.ctx, id)
			e.t.Fatalf("Session fehlgeschlagen: %s", v.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatalf("Status %s nicht erreicht (ist %s)", want, e.status(id))
}

// Lese-Rolle in der Sandbox (M9): läuft im Lese-Container, /workspace ist nur lesend; Stop
// beendet ihre Prozesse dort.
func TestSandboxReadOnlyRole(t *testing.T) {
	e := sandboxSetup(t, "")
	e.fake.Argv = []string{"sh", "-c", `cat /workspace/README.md > /tmp/gelesen; if echo x > /workspace/neu 2>/dev/null; then echo geschrieben > /tmp/ergebnis; else echo verweigert > /tmp/ergebnis; fi; exec cat`}
	ws, err := e.ws.Create(e.ctx, e.project.ID, workspace.CreateOptions{Sandbox: boolPtr(true)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := e.m.StartRole(e.ctx, StartRequest{WorkspaceID: ws.ID, Task: "lesen"}, RoleStart{Role: "reviewer", Prompt: "lies", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	e.waitStatusTimeout(s.ID, store.SessionRunning, 2*time.Minute)
	sb := e.ws.Sandbox()
	read := func(path string) string {
		out, _ := sb.Command(e.ctx, ws.ID, sandbox.Exec{Argv: []string{"cat", path}, Reader: true}).Output()
		return strings.TrimSpace(string(out))
	}
	waitFor(t, "Ergebnis im Lese-Container", func() bool { return read("/tmp/ergebnis") != "" })
	if got := read("/tmp/ergebnis"); got != "verweigert" {
		t.Fatalf("Schreiben in /workspace: %s", got)
	}
	if got := read("/tmp/gelesen"); got != "hallo" {
		t.Fatalf("Lesen: %q", got)
	}
	if _, err := os.Stat(ws.Path + "/neu"); err == nil {
		t.Fatal("Datei der Lese-Rolle im Workspace")
	}
	if c, err := sb.GetReader(e.ctx, ws.ID); err != nil || c == nil || !c.Running {
		t.Fatalf("Lese-Container %+v %v", c, err)
	}
	v, err := e.m.Stop(e.ctx, s.ID)
	if err != nil || v.Status != store.SessionStopped {
		t.Fatalf("Stop = %+v, %v", v, err)
	}
	out, _ := sb.Command(e.ctx, ws.ID, sandbox.Exec{Reader: true, Argv: []string{"sh", "-c",
		`n=0; for d in /proc/[0-9]*; do tr '\0' '\n' < $d/environ 2>/dev/null | grep -qx "ADE_SESSION_ID=` + s.ID + `" && n=$((n+1)); done; echo $n`}}).Output()
	if strings.TrimSpace(string(out)) != "0" {
		t.Errorf("Prozesse der Lese-Rolle nach Stop: %q", out)
	}
	if err := e.ws.Delete(e.ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	if c, _ := sb.GetReader(e.ctx, ws.ID); c != nil {
		t.Errorf("Lese-Container nach Verwerfen: %+v", c)
	}
}

func boolPtr(b bool) *bool { return &b }
