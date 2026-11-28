// Package sandbox manages a workspace's Docker container (doc/sandbox.md): create, start,
// run commands via docker exec, stop, remove and clean up orphaned containers. For that ADE
// calls the Docker CLI (ADR-014), just like git through internal/git.
//
// A container only sees the workspace directory (/workspace), runs with the user's UID,
// without capabilities and with resource limits. It is attached to an internal network
// without a route to the outside; the only way to the internet is the workspace's egress
// proxy in its own container (network.go).
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/DavidW475/ade/internal/proxy"
)

// Paths and ports in the container.
const (
	Workdir = "/workspace"
	Home    = "/home/ade"
	// IDEPort is the port of openvscode-server in the container; Docker publishes it on a
	// free port of 127.0.0.1.
	IDEPort = 3000
)

// Labels on all of ADE's Docker objects.
const (
	LabelManaged   = "ade.managed"
	LabelWorkspace = "ade.workspace"
	// LabelDataDir separates several ADE instances (e.g. a development daemon) on one Docker.
	LabelDataDir = "ade.data_dir"
	// LabelRole distinguishes agent and proxy containers.
	LabelRole = "ade.role"
	// LabelNet is the version of the network layout (NetVersion).
	LabelNet = "ade.net"
	// LabelBinary is the mounted ade binary of a proxy container.
	LabelBinary = "ade.binary"
)

// Roles of a workspace's containers.
const (
	RoleAgent = "agent"
	RoleProxy = "proxy"
	// RoleReader is the container for read-only roles of a factory: /workspace read-only (M9).
	RoleReader = "reader"
)

// ReaderAlias is the name of the reader container in the workspace network.
const ReaderAlias = "reader"

// DetachKeys replaces Docker's Ctrl-P Ctrl-Q, so the keys reach the terminal.
const DetachKeys = "ctrl-@,ctrl-^,ctrl-@,ctrl-^"

// StopTimeout is the time docker stop gives the processes in the container.
const StopTimeout = 3 * time.Second

// Options configures the manager.
type Options struct {
	// Docker is the docker binary (default: docker in PATH). Also podman or a docker shim of
	// Podman; Available detects the runtime.
	Docker string
	// Runtime sets the runtime instead of detecting it (for tests).
	Runtime *Runtime
	// DataDir marks the containers of this ADE instance (label ade.data_dir).
	DataDir string
	// Image is the default image if sandbox.yaml names none.
	Image string
	// UID and GID for processes in the container (default: those of the daemon).
	UID, GID int
	// ProxyBinary is the ade binary on the host that runs in the proxy container (statically
	// built, only mounted read-only).
	ProxyBinary string
	// ProxyImage is the image of the proxy container (default: Image).
	ProxyImage string
	// Secrets returns the API keys per provider for the proxy's secret injection.
	Secrets func() map[string]string
	// OnEgress receives the proxies' egress events.
	OnEgress func(workspace string, events []proxy.Event)
	// OnQuestion answers questions of the proxies (approvals, M7). It blocks until decided;
	// ctx ends when the asker gives up. Without a handler, deny applies.
	OnQuestion QuestionHandler
	// AskWait is the waiting time of a request with egress.on_blocked: ask (default 2m).
	AskWait time.Duration
	// ReloadInterval is the interval at which changed policies go to the proxies.
	ReloadInterval time.Duration
	Logger         *slog.Logger
}

// Manager manages the containers of all workspaces.
type Manager struct {
	opts Options
	log  *slog.Logger

	mu     sync.Mutex
	locks  map[string]*sync.Mutex
	avail  *availability
	rt     *Runtime             // detected runtime (nil: not detected yet)
	ensure map[string]time.Time // last seen running
	links  map[string]*proxyLink
	source SpecSource

	http     *http.Client
	bg       context.Context
	bgCancel context.CancelFunc
	bgOnce   sync.Once
}

