package ide

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/apperr"
)

// The test binary plays openvscode-server if ADE_FAKE_IDE is set. It checks the connection
// token like the original (cookie vscode-tkn) and reports the arguments.
func TestMain(m *testing.M) {
	if mode := os.Getenv("ADE_FAKE_IDE"); mode != "" {
		fakeServer(mode)
		return
	}
	os.Exit(m.Run())
}

func fakeServer(mode string) {
	fs := flag.NewFlagSet("fake", flag.ExitOnError)
	host := fs.String("host", "", "")
	port := fs.String("port", "", "")
	tokenFile := fs.String("connection-token-file", "", "")
	folder := fs.String("default-folder", "", "")
	for _, f := range []string{"server-data-dir", "user-data-dir", "extensions-dir", "telemetry-level"} {
		fs.String(f, "", "")
	}
	_ = fs.Parse(os.Args[1:])
	if mode == "crash" {
		fmt.Fprintln(os.Stderr, "kaputt")
		os.Exit(3)
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		os.Exit(4)
	}
	if mode == "slow" {
		time.Sleep(time.Hour)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("vscode-tkn")
		if err != nil || c.Value != string(token) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, _ = fmt.Fprintf(w, "folder=%s env=%s secret=%s", *folder, os.Getenv(EnvWorkspaceID), os.Getenv("GH_TOKEN"))
	})
	ln, err := net.Listen("tcp", net.JoinHostPort(*host, *port))
	if err != nil {
		os.Exit(5)
	}
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM)
		<-ch
		os.Exit(0)
	}()
	_ = http.Serve(ln, mux) //nolint:gosec // Test
}

type fixture struct {
	m       *Manager
	workdir string
}

func newFixture(t *testing.T, mode string, mutate ...func(*Options)) *fixture {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	opts := Options{
		Dir:          t.TempDir(),
		Binary:       exe,
		StartTimeout: 10 * time.Second,
		StopTimeout:  2 * time.Second,
		Env:          []string{"PATH=" + os.Getenv("PATH"), "GH_TOKEN=secret", "ADE_FAKE_IDE=filtered"},
		ExtraEnv:     []string{"ADE_FAKE_IDE=" + mode},
		Workspaces: func(_ context.Context, id string) (string, error) {
			if id == "missing" {
				return "", apperr.NotFound("workspace %s not found", id)
			}
			return workdir, nil
		},
	}
	for _, f := range mutate {
		f(&opts)
	}
	m := NewManager(opts)
	t.Cleanup(m.Close)
	return &fixture{m: m, workdir: workdir}
}

func waitState(t *testing.T, m *Manager, id, want string) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := m.Status(id)
		if st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("Status = %+v, want %s", st, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func get(t *testing.T, up *Upstream, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, up.URL.String()+"/", nil)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: "vscode-tkn", Value: token})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestStartAcquireStop(t *testing.T) {
	f := newFixture(t, "ok")
	ctx := context.Background()

	if st := f.m.Status("ws1"); st.State != StateStopped {
		t.Fatalf("status before start = %+v", st)
	}
	if _, err := f.m.Start(ctx, "ws1"); err != nil {
		t.Fatal(err)
	}
	waitState(t, f.m, "ws1", StateRunning)

	up, err := f.m.Acquire(ctx, "ws1")
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := get(t, up, ""); code != http.StatusForbidden {
		t.Errorf("without token: %d, want 403", code)
	}
	code, body := get(t, up, up.Token)
	if code != http.StatusOK {
		t.Fatalf("with token: %d %s", code, body)
	}
	if want := "folder=" + f.workdir + " env=ws1 secret="; body != want {
		t.Errorf("answer = %q, want %q (working folder, workspace ID, no GH_TOKEN)", body, want)
	}
	if st := f.m.Status("ws1"); st.Connections != 1 {
		t.Errorf("Connections = %d, want 1", st.Connections)
	}
	up.Release()
	up.Release() // twice is harmless
	if st := f.m.Status("ws1"); st.Connections != 0 {
		t.Errorf("Connections after Release = %d", st.Connections)
	}

	// The token is only in a file, not on the command line.
	data, err := os.ReadFile(filepath.Join(f.m.dataDir("ws1"), "connection-token"))
	if err != nil || string(data) != up.Token {
		t.Errorf("token file = %q, %v", data, err)
	}

	f.m.Stop("ws1")
	if st := f.m.Status("ws1"); st.State != StateStopped {
		t.Errorf("status after Stop = %+v", st)
	}
	if code, _, err := tryGet(up); err == nil {
		t.Errorf("IDE still answers after Stop (%d)", code)
	}
}

