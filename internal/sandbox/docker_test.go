package sandbox_test

// Integrationstests gegen einen echten Docker-Daemon. Sie laufen nur mit ADE_TEST_DOCKER=1
// (CI setzt das). ADE_TEST_IMAGE wählt ein Image mit git und sh; ohne Angabe baut der Test
// ein kleines Debian-Image mit git.

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/git"
	"github.com/DavidW475/ade/internal/sandbox"
)

const testImageDockerfile = `FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/*
`

func testImage(t *testing.T) string {
	t.Helper()
	if os.Getenv("ADE_TEST_DOCKER") != "1" {
		t.Skip("Docker-Tests nur mit ADE_TEST_DOCKER=1")
	}
	if img := os.Getenv("ADE_TEST_IMAGE"); img != "" {
		return img
	}
	const tag = "ade-test-git:1"
	if exec.Command("docker", "image", "inspect", tag).Run() == nil {
		return tag
	}
	cmd := exec.Command("docker", "build", "-t", tag, "-")
	cmd.Stdin = strings.NewReader(testImageDockerfile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Test-Image bauen: %v\n%s", err, out)
	}
	return tag
}

type env struct {
	m    *sandbox.Manager
	ws   string
	path string
}

func setup(t *testing.T) *env {
	t.Helper()
	img := testImage(t)
	dataDir := t.TempDir()
	m := sandbox.New(sandbox.Options{DataDir: dataDir, Image: img, Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))})
	ws := "t" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", ""))
	if len(ws) > 30 {
		ws = ws[:30]
	}
	path := filepath.Join(t.TempDir(), ws)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := m.Ensure(ctx, sandbox.Spec{Workspace: ws, Path: path, Config: sandbox.Config{Resources: sandbox.Resources{Memory: "512m", CPUs: 1, PIDs: 128}}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Remove(context.Background(), ws) })
	return &env{m: m, ws: ws, path: path}
}

func (e *env) exec(t *testing.T, script string) string {
	t.Helper()
	cmd := e.m.Command(context.Background(), e.ws, sandbox.Exec{Argv: []string{"sh", "-c", script}})
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestContainerIsolation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c, err := e.m.Get(ctx, e.ws)
	if err != nil || c == nil || !c.Running {
		t.Fatalf("Container: %+v, %v", c, err)
	}
	// Genau ein Mount: das Workspace-Verzeichnis.
	if len(c.Mounts) != 1 || c.Mounts[0].Destination != sandbox.Workdir || c.Mounts[0].Source != e.path {
		t.Fatalf("Mounts = %+v", c.Mounts)
	}
	if got := e.exec(t, "id -u"); got != strconv.Itoa(os.Getuid()) || got == "0" {
		t.Errorf("UID im Container = %s", got)
	}
	if got := e.exec(t, "grep CapEff /proc/self/status | tr -d ' \\t'"); got != "CapEff:0000000000000000" {
		t.Errorf("Capabilities: %s", got)
	}
	if got := e.exec(t, "grep NoNewPrivs /proc/self/status | tr -d ' \\t'"); got != "NoNewPrivs:1" {
		t.Errorf("no-new-privileges: %s", got)
	}
	// Weder $HOME des Nutzers noch das Verzeichnis über dem Workspace sind sichtbar.
	home, _ := os.UserHomeDir()
	for _, p := range []string{home, filepath.Dir(e.path), "/var/run/docker.sock"} {
		if out := e.exec(t, "test -e '"+p+"' && echo da || echo fehlt"); out != "fehlt" {
			t.Errorf("%s ist im Container sichtbar", p)
		}
	}
	// Schreiben im Container landet im Workspace.
	e.exec(t, "echo hallo > /workspace/aus-dem-container")
	if data, err := os.ReadFile(filepath.Join(e.path, "aus-dem-container")); err != nil || string(data) != "hallo\n" {
		t.Errorf("Datei im Workspace: %q, %v", data, err)
	}
}