type availability struct {
	at      time.Time
	version string
	err     error
}

// New creates a manager.
func New(opts Options) *Manager {
	if opts.Docker == "" {
		opts.Docker = "docker"
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.UID == 0 && opts.GID == 0 {
		opts.UID, opts.GID = os.Getuid(), os.Getgid()
	}
	if opts.Image == "" {
		opts.Image = DefaultImage("")
	}
	if opts.ProxyImage == "" {
		opts.ProxyImage = opts.Image
	}
	if opts.AskWait <= 0 {
		opts.AskWait = proxy.DefaultAskWait
	}
	if opts.ReloadInterval <= 0 {
		opts.ReloadInterval = 3 * time.Second
	}
	m := &Manager{
		opts: opts, log: opts.Logger, locks: map[string]*sync.Mutex{}, ensure: map[string]time.Time{},
		links: map[string]*proxyLink{},
		// Only 127.0.0.1, never through a proxy from the daemon's environment.
		http: &http.Client{Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4}},
	}
	if opts.Runtime != nil {
		rt := *opts.Runtime
		m.rt = &rt
	}
	m.bg, m.bgCancel = context.WithCancel(context.Background())
	return m
}

// Close ends background work (event fetching, hot reload). Containers stay as they are.
func (m *Manager) Close() {
	m.bgCancel()
	m.mu.Lock()
	links := m.links
	m.links = map[string]*proxyLink{}
	m.mu.Unlock()
	for _, l := range links {
		l.cancel()
		<-l.done
	}
}

func (m *Manager) startReload() {
	m.bgOnce.Do(func() { go m.reloadLoop() })
}

// Image returns the default image.
func (m *Manager) Image() string { return m.opts.Image }

// ContainerName is the name of a workspace's container.
func ContainerName(workspace string) string { return "ade-ws-" + workspace }

// ReaderName is the name of a workspace's reader container.
func ReaderName(workspace string) string { return ContainerName(workspace) + "-ro" }

// Error is a failed docker call with its error output.
type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	sub := ""
	if len(e.Args) > 0 {
		sub = " " + e.Args[0]
	}
	return "docker" + sub + ": " + msg
}

func (e *Error) Unwrap() error { return e.Err }

// dockerEnv is the Docker CLI's environment: only what it (or Podman) needs to find the
// daemon or the rootless environment.
func dockerEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if reservedEnv(k) {
			out = append(out, kv)
		}
	}
	return out
}

// reservedEnv reports variables the Docker CLI uses itself. They are passed to docker exec
// with their value, all others without a value (from the CLI's environment, so values like
// API keys do not show up in the process list).
func reservedEnv(k string) bool {
	switch k {
	case "PATH", "HOME", "USER", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "DBUS_SESSION_BUS_ADDRESS",
		"CONTAINER_HOST", "CONTAINER_CONNECTION", "CONTAINER_SSHKEY":
		return true
	}
	return strings.HasPrefix(k, "DOCKER_") || strings.HasPrefix(k, "CONTAINERS_")
}

func (m *Manager) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, m.opts.Docker, args...) //nolint:gosec // G204: the manager builds the arguments
	cmd.Env = dockerEnv()
	return cmd
}

func (m *Manager) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := m.command(ctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("docker CLI is not installed (%s not found)", m.opts.Docker)
		}
		return stdout.Bytes(), &Error{Args: args, Stderr: stderr.String(), Err: err}
	}
	return stdout.Bytes(), nil
}