func tryGet(up *Upstream) (int, string, error) {
	c := &http.Client{Timeout: time.Second}
	resp, err := c.Get(up.URL.String() + "/")
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, "", nil
}

func TestAcquireStartsIDE(t *testing.T) {
	f := newFixture(t, "ok")
	up, err := f.m.Acquire(context.Background(), "ws1")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Release()
	if code, body := get(t, up, up.Token); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
}

func TestStartIsIdempotent(t *testing.T) {
	f := newFixture(t, "ok")
	ctx := context.Background()
	if _, err := f.m.Start(ctx, "ws1"); err != nil {
		t.Fatal(err)
	}
	a, err := f.m.Acquire(ctx, "ws1")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	if _, err := f.m.Start(ctx, "ws1"); err != nil {
		t.Fatal(err)
	}
	b, err := f.m.Acquire(ctx, "ws1")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release()
	if a.URL.String() != b.URL.String() || a.Token != b.Token {
		t.Error("second start started a new IDE")
	}
}

func TestUnknownWorkspace(t *testing.T) {
	f := newFixture(t, "ok")
	if _, err := f.m.Start(context.Background(), "missing"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("err = %v, want NotFound", err)
	}
}

func TestCrashReportsError(t *testing.T) {
	f := newFixture(t, "crash")
	if _, err := f.m.Acquire(context.Background(), "ws1"); !errors.Is(err, apperr.ErrConflict) {
		t.Fatalf("Acquire err = %v, want Conflict", err)
	}
	st := waitState(t, f.m, "ws1", StateError)
	if !strings.Contains(st.Error, "kaputt") {
		t.Errorf("error = %q, want the server's output", st.Error)
	}
}

func TestStartTimeout(t *testing.T) {
	f := newFixture(t, "slow", func(o *Options) { o.StartTimeout = 300 * time.Millisecond })
	if _, err := f.m.Start(context.Background(), "ws1"); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, f.m, "ws1", StateError)
	if !strings.Contains(st.Error, "does not answer after") {
		t.Errorf("Fehler = %q", st.Error)
	}
}

func TestStopDuringStart(t *testing.T) {
	f := newFixture(t, "slow")
	if _, err := f.m.Start(context.Background(), "ws1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	done := make(chan struct{})
	go func() { f.m.Stop("ws1"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hangs")
	}
	if st := f.m.Status("ws1"); st.State != StateStopped {
		t.Errorf("Status = %+v", st)
	}
}

func TestIdleStop(t *testing.T) {
	f := newFixture(t, "ok", func(o *Options) { o.IdleTimeout = 200 * time.Millisecond })
	ctx := context.Background()
	up, err := f.m.Acquire(ctx, "ws1")
	if err != nil {
		t.Fatal(err)
	}
	// With an open connection the IDE stays on.
	time.Sleep(500 * time.Millisecond)
	if st := f.m.Status("ws1"); st.State != StateRunning {
		t.Fatalf("IDE with an open connection stopped: %+v", st)
	}
	up.Release()
	waitState(t, f.m, "ws1", StateStopped)
}

func TestBeforeWorkspaceDeleteRemovesData(t *testing.T) {
	f := newFixture(t, "ok")
	if _, err := f.m.Acquire(context.Background(), "ws1"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.BeforeWorkspaceDelete(context.Background(), "ws1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.m.dataDir("ws1")); !os.IsNotExist(err) {
		t.Errorf("data directory still exists: %v", err)
	}
	if st := f.m.Status("ws1"); st.State != StateStopped {
		t.Errorf("Status = %+v", st)
	}
}

func TestMissingBinary(t *testing.T) {
	f := newFixture(t, "ok", func(o *Options) { o.Binary = "/does/not/exist" })
	if _, err := f.m.Start(context.Background(), "ws1"); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, f.m, "ws1", StateError)
	if !strings.Contains(st.Error, "not found") {
		t.Errorf("Fehler = %q", st.Error)
	}
}

func TestKillOwned(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("sleep", "30")
	cmd.Env = []string{EnvOwner + "=" + dir}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	other := exec.Command("sleep", "30")
	other.Env = []string{EnvOwner + "=" + dir + "-anderer"}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })

	if n := killOwned(dir); n != 1 {
		t.Errorf("killOwned = %d, want 1", n)
	}
	if err := cmd.Wait(); err == nil {
		t.Error("own process kept running")
	}
	if err := other.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("foreign process was ended: %v", err)
	}
}

