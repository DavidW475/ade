package agent

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/sandbox"
	"github.com/DavidW475/ade/internal/store"
)

// Ende-zu-Ende mit echtem Docker (nur mit ADE_TEST_DOCKER=1): Agent und Terminal laufen im
// Container, das Ergebnis landet im Workspace und per WIP-Commit im Projekt-Repo.

func sandboxSetup(t *testing.T, projectYAML string) *env {
	t.Helper()
	if os.Getenv("ADE_TEST_DOCKER") != "1" {
		t.Skip("Docker-Tests nur mit ADE_TEST_DOCKER=1")
	}
	img := os.Getenv("ADE_TEST_IMAGE")
	if img == "" {
		img = "ade-test-git:1" // baut internal/sandbox/docker_test.go
		if exec.Command("docker", "image", "inspect", img).Run() != nil {
			cmd := exec.Command("docker", "build", "-t", img, "-")
			cmd.Stdin = strings.NewReader("FROM debian:trixie-slim\nRUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/*\n")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("Test-Image: %v\n%s", err, out)
			}
		}
	}
	e := setup(t, projectYAML)
	sb := sandbox.New(sandbox.Options{DataDir: e.dataDir, Image: img, Logger: slog.Default()})
	e.ws.SetSandbox(sb, true)
	t.Cleanup(func() {
		_, _ = sb.GC(context.Background(), func(string) bool { return false }, false)
	})
	return e
}

func TestSandboxAgentSession(t *testing.T) {
	e := sandboxSetup(t, "setup:\n  - echo setup-im-container > setup.txt; id -u >> setup.txt\n")
	e.fake.Argv = []string{"sh", "-c", `echo "$ADE_SESSION_ID $(pwd) $HOME" > arbeit.txt; test -e /etc/hostname && hostname >> arbeit.txt; exec cat`}
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