// Available checks (at most every 30 s) whether the Docker CLI reaches a runtime, detects its
// kind (Runtime) and returns the version.
func (m *Manager) Available(ctx context.Context) (string, error) {
	m.mu.Lock()
	if a := m.avail; a != nil && time.Since(a.at) < 30*time.Second {
		m.mu.Unlock()
		return a.version, a.err
	}
	m.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := m.run(cctx, nil, "info", "--format", "{{json .}}")
	var rt Runtime
	if err == nil {
		rt, err = ParseInfo(out)
	}
	if err != nil {
		err = fmt.Errorf("docker not reachable: %w", err)
	}
	m.mu.Lock()
	m.avail = &availability{at: time.Now(), version: rt.Version, err: err}
	if err == nil && m.opts.Runtime == nil {
		if m.rt == nil || *m.rt != rt {
			m.log.Info("container runtime detected", "runtime", rt.Name(), "version", rt.Version)
		}
		m.rt = &rt
	}
	m.mu.Unlock()
	return rt.Version, err
}

// Runtime returns the detected container runtime; ok is false as long as Available has not
// detected it yet.
func (m *Manager) Runtime() (rt Runtime, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rt == nil {
		return Runtime{}, false
	}
	return *m.rt, true
}

// Spec describes a workspace's container.
type Spec struct {
	Workspace string
	// Path is the workspace directory on the host (mounted to /workspace).
	Path   string
	Config Config
	// Env are fixed variables of the container (e.g. Git identity), no secrets.
	Env []string
	// Grants are egress exceptions from approvals in the dashboard (they extend egress.allow).
	Grants []string
}

// ImageFor returns the image for a configuration.
func (m *Manager) ImageFor(c Config) string {
	if c.Image != "" {
		return c.Image
	}
	return m.opts.Image
}

// Container describes a container of ADE.
type Container struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Workspace string    `json:"workspace"`
	DataDir   string    `json:"data_dir"`
	Image     string    `json:"image"`
	State     string    `json:"state"`
	Running   bool      `json:"running"`
	Created   time.Time `json:"created"`
	Role      string    `json:"role,omitempty"`
	// Net is the version of the network layout (empty: container from M5 with a normal network).
	Net    string `json:"net,omitempty"`
	Binary string `json:"-"`
	token  string
	// Mounts are the targets of all mounts (for tests and diagnosis).
	Mounts []Mount `json:"mounts,omitempty"`
}

