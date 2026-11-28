package sandbox_test

// Integration tests against a real Docker daemon. They only run with ADE_TEST_DOCKER=1
// (CI sets it). ADE_TEST_IMAGE selects an image with git, curl, getent and busybox; without
// it the test builds a small Debian image with them. For the egress proxy the test builds
// the ade binary (statically). The network tests need internet.

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
		Docker:  sandboxtest.Docker(),
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

const testSecret = "sk-ant-test-must-never-be-in-the-container"

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
	// Only the workspace directory (writable) and the ade binary for the approval hook
	// (read-only).
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
	if got := e.exec(t, "test -w "+sandbox.ProxyBinary+" && echo writable || echo read-only"); got != "read-only" {
		t.Errorf("Hook-Binary: %s", got)
	}
	// UID in the container: the user's, only under rootless Docker root (= the user on the host).
	rt, _ := e.m.Runtime()
	wantUID := strconv.Itoa(os.Getuid())
	if rt.RootInContainer() {
		wantUID = "0"
	}
	if got := e.exec(t, "id -u"); got != wantUID || (got == "0" && !rt.RootInContainer()) {
		t.Errorf("UID in the container (%s) = %s, want %s", rt.Name(), got, wantUID)
	}
	if got := e.exec(t, "grep CapEff /proc/self/status | tr -d ' \\t'"); got != "CapEff:0000000000000000" {
		t.Errorf("Capabilities: %s", got)
	}
	if got := e.exec(t, "grep NoNewPrivs /proc/self/status | tr -d ' \\t'"); got != "NoNewPrivs:1" {
		t.Errorf("no-new-privileges: %s", got)
	}
	// Neither the user's $HOME nor the directory above the workspace are visible.
	home, _ := os.UserHomeDir()
	for _, p := range []string{home, filepath.Dir(e.path), "/var/run/docker.sock"} {
		if out := e.exec(t, "test -e '"+p+"' && echo present || echo missing"); out != "missing" {
			t.Errorf("%s is visible in the container", p)
		}
	}
	// Writing in the container ends up in the workspace.
	e.exec(t, "echo hello > /workspace/from-the-container")
	if data, err := os.ReadFile(filepath.Join(e.path, "from-the-container")); err != nil || string(data) != "hello\n" {
		t.Errorf("file in the workspace: %q, %v", data, err)
	}
	// #94: files and directories from the container belong to the user on the host.
	e.exec(t, "mkdir -p /workspace/new/deep && echo x > /workspace/new/deep/file")
	for _, p := range []string{"from-the-container", "new", "new/deep/file"} {
		fi, err := os.Stat(filepath.Join(e.path, p))
		if err != nil {
			t.Fatal(err)
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
			t.Errorf("%s belongs to UID %d on the host, want %d (%s)", p, st.Uid, os.Getuid(), rt.Name())
		}
	}
}

