package sandbox_test

// Integrationstests gegen einen echten Docker-Daemon. Sie laufen nur mit ADE_TEST_DOCKER=1
// (CI setzt das). ADE_TEST_IMAGE wählt ein Image mit git, curl, getent und busybox; ohne
// Angabe baut der Test ein kleines Debian-Image damit. Für den Egress-Proxy baut der Test
// das ade-Binary (statisch). Die Netzwerk-Tests brauchen Internet.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/git"
	"github.com/DavidW475/ade/internal/proxy"
	"github.com/DavidW475/ade/internal/sandbox"
	"github.com/DavidW475/ade/internal/sandbox/sandboxtest"
)

func testImage(t *testing.T) string { return sandboxtest.Image(t) }

func proxyBinary(t *testing.T) string { return sandboxtest.ProxyBinary(t) }

type env struct {
	m    *sandbox.Manager
	ws   string
	path string
	spec sandbox.Spec

	mu     sync.Mutex
	egress []proxy.Event
}

func (e *env) events() []proxy.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]proxy.Event(nil), e.egress...)
}

func setup(t *testing.T, cfg ...sandbox.Config) *env {
	t.Helper()
	img := testImage(t)
	dataDir := t.TempDir()
	e := &env{}
	m := sandbox.New(sandbox.Options{
		DataDir: dataDir, Image: img, ProxyBinary: proxyBinary(t),
		Logger:  slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Secrets: func() map[string]string { return map[string]string{"anthropic": testSecret} },
		OnEgress: func(_ string, evs []proxy.Event) {
			e.mu.Lock()
			e.egress = append(e.egress, evs...)
			e.mu.Unlock()
		},
	})
	t.Cleanup(m.Close)
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
	c := sandbox.Config{Resources: sandbox.Resources{Memory: "512m", CPUs: 1, PIDs: 128}}
	if len(cfg) > 0 {
		c = cfg[0]
	}
	spec := sandbox.Spec{Workspace: ws, Path: path, Config: c}
	if err := m.Ensure(ctx, spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Remove(context.Background(), ws) })
	e.m, e.ws, e.path, e.spec = m, ws, path, spec
	return e
}