// Mount is a mount of a container.
type Mount struct {
	Type        string `json:"type"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	RW          bool   `json:"rw"`
}

type inspectJSON struct {
	ID      string `json:"Id"`
	Name    string `json:"Name"`
	Created time.Time
	State   struct {
		Status  string
		Running bool
	}
	Config struct {
		Image  string
		Labels map[string]string
		Env    []string
	}
	Mounts []struct {
		Type        string
		Source      string
		Destination string
		RW          bool
	}
}

func (m *Manager) inspect(ctx context.Context, refs ...string) ([]Container, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	out, err := m.run(ctx, nil, append([]string{"container", "inspect", "--"}, refs...)...)
	if err != nil {
		var de *Error
		if !errors.As(err, &de) || !strings.Contains(strings.ToLower(de.Stderr), "no such") {
			return nil, err
		}
		if len(refs) == 1 {
			return nil, nil
		}
		// A container disappeared between listing and inspecting (discard, parallel ade gc):
		// ask for each one and skip the missing ones (#66).
		var cs []Container
		for _, ref := range refs {
			c, err := m.inspect(ctx, ref)
			if err != nil {
				return nil, err
			}
			cs = append(cs, c...)
		}
		return cs, nil
	}
	var raw []inspectJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("read docker inspect: %w", err)
	}
	cs := make([]Container, 0, len(raw))
	for _, r := range raw {
		c := Container{
			ID: r.ID, Name: strings.TrimPrefix(r.Name, "/"), Image: r.Config.Image, State: r.State.Status,
			Running: r.State.Running, Created: r.Created,
			Workspace: r.Config.Labels[LabelWorkspace], DataDir: r.Config.Labels[LabelDataDir],
			Role: r.Config.Labels[LabelRole], Net: r.Config.Labels[LabelNet], Binary: r.Config.Labels[LabelBinary],
		}
		if c.Role == "" {
			c.Role = RoleAgent // container from M5
		}
		for _, kv := range r.Config.Env {
			if v, ok := strings.CutPrefix(kv, proxy.EnvToken+"="); ok {
				c.token = v
			}
		}
		for _, mt := range r.Mounts {
			c.Mounts = append(c.Mounts, Mount{Type: mt.Type, Source: mt.Source, Destination: mt.Destination, RW: mt.RW})
		}
		cs = append(cs, c)
	}
	return cs, nil
}

// Get returns a workspace's container or nil if there is none.
func (m *Manager) Get(ctx context.Context, workspace string) (*Container, error) {
	return m.getRole(ctx, workspace, ContainerName(workspace), RoleAgent)
}

// GetReader returns a workspace's reader container or nil if there is none.
func (m *Manager) GetReader(ctx context.Context, workspace string) (*Container, error) {
	return m.getRole(ctx, workspace, ReaderName(workspace), RoleReader)
}

func (m *Manager) getRole(ctx context.Context, workspace, name, role string) (*Container, error) {
	cs, err := m.inspect(ctx, name)
	if err != nil || len(cs) == 0 {
		return nil, err
	}
	c := cs[0]
	if c.DataDir != m.opts.DataDir || c.Workspace != workspace || c.Role != role {
		return nil, fmt.Errorf("container %s does not belong to this ADE instance", c.Name)
	}
	return &c, nil
}

// List returns all containers with the label ade.managed – also those of other ADE instances.
func (m *Manager) List(ctx context.Context) ([]Container, error) {
	out, err := m.run(ctx, nil, "container", "ls", "--all", "--quiet", "--no-trunc", "--filter", "label="+LabelManaged+"=true")
	if err != nil {
		return nil, err
	}
	return m.inspect(ctx, strings.Fields(string(out))...)
}

func (m *Manager) lock(workspace string) func() {
	m.mu.Lock()
	l := m.locks[workspace]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[workspace] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Ensure makes sure that network, proxy and container of the workspace exist and run, and
// sends the policy to the proxy. If an image is missing locally, Docker downloads it (that
// can take a while). Containers from M5 (normal network) are recreated in the process.
func (m *Manager) Ensure(ctx context.Context, s Spec) error {
	unlock := m.lock(s.Workspace)
	defer unlock()
	m.startReload()
	m.mu.Lock()
	seen, linked := m.ensure[s.Workspace], m.links[s.Workspace] != nil
	m.mu.Unlock()
	if linked && time.Since(seen) < 2*time.Second {
		return nil
	}

	// The runtime determines UID and options, also for docker exec (result cached for 30 s).
	if _, err := m.Available(ctx); err != nil {
		return err
	}
	if err := m.checkResources(); err != nil {
		return err
	}
	c, err := m.Get(ctx, s.Workspace)
	if err != nil {
		return err
	}
	netCreated, err := m.ensureNetworks(ctx, s.Workspace)
	if err != nil {
		return err
	}
	if netCreated || (c != nil && c.Net != NetVersion) {
		// A reader container is attached to the old network: recreate it on next use.
		if _, err := m.run(ctx, nil, "container", "rm", "--force", "--", ReaderName(s.Workspace)); err != nil && !isNoSuch(err) {
			return fmt.Errorf("remove old reader container: %w", err)
		}
	}
	if c != nil && (c.Net != NetVersion || netCreated) {
		m.log.Info("sandbox container is recreated with the current layout", "workspace", s.Workspace, "before", c.Net)
		m.unlinkLocked(s.Workspace)
		if _, err := m.run(ctx, nil, "container", "rm", "--force", "--", c.ID); err != nil {
			return fmt.Errorf("remove old container: %w", err)
		}
		c = nil
	}
	if _, err := m.ensureProxy(ctx, s.Workspace, netCreated); err != nil {
		return err
	}
	if err := m.connectProxy(ctx, s.Workspace, m.policyFor(s)); err != nil {
		return err
	}
	created := false
	if c == nil {
		if err := m.create(ctx, s); err != nil {
			return err
		}
		created = true
	}
	if c == nil || !c.Running {
		if _, err := m.run(ctx, nil, "container", "start", ContainerName(s.Workspace)); err != nil {
			return fmt.Errorf("start container: %w", err)
		}
		m.log.Info("sandbox container started", "workspace", s.Workspace)
	}
	if created {
		m.prepareUser(ctx, ContainerName(s.Workspace))
	}
	m.mu.Lock()
	m.ensure[s.Workspace] = time.Now()
	m.mu.Unlock()
	return nil
}

// EnsureReader takes care of network and proxy like Ensure and additionally of the
// workspace's reader container: same image, same internal network, same proxy, but
// /workspace mounted read-only. Read-only roles of a factory (planner, reviewer) run in it and
// cannot change the workspace.
func (m *Manager) EnsureReader(ctx context.Context, s Spec) error {
	if err := m.Ensure(ctx, s); err != nil {
		return err
	}
	unlock := m.lock(s.Workspace)
	defer unlock()
	c, err := m.GetReader(ctx, s.Workspace)
	if err != nil {
		return err
	}
	if c != nil && c.Net != NetVersion {
		if _, err := m.run(ctx, nil, "container", "rm", "--force", "--", c.ID); err != nil {
			return fmt.Errorf("remove old reader container: %w", err)
		}
		c = nil
	}
	created := false
	if c == nil {
		if err := checkMountPath(s.Path); err != nil {
			return err
		}
		m.log.Info("creating reader container", "workspace", s.Workspace, "image", m.ImageFor(s.Config))
		if _, err := m.run(ctx, nil, m.createArgs(s, true)...); err != nil {
			return fmt.Errorf("create reader container: %w", err)
		}
		created = true
	}
	if c == nil || !c.Running {
		if _, err := m.run(ctx, nil, "container", "start", ReaderName(s.Workspace)); err != nil {
			return fmt.Errorf("start reader container: %w", err)
		}
	}
	if created {
		m.prepareUser(ctx, ReaderName(s.Workspace))
	}
	return nil
}

// CreateArgs returns the arguments for docker create (exported for tests).
func (m *Manager) CreateArgs(s Spec) []string { return m.createArgs(s, false) }

// ReaderCreateArgs returns the arguments for docker create of the reader container.
func (m *Manager) ReaderCreateArgs(s Spec) []string { return m.createArgs(s, true) }

func (m *Manager) createArgs(s Spec, reader bool) []string {
	r := s.Config.Resources
	name, role, alias, mount := ContainerName(s.Workspace), RoleAgent, AgentAlias, "type=bind,source="+s.Path+",target="+Workdir
	if reader {
		name, role, alias, mount = ReaderName(s.Workspace), RoleReader, ReaderAlias, mount+",readonly"
	}
	args := []string{
		"container", "create",
		"--name", name,
		"--label", LabelManaged + "=true",
		"--label", LabelWorkspace + "=" + s.Workspace,
		"--label", LabelDataDir + "=" + m.opts.DataDir,
		"--hostname", "ws-" + s.Workspace,
	}
	args = append(args, m.userArgs()...)
	args = append(args,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--init",
		"--mount", mount,
		"--label", LabelRole+"="+role,
		"--label", LabelNet+"="+NetVersion,
		"--network", NetworkName(s.Workspace),
		"--network-alias", alias,
		"--dns", noDNS,
		"--workdir", Workdir,
		"--env", "HOME="+Home,
	)
	for _, kv := range append(proxyEnv(), m.runtimeEnv()...) {
		args = append(args, "--env", kv)
	}
	if m.opts.ProxyBinary != "" {
		// For ade hook pretooluse (approvals); read-only, the binary contains no secrets.
		args = append(args, "--mount", "type=bind,source="+m.opts.ProxyBinary+",target="+ProxyBinary+",readonly")
	}
	if r.Memory != "" {
		args = append(args, "--memory", r.Memory)
	}
	if r.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(r.CPUs, 'f', -1, 64))
	}
	if r.PIDs > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(r.PIDs))
	}
	for _, kv := range s.Env {
		args = append(args, "--env", kv)
	}
	return append(args, "--entrypoint", "sleep", m.ImageFor(s.Config), "infinity")
}

func checkMountPath(p string) error {
	if strings.ContainsAny(p, ",\n") || !strings.HasPrefix(p, "/") {
		return fmt.Errorf("workspace path %q is not suitable for a mount", p)
	}
	return nil
}

func (m *Manager) create(ctx context.Context, s Spec) error {
	if err := checkMountPath(s.Path); err != nil {
		return err
	}
	m.log.Info("creating sandbox container", "workspace", s.Workspace, "image", m.ImageFor(s.Config))
	if _, err := m.run(ctx, nil, m.CreateArgs(s)...); err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	return nil
}

// prepareUser adds a passwd entry if the UID is missing in the image (otherwise shells show
// “I have no name!” and some tools fail). Errors are not fatal.
func (m *Manager) prepareUser(ctx context.Context, container string) {
	if m.runtime().RootInContainer() {
		return
	}
	uid, gid := strconv.Itoa(m.opts.UID), strconv.Itoa(m.opts.GID)
	script := `getent group "$2" >/dev/null || echo "ade$2:x:$2:" >> /etc/group
getent passwd "$1" >/dev/null || echo "ade$1:x:$1:$2:ADE:` + Home + `:/bin/bash" >> /etc/passwd`
	if _, err := m.run(ctx, nil, "exec", "--user", "0", container, "sh", "-c", script, "sh", uid, gid); err != nil {
		m.log.Debug("passwd entry in the container", "container", container, "err", err)
	}
}

// Exec describes a process in the container.
type Exec struct {
	Argv []string
	// Env (KEY=VALUE) is added to the image's environment.
	Env []string
	// TTY: with a pseudo terminal (the caller starts the CLI itself in a PTY).
	TTY bool
	// Stdin: pass on input.
	Stdin bool
	// Root: as root instead of as the user (only for ADE's preparations).
	Root bool
	// Reader: in the reader container instead of the agent container (EnsureReader).
	Reader bool
}

// ExecArgs returns the arguments for docker exec and the CLI's environment.
func (m *Manager) ExecArgs(workspace string, e Exec) ([]string, []string) {
	args := []string{"exec"}
	if e.Stdin || e.TTY {
		args = append(args, "--interactive")
	}
	if e.TTY {
		args = append(args, "--tty", "--detach-keys", DetachKeys)
	}
	user := m.user()
	if e.Root {
		user = "0"
	}
	args = append(args, "--user", user, "--workdir", Workdir)
	env := dockerEnv()
	for _, kv := range e.Env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if reservedEnv(k) {
			args = append(args, "--env", kv)
			continue
		}
		args = append(args, "--env", k)
		env = append(env, kv)
	}
	name := ContainerName(workspace)
	if e.Reader {
		name = ReaderName(workspace)
	}
	args = append(args, name)
	return append(args, e.Argv...), env
}

// Command builds a docker exec call. If ctx ends, only the CLI is ended; Signal ends the
// processes in the container.
func (m *Manager) Command(ctx context.Context, workspace string, e Exec) *exec.Cmd {
	args, env := m.ExecArgs(workspace, e)
	cmd := m.command(ctx, args...)
	cmd.Env = env
	return cmd
}

// signalScript sends a signal to all processes in the container whose environment contains
// the line $1 (KEY=VALUE) – like a signal to a process group, just without setsid.
const signalScript = `for d in /proc/[0-9]*; do
  p=${d#/proc/}
  [ "$p" = "$$" ] && continue
  if tr '\0' '\n' < "$d/environ" 2>/dev/null | grep -qxF -- "$1"; then kill -s "$2" "$p" 2>/dev/null; fi
done
exit 0`

// Signal sends sig to all processes in the container with the environment variable marker
// (KEY=VALUE), e.g. ADE_SESSION_ID=<id>.
func (m *Manager) Signal(ctx context.Context, workspace, marker string, sig syscall.Signal) error {
	return m.signalIn(ctx, ContainerName(workspace), marker, sig)
}

// SignalReader is Signal for the reader container.
func (m *Manager) SignalReader(ctx context.Context, workspace, marker string, sig syscall.Signal) error {
	return m.signalIn(ctx, ReaderName(workspace), marker, sig)
}

func (m *Manager) signalIn(ctx context.Context, container, marker string, sig syscall.Signal) error {
	name := map[syscall.Signal]string{syscall.SIGTERM: "TERM", syscall.SIGKILL: "KILL", syscall.SIGINT: "INT", syscall.SIGHUP: "HUP"}[sig]
	if name == "" {
		return fmt.Errorf("signal %v is not supported", sig)
	}
	if !strings.Contains(marker, "=") || strings.ContainsAny(marker, "\n\x00") {
		return fmt.Errorf("invalid marker %q", marker)
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := m.run(cctx, nil, "exec", "--user", m.user(),
		container, "sh", "-c", signalScript, "sh", marker, name)
	return err
}

// WriteFile writes a file in the container (e.g. the IDE's connection token) without its
// contents appearing in a process list.
func (m *Manager) WriteFile(ctx context.Context, workspace, path string, data []byte) error {
	_, err := m.run(ctx, data, "exec", "--interactive", "--user", m.user(),
		ContainerName(workspace), "sh", "-c", `umask 077 && mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", path)
	return err
}

// hostPort returns the port on 127.0.0.1 to which Docker maps port of the container name.
func (m *Manager) hostPort(ctx context.Context, name string, port int) (int, error) {
	out, err := m.run(ctx, nil, "container", "port", name, strconv.Itoa(port)+"/tcp")
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "127.0.0.1:") {
			continue
		}
		p, err := strconv.Atoi(strings.TrimPrefix(line, "127.0.0.1:"))
		if err == nil && p > 0 {
			return p, nil
		}
	}
	return 0, fmt.Errorf("port %d of the container is not published on 127.0.0.1", port)
}