func TestSignalByMarker(t *testing.T) {
	e := setup(t)
	cmd := e.m.Command(context.Background(), e.ws, sandbox.Exec{Argv: []string{"sh", "-c", "sleep 300 & sleep 300; wait"}, Env: []string{"ADE_SESSION_ID=s1"}})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	time.Sleep(500 * time.Millisecond)
	// Eine andere Markierung trifft nichts.
	if err := e.m.Signal(context.Background(), e.ws, "ADE_SESSION_ID=anders", syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("falscher Prozess beendet: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := e.m.Signal(context.Background(), e.ws, "ADE_SESSION_ID=s1", syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Logf("Prozess beendet: %v", err)
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("Prozess im Container läuft nach SIGTERM weiter")
	}
	if out := e.exec(t, "n=0; for d in /proc/[0-9]*; do tr '\\0' ' ' < $d/cmdline 2>/dev/null | grep -q '^sleep 300' && n=$((n+1)); done; echo $n"); out != "0" {
		t.Errorf("%s Prozesse der Session laufen noch", out)
	}
}

// Issue #41: Ein vom Agenten im Workspace konfigurierter Filter darf beim WIP-Commit des
// Daemons nicht auf dem Host laufen.
func TestWorkspaceGitConfigDoesNotRunOnHost(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "filter-lief-auf-dem-host")
	run := func(args ...string) {
		t.Helper()
		if _, err := git.Run(ctx, git.Cmd{Dir: e.path, Args: args}); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "--quiet", "-b", "main")
	run("-c", "user.name=T", "-c", "user.email=t@t", "commit", "--quiet", "--allow-empty", "-m", "init")
	// Das tut ein Agent im Workspace: Filter, fsmonitor und Hook, die eine Datei auf dem Host anlegen.
	evil := "touch " + marker + "; cat"
	run("config", "filter.evil.clean", evil)
	run("config", "filter.evil.smudge", evil)
	run("config", "core.fsmonitor", "touch "+marker)
	if err := os.WriteFile(filepath.Join(e.path, ".gitattributes"), []byte("* filter=evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.path, "datei.txt"), []byte("inhalt\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	wt := git.Worktree{Path: e.path, Runner: func(ctx context.Context, env, argv []string) *exec.Cmd {
		return e.m.Command(ctx, e.ws, sandbox.Exec{Argv: argv, Env: env})
	}}
	d, err := git.WorktreeDiff(ctx, wt, "HEAD", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != 2 {
		t.Errorf("Diff: %+v", d.Files)
	}
	if _, ok, err := git.WorktreeBlob(ctx, wt, "datei.txt"); err != nil || !ok {
		t.Errorf("WorktreeBlob: %v %v", ok, err)
	}
	committed, err := git.CommitAll(ctx, wt, "ade: wip test")
	if err != nil || !committed {
		t.Fatalf("CommitAll: %v %v", committed, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ein Befehl aus der Workspace-Konfiguration lief auf dem Host")
	}
	if out, _ := git.Output(ctx, e.path, "log", "--format=%s", "-1"); out != "ade: wip test" {
		t.Errorf("letzter Commit: %q", out)
	}
}

func TestGCAndRecover(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	known := func(ws string) bool { return ws == e.ws }
	orphans, err := e.m.GC(ctx, known, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range orphans {
		if c.Workspace == e.ws {
			t.Fatal("Container eines bekannten Workspaces gilt als verwaist")
		}
	}
	if err := e.m.Recover(ctx, known); err != nil {
		t.Fatal(err)
	}
	if c, _ := e.m.Get(ctx, e.ws); c == nil || c.Running {
		t.Fatalf("nach Recover: %+v (erwartet: vorhanden, gestoppt)", c)
	}
	removed, err := e.m.GC(ctx, func(string) bool { return false }, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0].Workspace != e.ws {
		t.Fatalf("entfernt: %+v", removed)
	}
	if c, _ := e.m.Get(ctx, e.ws); c != nil {
		t.Fatal("verwaister Container existiert noch")
	}
}
