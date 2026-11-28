// Package ide manages the browser IDE (openvscode-server) per workspace: download on demand,
// start, make it reachable through the gateway and stop it when idle
// (doc/architecture.md#24-browser-ide). Since M5 it runs in the sandbox container.
package ide

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/DavidW475/ade/internal/agent"
	"github.com/DavidW475/ade/internal/apperr"
)

// States of an IDE.
const (
	StateStopped    = "stopped"
	StateInstalling = "installing"
	StateStarting   = "starting"
	StateRunning    = "running"
	StateError      = "error"
)

// Environment variables ADE sets for the IDE process. EnvOwner detects orphaned processes
// after a crash (like ADE_SESSION_ID for agents) and separates several daemons.
const (
	EnvOwner         = "ADE_IDE_OWNER"
	EnvWorkspaceID   = agent.EnvWorkspaceID
	EnvWorkspacePath = agent.EnvWorkspacePath
)

// Defaults for time limits.
const (
	DefaultStartTimeout = 60 * time.Second
	DefaultStopTimeout  = 5 * time.Second
	DefaultIdleTimeout  = 15 * time.Minute
)

// WorkspaceFunc returns the path of a workspace's working directory. For unknown workspaces
// it returns an apperr.NotFound error, for those without a working directory
// apperr.Conflict.
type WorkspaceFunc func(ctx context.Context, id string) (string, error)

// Sandbox runs commands in a workspace's Docker container (since M5). The IDE then runs
// there: openvscode-server from the image, port published on 127.0.0.1 by Docker.
type Sandbox interface {
	// Command builds a process in the container (runs in the workspace directory).
	Command(ctx context.Context, argv, env []string) *exec.Cmd
	// Signal sends sig to all processes in the container with the variable marker (KEY=VALUE).
	Signal(ctx context.Context, marker string, sig syscall.Signal) error
	// HostPort returns the port on 127.0.0.1 for port in the container.
	HostPort(ctx context.Context, port int) (int, error)
	// WriteFile writes a file in the container, readable only by the user.
	WriteFile(ctx context.Context, path string, data []byte) error
}

// SandboxFunc returns a workspace's sandbox (nil: the IDE runs locally). It starts the
// container on demand.
type SandboxFunc func(ctx context.Context, id string) (Sandbox, error)

// Paths and port of the IDE in the container.
const (
	sandboxPort      = 3000
	sandboxTokenFile = "/home/ade/.ade/ide-connection-token" //nolint:gosec // G101: path, not a secret
)

// Options configures the manager.
type Options struct {
	// Dir contains the downloaded IDE, shared extensions and data per workspace.
	Dir string
	// Binary is an own openvscode-server; empty = download the pinned version.
	Binary     string
	Workspaces WorkspaceFunc
	// Sandbox is optional; without it all IDEs run locally.
	Sandbox SandboxFunc
	Logger  *slog.Logger

	IdleTimeout  time.Duration
	StartTimeout time.Duration
	StopTimeout  time.Duration
	// Env is the environment the IDE inherits from (default: os.Environ). ADE only passes on
	// the same variables as to agents (agent.FilterEnv).
	Env []string
	// ExtraEnv is appended unfiltered (tests).
	ExtraEnv []string
	// HTTPClient downloads the release (tests set their own).
	HTTPClient *http.Client
	// release overrides the download target (tests).
	release *release
}