// Stop stops container, reader container and proxy of a workspace (the files stay in the
// workspace).
func (m *Manager) Stop(ctx context.Context, workspace string) error {
	unlock := m.lock(workspace)
	defer unlock()
	m.unlinkLocked(workspace)
	if err := m.stop(ctx, ReaderName(workspace)); err != nil {
		return err
	}
	return m.stop(ctx, ContainerName(workspace), ProxyName(workspace))
}

// unlinkLocked forgets the state of a workspace (the caller holds its lock).
func (m *Manager) unlinkLocked(workspace string) {
	m.forget(workspace)
	m.unlink(workspace)
}

func (m *Manager) stop(ctx context.Context, refs ...string) error {
	if len(refs) == 0 {
		return nil
	}
	_, err := m.run(ctx, nil, append([]string{"container", "stop", "--time", strconv.Itoa(int(StopTimeout.Seconds())), "--"}, refs...)...)
	if err != nil && isNoSuch(err) {
		return nil // e.g. workspace from M5 without a proxy
	}
	return err
}

func (m *Manager) forget(workspace string) {
	m.mu.Lock()
	delete(m.ensure, workspace)
	m.mu.Unlock()
}

// Remove removes container, reader container, proxy and network of a workspace (as far as
// they exist).
func (m *Manager) Remove(ctx context.Context, workspace string) error {
	unlock := m.lock(workspace)
	defer unlock()
	m.unlinkLocked(workspace)
	c, err := m.Get(ctx, workspace)
	if err != nil {
		return err
	}
	rd, err := m.GetReader(ctx, workspace)
	if err != nil {
		return err
	}
	p, err := m.getProxy(ctx, workspace)
	if err != nil {
		return err
	}
	var ids []string
	for _, x := range []*Container{c, rd, p} {
		if x != nil {
			ids = append(ids, x.ID)
		}
	}
	if len(ids) > 0 {
		if _, err := m.run(ctx, nil, append([]string{"container", "rm", "--force", "--volumes", "--"}, ids...)...); err != nil {
			return fmt.Errorf("remove container: %w", err)
		}
		m.log.Info("sandbox container removed", "workspace", workspace)
	}
	nets, err := m.orphanNetworks(ctx, func(ws string) bool { return ws != workspace })
	if err != nil {
		return err
	}
	if err := m.removeNetworks(ctx, nets...); err != nil {
		return fmt.Errorf("remove network: %w", err)
	}
	return nil
}