// Read-only roles of a factory (M9): /workspace read-only, same network and same proxy.
func TestReaderContainer(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(e.path, "plan.txt"), []byte("readable\n"), 0o600); err != nil {
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
			t.Fatalf("/workspace is mounted writable: %+v", mt)
		}
	}
	run := func(script string) (string, error) {
		out, err := e.m.Command(ctx, e.ws, sandbox.Exec{Argv: []string{"sh", "-c", script}, Reader: true}).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := run("cat /workspace/plan.txt"); err != nil || out != "readable" {
		t.Fatalf("reading: %q, %v", out, err)
	}
	for _, script := range []string{"echo x > /workspace/new", "rm /workspace/plan.txt", "mkdir /workspace/d", "touch /workspace/plan.txt"} {
		if out, err := run(script); err == nil {
			t.Errorf("%s succeeded in the reader container: %s", script, out)
		}
	}
	if _, err := os.Stat(filepath.Join(e.path, "new")); err == nil {
		t.Fatal("file from the reader container in the workspace")
	}
	// Outside /workspace the container's file system stays writable (HOME of the CLI in the
	// real image), the proxy is reachable.
	if out, err := run("echo ok > /tmp/x && cat /tmp/x"); err != nil || out != "ok" {
		t.Errorf("/tmp: %q, %v", out, err)
	}
	if out, err := run("getent hosts proxy >/dev/null && echo da"); err != nil || out != "da" {
		t.Errorf("proxy in the network: %q, %v", out, err)
	}
	// The agent container keeps writing.
	e.exec(t, "echo y > /workspace/from-the-agent")
	if out, err := run("cat /workspace/from-the-agent"); err != nil || out != "y" {
		t.Errorf("the agent's change in the reader container: %q, %v", out, err)
	}
	// A second EnsureReader takes the existing container; Remove removes it.
	if err := e.m.EnsureReader(ctx, e.spec); err != nil {
		t.Fatal(err)
	}
	if c2, _ := e.m.GetReader(ctx, e.ws); c2 == nil || c2.ID != c.ID {
		t.Fatalf("reader container recreated: %+v", c2)
	}
	if err := e.m.Remove(ctx, e.ws); err != nil {
		t.Fatal(err)
	}
	if c3, err := e.m.GetReader(ctx, e.ws); err != nil || c3 != nil {
		t.Fatalf("reader container after Remove: %+v, %v", c3, err)
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
	// Another marker hits nothing.
	if err := e.m.Signal(context.Background(), e.ws, "ADE_SESSION_ID=anders", syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("wrong process ended: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := e.m.Signal(context.Background(), e.ws, "ADE_SESSION_ID=s1", syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Logf("process ended: %v", err)
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("process in the container keeps running after SIGTERM")
	}
	if out := e.exec(t, "n=0; for d in /proc/[0-9]*; do tr '\\0' ' ' < $d/cmdline 2>/dev/null | grep -q '^sleep 300' && n=$((n+1)); done; echo $n"); out != "0" {
		t.Errorf("%s processes of the session are still running", out)
	}
}

// Issue #41: a filter the agent configured in the workspace must not run on the host during
// the daemon's WIP commit.
func TestWorkspaceGitConfigDoesNotRunOnHost(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "filter-ran-on-the-host")
	run := func(args ...string) {
		t.Helper()
		if _, err := git.Run(ctx, git.Cmd{Dir: e.path, Args: args}); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "--quiet", "-b", "main")
	run("-c", "user.name=T", "-c", "user.email=t@t", "commit", "--quiet", "--allow-empty", "-m", "init")
	// This is what an agent does in the workspace: filter, fsmonitor and hook that create a file on the host.
	evil := "touch " + marker + "; cat"
	run("config", "filter.evil.clean", evil)
	run("config", "filter.evil.smudge", evil)
	run("config", "core.fsmonitor", "touch "+marker)
	if err := os.WriteFile(filepath.Join(e.path, ".gitattributes"), []byte("* filter=evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.path, "file.txt"), []byte("content\n"), 0o600); err != nil {
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
	if _, ok, err := git.WorktreeBlob(ctx, wt, "file.txt"); err != nil || !ok {
		t.Errorf("WorktreeBlob: %v %v", ok, err)
	}
	committed, err := git.CommitAll(ctx, wt, "ade: wip test")
	if err != nil || !committed {
		t.Fatalf("CommitAll: %v %v", committed, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a command from the workspace configuration ran on the host")
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
			t.Fatal("container of a known workspace counts as orphaned")
		}
	}
	if err := e.m.Recover(ctx, known); err != nil {
		t.Fatal(err)
	}
	if c, _ := e.m.Get(ctx, e.ws); c == nil || c.Running {
		t.Fatalf("after Recover: %+v (want: present, stopped)", c)
	}
	removed, err := e.m.GC(ctx, func(string) bool { return false }, false)
	if err != nil {
		t.Fatal(err)
	}
	// Agent and proxy container.
	if len(removed) != 2 || removed[0].Workspace != e.ws || removed[1].Workspace != e.ws {
		t.Fatalf("entfernt: %+v", removed)
	}
	if c, _ := e.m.Get(ctx, e.ws); c != nil {
		t.Fatal("orphaned container still exists")
	}
	if exec.Command(sandboxtest.Docker(), "network", "inspect", sandbox.NetworkName(e.ws)).Run() == nil {
		t.Fatal("network of the orphaned workspace still exists")
	}
}

// curl in the container: returns the HTTP status (000 on a connection error).
func (e *env) curl(t *testing.T, args ...string) string {
	t.Helper()
	argv := append([]string{"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "15"}, args...)
	out, _ := e.m.Command(context.Background(), e.ws, sandbox.Exec{Argv: argv}).Output()
	return strings.TrimSpace(string(out))
}

// M6: only allowed targets are reachable, and only through the proxy.
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
	// Blocked: CONNECT is rejected with 403, curl reports 403 or 000.
	if got := e.curl(t, "https://example.com/"); got == "200" {
		t.Errorf("example.com erreichbar: %s", got)
	}
	if got := e.curl(t, "http://example.com/"); got != "403" {
		t.Errorf("http://example.com: %s, want 403", got)
	}
	// A program that ignores the proxy gets nowhere – not even by IP.
	for _, target := range []string{"https://registry.npmjs.org/", "https://1.1.1.1/"} {
		if got := e.curl(t, "--noproxy", "*", target); got != "000" {
			t.Errorf("without proxy %s: %s", target, got)
		}
	}
	// DNS: external names do not resolve, the proxy does.
	if out, err := e.m.Command(ctx, e.ws, sandbox.Exec{Argv: []string{"getent", "hosts", "example.com"}}).CombinedOutput(); err == nil {
		t.Errorf("external name resolved: %s", out)
	}
	// Podman returns one line per address family or alias, Docker one.
	if got := e.exec(t, "getent hosts proxy || true"); !strings.Contains(got, "proxy") {
		t.Errorf("proxy not resolvable: %q", got)
	} else {
		t.Logf("getent hosts proxy: %q", got)
	}
	// The proxy's control API is closed without a token.
	if got := e.curl(t, "--noproxy", "*", "http://proxy:8081/healthz"); got != "401" {
		t.Errorf("control API without token: %s", got)
	}

	// Events reach the daemon.
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

	// Hot reload: a changed policy takes effect without a restart.
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
		t.Errorf("example.com after hot reload: %s", got)
	}
}

// M6: the API key is neither in the configuration nor in the environment of the containers.
func TestSecretsStayOutOfContainers(t *testing.T) {
	e := setup(t)
	for _, name := range []string{sandbox.ContainerName(e.ws), sandbox.ProxyName(e.ws)} {
		out, err := exec.Command(sandboxtest.Docker(), "container", "inspect", name).CombinedOutput()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), testSecret) {
			t.Errorf("Key in docker inspect %s", name)
		}
	}
	if out := e.exec(t, "env; cat /proc/1/environ 2>/dev/null; grep -rs '"+testSecret+"' /home /workspace /tmp || true"); strings.Contains(out, testSecret) {
		t.Error("key readable in the agent container")
	}
}

// M6: the IDE is reachable through the proxy's forwarding.
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
			t.Fatalf("IDE forwarding: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Containers from M5 (normal network, without proxy) are recreated on the next start.
func TestRecreatesM5Container(t *testing.T) {
	img := testImage(t)
	e := setup(t)
	ctx := context.Background()
	if err := e.m.Remove(ctx, e.ws); err != nil {
		t.Fatal(err)
	}
	// This is what a container from M5 looked like.
	c, _ := e.m.Get(ctx, e.ws)
	if c != nil {
		t.Fatal("container still there after Remove")
	}
	dataDir := ""
	for _, kv := range e.m.ProxyCreateArgs(e.ws) {
		if v, ok := strings.CutPrefix(kv, sandbox.LabelDataDir+"="); ok {
			dataDir = v
		}
	}
	out, err := exec.Command(sandboxtest.Docker(), "container", "create", "--name", sandbox.ContainerName(e.ws),
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
		t.Fatalf("after Ensure: %+v %v", c, err)
	}
	if got := e.curl(t, "--noproxy", "*", "https://1.1.1.1/"); got != "000" {
		t.Errorf("recreated container has a network: %s", got)
	}
}