// Status describes the state of a workspace's IDE.
type Status struct {
	State     string     `json:"state"`
	Error     string     `json:"error,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	// Connections is the number of open connections through the gateway.
	Connections int `json:"connections"`
}

// Manager manages the IDEs of all workspaces.
type Manager struct {
	opts Options
	log  *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	inst    map[string]*instance
	install *installRun
}

type installRun struct {
	done chan struct{}
	path string
	err  error
}

type instance struct {
	wsID    string
	state   string
	err     string
	started time.Time
	port    int
	token   string
	pid     int
	sb      Sandbox // set if the IDE runs in the container

	// ctx ends with Stop; it aborts download and start waiting, not the process itself.
	ctx    context.Context
	cancel context.CancelFunc

	ready    chan struct{} // closed as soon as running or failed
	exited   chan struct{} // closed when the process has ended
	stopping bool

	conns      int
	lastActive time.Time
}

// NewManager creates a manager. It starts nothing until an IDE is requested.
func NewManager(opts Options) *Manager {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = DefaultStartTimeout
	}
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = DefaultStopTimeout
	}
	if opts.Env == nil {
		opts.Env = os.Environ()
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 15 * time.Minute}
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{opts: opts, log: opts.Logger, ctx: ctx, cancel: cancel, inst: map[string]*instance{}}
	m.wg.Add(1)
	go m.reaper()
	return m
}

// Recover ends IDE processes that an earlier run of this daemon left behind.
func (m *Manager) Recover() {
	if n := killOwned(m.opts.Dir); n > 0 {
		m.log.Warn("orphaned IDE processes ended", "count", n)
	}
}

// Close stops all IDEs and waits for them to end.
func (m *Manager) Close() {
	m.cancel()
	m.mu.Lock()
	ids := make([]string, 0, len(m.inst))
	for id := range m.inst {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Stop(id)
	}
	m.wg.Wait()
}

// Status returns the state of a workspace's IDE.
func (m *Manager) Status(id string) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked(id)
}

func (m *Manager) statusLocked(id string) Status {
	in, ok := m.inst[id]
	if !ok {
		return Status{State: StateStopped}
	}
	st := Status{State: in.state, Error: in.err, Connections: in.conns}
	if in.state == StateRunning {
		t := in.started
		st.StartedAt = &t
	}
	return st
}

// Start starts a workspace's IDE if it is not already running or starting, and returns
// right away. Status reports the progress.
func (m *Manager) Start(ctx context.Context, id string) (Status, error) {
	if m.ctx.Err() != nil {
		return Status{}, errors.New("daemon is stopping")
	}
	path, err := m.opts.Workspaces(ctx, id)
	if err != nil {
		return Status{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if in, ok := m.inst[id]; ok && in.state != StateError {
		return m.statusLocked(id), nil
	}
	ictx, icancel := context.WithCancel(m.ctx)
	in := &instance{
		ctx:        ictx,
		cancel:     icancel,
		wsID:       id,
		state:      StateStarting,
		ready:      make(chan struct{}),
		exited:     make(chan struct{}),
		lastActive: time.Now(),
	}
	m.inst[id] = in
	m.wg.Add(1)
	go m.run(in, path) //nolint:gosec // G118: the IDE outlives the request; it ends with in.ctx
	return m.statusLocked(id), nil
}

// Upstream is a running IDE to which the gateway passes a request.
type Upstream struct {
	URL   *url.URL
	Token string
	// Release reports the end of the request or of the WebSocket connection.
	Release func()
}

// Acquire returns a workspace's running IDE and counts the connection until Release is
// called. If the IDE is not running, Acquire starts it and waits until it is ready or ctx
// ends.
func (m *Manager) Acquire(ctx context.Context, id string) (*Upstream, error) {
	if _, err := m.Start(ctx, id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	in := m.inst[id]
	m.mu.Unlock()
	if in == nil {
		return nil, apperr.Conflict("IDE was stopped")
	}
	select {
	case <-in.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inst[id] != in || in.state != StateRunning {
		msg := in.err
		if msg == "" {
			msg = "IDE is not running"
		}
		return nil, apperr.Conflict("%s", msg)
	}
	in.conns++
	in.lastActive = time.Now()
	var once sync.Once
	return &Upstream{
		URL:   &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(in.port))},
		Token: in.token,
		Release: func() {
			once.Do(func() {
				m.mu.Lock()
				defer m.mu.Unlock()
				in.conns--
				in.lastActive = time.Now()
			})
		},
	}, nil
}

// Stop ends a workspace's IDE (SIGTERM to the process group, SIGKILL after StopTimeout)
// and waits for it to end.
func (m *Manager) Stop(id string) {
	m.mu.Lock()
	in, ok := m.inst[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	in.stopping = true
	pid, sb := in.pid, in.sb
	m.mu.Unlock()
	in.cancel()

	if pid > 0 && sb != nil {
		marker := EnvOwner + "=" + m.opts.Dir
		_ = sb.Signal(context.Background(), marker, syscall.SIGTERM)
		select {
		case <-in.exited:
		case <-time.After(m.opts.StopTimeout):
			_ = sb.Signal(context.Background(), marker, syscall.SIGKILL)
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			<-in.exited
		}
	} else if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		select {
		case <-in.exited:
		case <-time.After(m.opts.StopTimeout):
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			<-in.exited
		}
	} else {
		<-in.exited // Start is still running (download etc.) and aborts through in.ctx
	}
	m.mu.Lock()
	if m.inst[id] == in {
		delete(m.inst, id)
	}
	m.mu.Unlock()
}

// BeforeWorkspaceDelete stops the IDE and removes its data.
func (m *Manager) BeforeWorkspaceDelete(_ context.Context, id string) error {
	m.Stop(id)
	if err := os.RemoveAll(m.dataDir(id)); err != nil {
		return fmt.Errorf("delete IDE data: %w", err)
	}
	return nil
}

func (m *Manager) dataDir(id string) string { return filepath.Join(m.opts.Dir, "workspaces", id) }

// run installs if needed, starts the process, waits for readiness and then for the
// process to end.
func (m *Manager) run(in *instance, workdir string) {
	defer m.wg.Done()
	defer in.cancel()
	defer close(in.exited)
	readyOnce := sync.OnceFunc(func() { close(in.ready) })
	defer readyOnce()

	fail := func(err error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if in.stopping {
			in.state = StateStopped
			return
		}
		in.state, in.err = StateError, err.Error()
		m.log.Error("IDE failed", "workspace", in.wsID, "err", err)
	}

	var sb Sandbox
	if m.opts.Sandbox != nil {
		var err error
		if sb, err = m.opts.Sandbox(in.ctx, in.wsID); err != nil {
			fail(fmt.Errorf("sandbox container: %w", err))
			return
		}
	}
	var cmd *exec.Cmd
	if sb != nil {
		var err error
		if cmd, err = m.sandboxCommand(in, sb); err != nil {
			fail(err)
			return
		}
	} else {
		bin, err := m.binary(in)
		if err != nil {
			fail(err)
			return
		}
		if cmd, err = m.command(in, bin, workdir); err != nil {
			fail(err)
			return
		}
	}
	var stderr tail
	cmd.Stdout, cmd.Stderr = &stderr, &stderr

	m.mu.Lock()
	if in.stopping {
		in.state = StateStopped
		m.mu.Unlock()
		return
	}
	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		fail(fmt.Errorf("start openvscode-server: %w", err))
		return
	}
	in.pid = cmd.Process.Pid
	m.mu.Unlock()
	if sb != nil {
		// Docker only knows the published port after the container has started.
		port, err := sb.HostPort(in.ctx, sandboxPort)
		if err != nil {
			_ = sb.Signal(context.Background(), EnvOwner+"="+m.opts.Dir, syscall.SIGKILL)
			_ = syscall.Kill(-in.pid, syscall.SIGKILL)
			_ = cmd.Wait()
			fail(fmt.Errorf("IDE port: %w", err))
			return
		}
		m.mu.Lock()
		in.port, in.sb = port, sb
		m.mu.Unlock()
	}
	m.log.Info("IDE started", "workspace", in.wsID, "pid", in.pid, "port", in.port, "sandbox", sb != nil)

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	killAll := func() {
		if sb != nil {
			_ = sb.Signal(context.Background(), EnvOwner+"="+m.opts.Dir, syscall.SIGKILL)
		}
		_ = syscall.Kill(-in.pid, syscall.SIGKILL)
	}
	if err := m.waitReady(in, waitErr); err != nil {
		killAll()
		if out := strings.TrimSpace(stderr.String()); out != "" {
			err = fmt.Errorf("%w: %s", err, lastLine(out))
		}
		fail(err)
		return
	}
	m.mu.Lock()
	in.state, in.started, in.lastActive = StateRunning, time.Now(), time.Now()
	m.mu.Unlock()
	readyOnce()

	err := <-waitErr
	// Take leftovers of the process group with it (e.g. extension hosts).
	killAll()
	m.mu.Lock()
	defer m.mu.Unlock()
	if in.stopping {
		in.state = StateStopped
		m.log.Info("IDE stopped", "workspace", in.wsID)
		return
	}
	in.state = StateError
	in.err = "openvscode-server exited unexpectedly"
	if err != nil {
		in.err += ": " + err.Error()
	}
	if out := strings.TrimSpace(stderr.String()); out != "" {
		in.err += " – " + lastLine(out)
	}
	m.log.Error("IDE exited unexpectedly", "workspace", in.wsID, "err", in.err)
}

// waitReady polls the port until the server answers, the process ends or StartTimeout
// expires.
func (m *Manager) waitReady(in *instance, waitErr chan error) error {
	client := &http.Client{Timeout: time.Second}
	deadline := time.NewTimer(m.opts.StartTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	u := fmt.Sprintf("http://127.0.0.1:%d/", in.port)
	for {
		select {
		case err := <-waitErr:
			waitErr <- err // queue it again for run
			return fmt.Errorf("openvscode-server exited during start: %w", err)
		case <-deadline.C:
			return fmt.Errorf("openvscode-server does not answer after %s", m.opts.StartTimeout)
		case <-in.ctx.Done():
			return errors.New("start aborted")
		case <-tick.C:
			resp, err := client.Get(u) //nolint:gosec // G107: local address
			if err == nil {
				_ = resp.Body.Close()
				return nil
			}
		}
	}
}

// binary returns the path of the openvscode-server and downloads it if needed.
// Several concurrent starts share one download.
func (m *Manager) binary(in *instance) (string, error) {
	if m.opts.Binary != "" {
		if !fileExists(m.opts.Binary) {
			return "", fmt.Errorf("ide.binary %s not found", m.opts.Binary)
		}
		return m.opts.Binary, nil
	}
	rel, err := defaultRelease()
	if m.opts.release != nil {
		rel, err = *m.opts.release, nil
	}
	if err != nil {
		return "", err
	}
	if p := binaryIn(filepath.Join(m.opts.Dir, rel.Name)); fileExists(p) {
		return p, nil
	}

	m.mu.Lock()
	in.state = StateInstalling
	run := m.install
	if run == nil {
		run = &installRun{done: make(chan struct{})}
		m.install = run
		go func() {
			m.log.Info("downloading openvscode-server", "version", Version, "url", rel.URL)
			run.path, run.err = install(m.ctx, m.opts.HTTPClient, m.opts.Dir, rel)
			if run.err == nil {
				m.log.Info("openvscode-server installed", "path", run.path)
			}
			m.mu.Lock()
			m.install = nil
			m.mu.Unlock()
			close(run.done)
		}()
	}
	m.mu.Unlock()
	select {
	case <-run.done:
	case <-in.ctx.Done():
		return "", errors.New("start aborted")
	}

	m.mu.Lock()
	if in.state == StateInstalling {
		in.state = StateStarting
	}
	m.mu.Unlock()
	return run.path, run.err
}

// command builds the call. The server reads the connection token from a file, so it does
// not show up in the process list.
func (m *Manager) command(in *instance, bin, workdir string) (*exec.Cmd, error) {
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("find a free port: %w", err)
	}
	data := m.dataDir(in.wsID)
	if err := os.MkdirAll(data, 0o700); err != nil {
		return nil, fmt.Errorf("create IDE data directory: %w", err)
	}
	ext := filepath.Join(m.opts.Dir, "extensions")
	if err := os.MkdirAll(ext, 0o700); err != nil {
		return nil, fmt.Errorf("create extensions directory: %w", err)
	}
	token := randomToken()
	tokenFile := filepath.Join(data, "connection-token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		return nil, fmt.Errorf("write connection token: %w", err)
	}

	cmd := exec.CommandContext(m.ctx, bin, //nolint:gosec // G204: binary from the configuration or a verified download
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--connection-token-file", tokenFile,
		"--server-data-dir", filepath.Join(data, "server"),
		"--user-data-dir", filepath.Join(data, "user"),
		"--extensions-dir", ext,
		"--default-folder", workdir,
		"--telemetry-level", "off",
	)
	cmd.Dir = workdir
	cmd.Env = append(agent.FilterEnv(m.opts.Env),
		EnvOwner+"="+m.opts.Dir,
		EnvWorkspaceID+"="+in.wsID,
		EnvWorkspacePath+"="+workdir,
	)
	cmd.Env = append(cmd.Env, m.opts.ExtraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// When the daemon ends, Close stops the IDE in order; CommandContext is only the fallback.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

	m.mu.Lock()
	in.port, in.token = port, token
	m.mu.Unlock()
	return cmd, nil
}

// sandboxCommand builds the call of openvscode-server in the container. The connection token
// is in a file in the container; the server listens on all addresses of the container, but
// from outside it is only reachable through the port published on 127.0.0.1.
func (m *Manager) sandboxCommand(in *instance, sb Sandbox) (*exec.Cmd, error) {
	token := randomToken()
	if err := sb.WriteFile(in.ctx, sandboxTokenFile, []byte(token)); err != nil {
		return nil, fmt.Errorf("write connection token in the container: %w", err)
	}
	cmd := sb.Command(m.ctx, []string{
		"openvscode-server",
		"--host", "0.0.0.0",
		"--port", strconv.Itoa(sandboxPort),
		"--connection-token-file", sandboxTokenFile,
		"--default-folder", "/workspace",
		"--telemetry-level", "off",
	}, []string{EnvOwner + "=" + m.opts.Dir, EnvWorkspaceID + "=" + in.wsID, EnvWorkspacePath + "=/workspace"})
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	m.mu.Lock()
	in.token = token
	m.mu.Unlock()
	return cmd, nil
}

// reaper stops IDEs without a connection that have been idle longer than IdleTimeout.
func (m *Manager) reaper() {
	defer m.wg.Done()
	every := min(m.opts.IdleTimeout/4, time.Minute)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
		m.mu.Lock()
		var idle []string
		for id, in := range m.inst {
			switch {
			case in.state == StateRunning && in.conns == 0 && time.Since(in.lastActive) > m.opts.IdleTimeout:
				idle = append(idle, id)
			case in.state == StateError && time.Since(in.lastActive) > m.opts.IdleTimeout:
				delete(m.inst, id) // do not show errors forever
			}
		}
		m.mu.Unlock()
		for _, id := range idle {
			m.log.Info("IDE idle, stopping", "workspace", id)
			m.Stop(id)
		}
	}
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// tail keeps the last 4 KiB of the output.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4<<10 {
		t.buf = t.buf[len(t.buf)-4<<10:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// procDir is the proc file system (tests set their own directory).
var procDir = "/proc"

// killOwned ends all processes whose environment contains EnvOwner=dir and returns their count.
func killOwned(dir string) int {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return 0
	}
	self := os.Getpid()
	want := []byte(EnvOwner + "=" + dir)
	n := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		data, err := os.ReadFile(filepath.Join(procDir, e.Name(), "environ"))
		if err != nil {
			continue
		}
		for kv := range bytes.SplitSeq(data, []byte{0}) {
			if bytes.Equal(kv, want) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				n++
				break
			}
		}
	}
	return n
}