// fakeSandbox plays the container: paths under /home/ade end up in dir, the port is rewritten
// to a free port, Signal hits the started processes.
type fakeSandbox struct {
	exe, dir string
	port     int

	mu      sync.Mutex
	procs   []*exec.Cmd
	signals []string
}

func (f *fakeSandbox) path(p string) string {
	if rest, ok := strings.CutPrefix(p, "/home/ade/"); ok {
		return filepath.Join(f.dir, rest)
	}
	return p
}

func (f *fakeSandbox) Command(ctx context.Context, argv, env []string) *exec.Cmd {
	args := append([]string(nil), argv[1:]...)
	for i := range args {
		switch {
		case i > 0 && args[i-1] == "--port":
			args[i] = fmt.Sprint(f.port)
		case i > 0 && args[i-1] == "--host":
			args[i] = "127.0.0.1"
		default:
			args[i] = f.path(args[i])
		}
	}
	cmd := exec.CommandContext(ctx, f.exe, args...)
	cmd.Env = append([]string{"ADE_FAKE_IDE=ok", "PATH=" + os.Getenv("PATH")}, env...)
	f.mu.Lock()
	f.procs = append(f.procs, cmd)
	f.mu.Unlock()
	return cmd
}

func (f *fakeSandbox) Signal(_ context.Context, marker string, sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, marker+" "+sig.String())
	for _, c := range f.procs {
		if c.Process != nil && slices.Contains(c.Env, marker) {
			_ = c.Process.Signal(sig)
		}
	}
	return nil
}

func (f *fakeSandbox) HostPort(context.Context, int) (int, error) { return f.port, nil }

func (f *fakeSandbox) WriteFile(_ context.Context, p string, data []byte) error {
	p = f.path(p)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

func TestSandboxIDE(t *testing.T) {
	exe, _ := os.Executable()
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	sb := &fakeSandbox{exe: exe, dir: t.TempDir(), port: port}
	f := newFixture(t, "ok", func(o *Options) {
		o.Binary = "/does/not/exist" // must not be needed in sandbox mode
		o.Sandbox = func(_ context.Context, id string) (Sandbox, error) {
			if id == "lokal" {
				return nil, nil
			}
			return sb, nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	up, err := f.m.Acquire(ctx, "box")
	if err != nil {
		t.Fatal(err)
	}
	if up.URL.Host != fmt.Sprintf("127.0.0.1:%d", port) {
		t.Errorf("Upstream = %s", up.URL)
	}
	code, body := get(t, up, up.Token)
	if code != http.StatusOK || !strings.Contains(body, "folder=/workspace") || !strings.Contains(body, "env=box") {
		t.Errorf("Antwort %d %q", code, body)
	}
	if code, _ := get(t, up, "falsch"); code != http.StatusForbidden {
		t.Errorf("wrong token: %d", code)
	}
	up.Release()
	f.m.Stop("box")
	if st := f.m.Status("box"); st.State != StateStopped {
		t.Errorf("after Stop: %+v", st)
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if len(sb.signals) == 0 || !strings.HasPrefix(sb.signals[0], EnvOwner+"=") || !strings.HasSuffix(sb.signals[0], "terminated") {
		t.Errorf("Signale = %v", sb.signals)
	}
}