// Orphans returns orphaned containers: those of this instance without a workspace (known
// returns false) and those of other instances whose data directory no longer exists.
func (m *Manager) Orphans(ctx context.Context, known func(workspace string) bool) ([]Container, error) {
	cs, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []Container
	for _, c := range cs {
		switch c.DataDir {
		case m.opts.DataDir:
			if c.Workspace == "" || !known(c.Workspace) {
				out = append(out, c)
			}
		case "":
			// Without an instance label, do not touch it.
		default:
			if _, err := os.Stat(c.DataDir); errors.Is(err, os.ErrNotExist) {
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// GC removes orphaned containers (agent and proxy, see Orphans) and then orphaned workspace
// networks. With dryRun it only lists them.
func (m *Manager) GC(ctx context.Context, known func(workspace string) bool, dryRun bool) ([]Container, error) {
	orphans, err := m.Orphans(ctx, known)
	if err != nil || dryRun {
		return orphans, err
	}
	if len(orphans) > 0 {
		ids := make([]string, len(orphans))
		for i, c := range orphans {
			ids[i] = c.ID
			m.unlink(c.Workspace)
		}
		if _, err := m.run(ctx, nil, append([]string{"container", "rm", "--force", "--volumes", "--"}, ids...)...); err != nil {
			return nil, fmt.Errorf("remove container: %w", err)
		}
		m.log.Info("orphaned sandbox containers removed", "count", len(orphans))
	}
	nets, err := m.orphanNetworks(ctx, known)
	if err != nil {
		return orphans, err
	}
	if err := m.removeNetworks(ctx, nets...); err != nil {
		return orphans, fmt.Errorf("remove networks: %w", err)
	}
	return orphans, nil
}

// Recover reconciles after a daemon start: remove orphaned containers, stop running
// containers of own workspaces (ends agent and IDE processes of the last run). They start
// again as soon as they are needed.
func (m *Manager) Recover(ctx context.Context, known func(workspace string) bool) error {
	if _, err := m.Available(ctx); err != nil {
		return err
	}
	if _, err := m.GC(ctx, known, false); err != nil {
		return err
	}
	return m.StopAll(ctx)
}

// StopAll stops all running containers of this instance.
func (m *Manager) StopAll(ctx context.Context) error {
	cs, err := m.List(ctx)
	if err != nil {
		return err
	}
	var running []string
	for _, c := range cs {
		if c.DataDir == m.opts.DataDir && c.Running {
			running = append(running, c.ID)
			m.forget(c.Workspace)
			m.unlink(c.Workspace)
		}
	}
	if len(running) > 0 {
		m.log.Info("stopping sandbox containers", "count", len(running))
	}
	return m.stop(ctx, running...)
}

// HasMount reports whether c has a mount with target dst (helper for tests).
func (c *Container) HasMount(dst string) bool {
	return slices.ContainsFunc(c.Mounts, func(mt Mount) bool { return mt.Destination == dst })
}
