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

// End to end with real Docker (only with ADE_TEST_DOCKER=1): agent and terminal run in the
// container, the result ends up in the workspace and via WIP commit in the project repository.

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
	e := sandboxSetup(t, "setup:\n  - echo setup-in-container > setup.txt; id -u >> setup.txt\n")
	e.fake.Argv = []string{"sh", "-c", `echo "$ADE_SESSION_ID $(pwd) $HOME" > work.txt; test -e /etc/hostname && hostname >> work.txt; echo "env:${SBX:-} ${HOSTKEY:-}" >> work.txt; exec cat`}
	// Real keys (AdapterEnv) stay out; in the sandbox SandboxEnv applies (M6).
	e.m.opts.AdapterEnv = func(string) []string { return []string{"HOSTKEY=secret"} }
	e.m.opts.SandboxEnv = func(string) []string { return []string{"SBX=proxy"} }
	s := e.start(StartRequest{Task: "work in the container"})
	w, err := e.ws.Get(e.ctx, s.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Sandbox || w.SandboxImage == "" {
		t.Fatalf("Workspace = %+v", w)
	}
	e.waitStatusTimeout(s.ID, store.SessionRunning, 2*time.Minute)
	if _, err := e.m.Input(e.ctx, s.ID, "continue"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "echo of the instruction", func() bool {
		for _, m := range messages(e.events(s.ID)) {
			if strings.Contains(m, "continue") && strings.HasPrefix(m, "assistant") {
				return true
			}
		}
		return false
	})
	v, err := e.m.Stop(e.ctx, s.ID)
	if err != nil || v.Status != store.SessionStopped {
		t.Fatalf("Stop = %+v, %v", v, err)
	}
	data, err := os.ReadFile(w.Path + "/work.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, s.ID+" /workspace /home/ade") || !strings.Contains(got, "ws-"+w.ID) {
		t.Errorf("work.txt = %q (want: session, /workspace, container host name)", got)
	}
	if got := string(data); !strings.Contains(got, "env:proxy ") || strings.Contains(got, "secret") {
		t.Errorf("work.txt = %q (want SandboxEnv without AdapterEnv)", got)
	}
	setupOut, _ := os.ReadFile(w.Path + "/setup.txt")
	if !strings.Contains(string(setupOut), "setup-in-container") {
		t.Errorf("setup.txt = %q", setupOut)
	}
	waitFor(t, "WIP commit in the project repository", func() bool { return len(e.commits(w)) == 1 })
	if files := e.gitOut(e.project.RepoPath, "show", "--name-only", "--format=", w.Branch); !strings.Contains(files, "work.txt") {
		t.Errorf("commit contains: %s", files)
	}
	// The diff (through git in the container) shows the work compared to the base commit.
	d, err := e.ws.Diff(e.ctx, w.ID, false)
	if err != nil || len(d.Files) != 2 || d.Head != e.gitOut(e.project.RepoPath, "rev-parse", w.Branch) {
		t.Errorf("Diff: %+v, %v", d, err)
	}
	// Discarding also removes the container.
	if err := e.ws.Delete(e.ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if c, _ := e.ws.Sandbox().Get(e.ctx, w.ID); c != nil {
		t.Errorf("container after discarding: %+v", c)
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
	if err := term.Write([]byte("echo \"pwd=$(pwd) term=$TERM\"; echo result-$((40+2))\n")); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	timeout := time.After(20 * time.Second)
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
	if !strings.Contains(out.String(), "pwd=/workspace term=xterm-256color") {
		t.Errorf("output: %q", out.String())
	}
	// A long-running command in the container is ended when stopping.
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
		t.Errorf("processes of the session in the container: %q, %v", got, err)
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
	e.t.Fatalf("status %s not reached (is %s)", want, e.status(id))
}

// Read-only role in the sandbox (M9): runs in the reader container, /workspace is read-only;
// Stop ends its processes there.
func TestSandboxReadOnlyRole(t *testing.T) {
	e := sandboxSetup(t, "")
	e.fake.Argv = []string{"sh", "-c", `cat /workspace/README.md > /tmp/read; if echo x > /workspace/new 2>/dev/null; then echo written > /tmp/result; else echo refused > /tmp/result; fi; exec cat`}
	ws, err := e.ws.Create(e.ctx, e.project.ID, workspace.CreateOptions{Sandbox: boolPtr(true)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := e.m.StartRole(e.ctx, StartRequest{WorkspaceID: ws.ID, Task: "read"}, RoleStart{Role: "reviewer", Prompt: "read it", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	e.waitStatusTimeout(s.ID, store.SessionRunning, 2*time.Minute)
	sb := e.ws.Sandbox()
	read := func(path string) string {
		out, _ := sb.Command(e.ctx, ws.ID, sandbox.Exec{Argv: []string{"cat", path}, Reader: true}).Output()
		return strings.TrimSpace(string(out))
	}
	waitFor(t, "result in the reader container", func() bool { return read("/tmp/result") != "" })
	if got := read("/tmp/result"); got != "refused" {
		t.Fatalf("writing to /workspace: %s", got)
	}
	if got := read("/tmp/read"); got != "hello" {
		t.Fatalf("reading: %q", got)
	}
	if _, err := os.Stat(ws.Path + "/new"); err == nil {
		t.Fatal("file of the read-only role in the workspace")
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
		t.Errorf("processes of the read-only role after Stop: %q", out)
	}
	if err := e.ws.Delete(e.ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	if c, _ := sb.GetReader(e.ctx, ws.ID); c != nil {
		t.Errorf("reader container after discarding: %+v", c)
	}
}

func boolPtr(b bool) *bool { return &b }