const testSecret = "sk-ant-test-darf-nie-im-container-stehen"

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
	// Nur das Workspace-Verzeichnis (beschreibbar) und das ade-Binary für den Freigabe-Hook
	// (nur lesend).
	var workspace, hook bool
	for _, mt := range c.Mounts {
		switch {
		case mt.Destination == sandbox.Workdir && mt.Source == e.path && mt.RW:
			workspace = true
		case mt.Destination == sandbox.ProxyBinary && !mt.RW:
			hook = true
		default:
			t.Errorf("unerwarteter Mount %+v", mt)
		}
	}
	if !workspace || !hook || len(c.Mounts) != 2 {
		t.Fatalf("Mounts = %+v", c.Mounts)
	}
	if got := e.exec(t, "test -w "+sandbox.ProxyBinary+" && echo schreibbar || echo nur-lesend"); got != "nur-lesend" {
		t.Errorf("Hook-Binary: %s", got)
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

// Lese-Rollen einer Fabrik (M9): /workspace nur lesend, gleiches Netz und gleicher Proxy.
func TestReaderContainer(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(e.path, "plan.txt"), []byte("lesbar\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.m.EnsureReader(ctx, e.spec); err != nil {
		t.Fatal(err)
	}
	c, err := e.m.GetReader(ctx, e.ws)
	if err != nil || c == nil || !c.Running || c.Role != sandbox.RoleReader {
		t.Fatalf("Lese-Container: %+v, %v", c, err)
	}
	for _, mt := range c.Mounts {
		if mt.Destination == sandbox.Workdir && mt.RW {
			t.Fatalf("/workspace ist beschreibbar gemountet: %+v", mt)
		}
	}
	run := func(script string) (string, error) {
		out, err := e.m.Command(ctx, e.ws, sandbox.Exec{Argv: []string{"sh", "-c", script}, Reader: true}).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := run("cat /workspace/plan.txt"); err != nil || out != "lesbar" {
		t.Fatalf("Lesen: %q, %v", out, err)
	}
	for _, script := range []string{"echo x > /workspace/neu", "rm /workspace/plan.txt", "mkdir /workspace/d", "touch /workspace/plan.txt"} {
		if out, err := run(script); err == nil {
			t.Errorf("%s gelang im Lese-Container: %s", script, out)
		}
	}
	if _, err := os.Stat(filepath.Join(e.path, "neu")); err == nil {
		t.Fatal("Datei aus dem Lese-Container im Workspace")
	}
	// Außerhalb von /workspace bleibt das Dateisystem des Containers beschreibbar (HOME der
	// CLI im echten Image), der Proxy ist erreichbar.
	if out, err := run("echo ok > /tmp/x && cat /tmp/x"); err != nil || out != "ok" {
		t.Errorf("/tmp: %q, %v", out, err)
	}
	if out, err := run("getent hosts proxy >/dev/null && echo da"); err != nil || out != "da" {
		t.Errorf("Proxy im Netz: %q, %v", out, err)
	}
	// Der Agent-Container schreibt weiter.
	e.exec(t, "echo y > /workspace/vom-agenten")
	if out, err := run("cat /workspace/vom-agenten"); err != nil || out != "y" {
		t.Errorf("Änderung des Agenten im Lese-Container: %q, %v", out, err)
	}
	// Ein zweites EnsureReader nimmt den bestehenden Container; Remove entfernt ihn.
	if err := e.m.EnsureReader(ctx, e.spec); err != nil {
		t.Fatal(err)
	}
	if c2, _ := e.m.GetReader(ctx, e.ws); c2 == nil || c2.ID != c.ID {
		t.Fatalf("Lese-Container neu angelegt: %+v", c2)
	}
	if err := e.m.Remove(ctx, e.ws); err != nil {
		t.Fatal(err)
	}
	if c3, err := e.m.GetReader(ctx, e.ws); err != nil || c3 != nil {
		t.Fatalf("Lese-Container nach Remove: %+v, %v", c3, err)
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
	// Agent- und Proxy-Container.
	if len(removed) != 2 || removed[0].Workspace != e.ws || removed[1].Workspace != e.ws {
		t.Fatalf("entfernt: %+v", removed)
	}
	if c, _ := e.m.Get(ctx, e.ws); c != nil {
		t.Fatal("verwaister Container existiert noch")
	}
	if exec.Command("docker", "network", "inspect", sandbox.NetworkName(e.ws)).Run() == nil {
		t.Fatal("Netz des verwaisten Workspaces existiert noch")
	}
}

// curl im Container: liefert den HTTP-Status (000 bei Verbindungsfehler).
func (e *env) curl(t *testing.T, args ...string) string {
	t.Helper()
	argv := append([]string{"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "15"}, args...)
	out, _ := e.m.Command(context.Background(), e.ws, sandbox.Exec{Argv: argv}).Output()
	return strings.TrimSpace(string(out))
}

// M6: Nur freigegebene Ziele sind erreichbar, und nur über den Proxy.
func TestEgressPolicy(t *testing.T) {
	cfg, err := sandbox.ParseConfig([]byte("resources:\n  memory: 512m\n  cpus: 1\n  pids: 128\negress:\n  presets: [npm]\n"))
	if err != nil {
		t.Fatal(err)
	}
	e := setup(t, *cfg)
	ctx := context.Background()

	if got := e.curl(t, "https://registry.npmjs.org/"); got != "200" {
		t.Errorf("registry.npmjs.org (Preset npm): %s", got)
	}
	// Geblockt: CONNECT wird mit 403 abgelehnt, curl meldet 403 bzw. 000.
	if got := e.curl(t, "https://example.com/"); got == "200" {
		t.Errorf("example.com erreichbar: %s", got)
	}
	if got := e.curl(t, "http://example.com/"); got != "403" {
		t.Errorf("http://example.com: %s, erwartet 403", got)
	}
	// Ein Programm, das den Proxy ignoriert, kommt nirgendwo hin – auch nicht per IP.
	for _, target := range []string{"https://registry.npmjs.org/", "https://1.1.1.1/"} {
		if got := e.curl(t, "--noproxy", "*", target); got != "000" {
			t.Errorf("ohne Proxy %s: %s", target, got)
		}
	}
	// DNS: externe Namen lösen nicht auf, der Proxy schon.
	if out, err := e.m.Command(ctx, e.ws, sandbox.Exec{Argv: []string{"getent", "hosts", "example.com"}}).CombinedOutput(); err == nil {
		t.Errorf("externer Name aufgelöst: %s", out)
	}
	if got := e.exec(t, "getent hosts proxy | wc -l"); got != "1" {
		t.Errorf("proxy nicht auflösbar")
	}
	// Die Steuer-API des Proxys ist ohne Token zu.
	if got := e.curl(t, "--noproxy", "*", "http://proxy:8081/healthz"); got != "401" {
		t.Errorf("Steuer-API ohne Token: %s", got)
	}

	// Events kommen beim Daemon an.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var allowed, blocked bool
		for _, ev := range e.events() {
			allowed = allowed || ev.Allowed && ev.Host == "registry.npmjs.org"
			blocked = blocked || !ev.Allowed && ev.Host == "example.com"
		}
		if allowed && blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Events: %+v", e.events())
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Hot-Reload: eine geänderte Policy wirkt ohne Neustart.
	next, err := sandbox.ParseConfig([]byte("egress:\n  presets: [npm]\n  allow: [example.com]\n"))
	if err != nil {
		t.Fatal(err)
	}
	e.m.SetSpecSource(func(context.Context, string) (sandbox.Spec, error) {
		s := e.spec
		s.Config = *next
		return s, nil
	})
	e.m.ReloadPolicies(ctx)
	if got := e.curl(t, "https://example.com/"); got != "200" {
		t.Errorf("example.com nach Hot-Reload: %s", got)
	}
}

// M6: Der API-Key steht weder in der Konfiguration noch in der Umgebung der Container.
func TestSecretsStayOutOfContainers(t *testing.T) {
	e := setup(t)
	for _, name := range []string{sandbox.ContainerName(e.ws), sandbox.ProxyName(e.ws)} {
		out, err := exec.Command("docker", "container", "inspect", name).CombinedOutput()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), testSecret) {
			t.Errorf("Key in docker inspect %s", name)
		}
	}
	if out := e.exec(t, "env; cat /proc/1/environ 2>/dev/null; grep -rs '"+testSecret+"' /home /workspace /tmp || true"); strings.Contains(out, testSecret) {
		t.Error("Key im Agent-Container lesbar")
	}
}

// M6: Die IDE ist über die Weiterleitung des Proxys erreichbar.
func TestIDEForward(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.exec(t, "echo ide-ok > /workspace/index.html")
	cmd := e.m.Command(ctx, e.ws, sandbox.Exec{Argv: []string{"busybox", "httpd", "-f", "-p", "3000", "-h", "/workspace"}, Env: []string{"ADE_IDE_OWNER=test"}})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = e.m.Signal(ctx, e.ws, "ADE_IDE_OWNER=test", syscall.SIGKILL)
		_ = cmd.Wait()
	}()
	port, err := e.m.IDEHostPort(ctx, e.ws)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/index.html")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if strings.TrimSpace(string(body)) == "ide-ok" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("IDE-Weiterleitung: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Container aus M5 (normales Netz, ohne Proxy) werden beim nächsten Start neu angelegt.
func TestRecreatesM5Container(t *testing.T) {
	img := testImage(t)
	e := setup(t)
	ctx := context.Background()
	if err := e.m.Remove(ctx, e.ws); err != nil {
		t.Fatal(err)
	}
	// So sah ein Container aus M5 aus.
	c, _ := e.m.Get(ctx, e.ws)
	if c != nil {
		t.Fatal("Container nach Remove noch da")
	}
	dataDir := ""
	for _, kv := range e.m.ProxyCreateArgs(e.ws) {
		if v, ok := strings.CutPrefix(kv, sandbox.LabelDataDir+"="); ok {
			dataDir = v
		}
	}
	out, err := exec.Command("docker", "container", "create", "--name", sandbox.ContainerName(e.ws),
		"--label", sandbox.LabelManaged+"=true", "--label", sandbox.LabelWorkspace+"="+e.ws, "--label", sandbox.LabelDataDir+"="+dataDir,
		"--entrypoint", "sleep", img, "infinity").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if c, err := e.m.Get(ctx, e.ws); err != nil || c == nil || c.Net != "" {
		t.Fatalf("M5-Container: %+v %v", c, err)
	}
	if err := e.m.Ensure(ctx, e.spec); err != nil {
		t.Fatal(err)
	}
	if c, err := e.m.Get(ctx, e.ws); err != nil || c == nil || c.Net != sandbox.NetVersion || !c.Running {
		t.Fatalf("nach Ensure: %+v %v", c, err)
	}
	if got := e.curl(t, "--noproxy", "*", "https://1.1.1.1/"); got != "000" {
		t.Errorf("neu angelegter Container hat Netz: %s", got)
	}
}
