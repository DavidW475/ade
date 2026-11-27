// Package sandbox verwaltet den Docker-Container eines Workspaces (doc/sandbox.md): anlegen,
// starten, Befehle per docker exec ausführen, stoppen, entfernen und verwaiste Container
// aufräumen. ADE ruft dafür die Docker-CLI auf (ADR-014), so wie git über internal/git.
//
// Ein Container sieht nur das Workspace-Verzeichnis (/workspace), läuft mit der UID des
// Nutzers, ohne Capabilities und mit Ressourcen-Grenzen. Er hängt in einem internen Netz
// ohne Route nach draußen; der einzige Weg ins Internet ist der Egress-Proxy des Workspaces
// in einem eigenen Container (network.go).
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

// Pfade und Ports im Container.
const (
	Workdir = "/workspace"
	Home    = "/home/ade"
	// IDEPort ist der Port von openvscode-server im Container; Docker veröffentlicht ihn
	// auf einem freien Port von 127.0.0.1.
	IDEPort = 3000
)

// Labels an allen Docker-Objekten von ADE.
const (
	LabelManaged   = "ade.managed"
	LabelWorkspace = "ade.workspace"
	// LabelDataDir trennt mehrere ADE-Instanzen (z. B. Entwicklungs-Daemon) auf einem Docker.
	LabelDataDir = "ade.data_dir"
	// LabelRole unterscheidet Agent- und Proxy-Container.
	LabelRole = "ade.role"
	// LabelNet ist die Version des Netz-Aufbaus (NetVersion).
	LabelNet = "ade.net"
	// LabelBinary ist das gemountete ade-Binary eines Proxy-Containers.
	LabelBinary = "ade.binary"
)

// Rollen der Container eines Workspaces.
const (
	RoleAgent = "agent"
	RoleProxy = "proxy"
)

// DetachKeys ersetzt Docker's Ctrl-P Ctrl-Q, damit die Tasten im Terminal ankommen.
const DetachKeys = "ctrl-@,ctrl-^,ctrl-@,ctrl-^"

// StopTimeout ist die Zeit, die docker stop den Prozessen im Container lässt.
const StopTimeout = 3 * time.Second

// Options konfiguriert den Manager.
type Options struct {
	// Docker ist das docker-Binary (Standard: docker im PATH).
	Docker string
	// DataDir kennzeichnet die Container dieser ADE-Instanz (Label ade.data_dir).
	DataDir string
	// Image ist das Standard-Image, wenn sandbox.yaml keins nennt.
	Image string
	// UID und GID für Prozesse im Container (Standard: die des Daemons).
	UID, GID int
	// ProxyBinary ist das ade-Binary auf dem Host, das im Proxy-Container läuft (statisch
	// gebaut, wird nur lesend gemountet).
	ProxyBinary string
	// ProxyImage ist das Image des Proxy-Containers (Standard: Image).
	ProxyImage string
	// Secrets liefert die API-Keys je Anbieter für die Secret-Injection des Proxys.
	Secrets func() map[string]string
	// OnEgress erhält die Egress-Events der Proxys.
	OnEgress func(workspace string, events []proxy.Event)
	// OnQuestion beantwortet Fragen der Proxys (Freigaben, M7). Es blockiert, bis
	// entschieden ist; ctx endet, wenn der Fragende aufgibt. Ohne Handler gilt deny.
	OnQuestion QuestionHandler
	// AskWait ist die Wartezeit einer Anfrage bei egress.on_blocked: ask (Standard 2m).
	AskWait time.Duration
	// ReloadInterval ist der Abstand, in dem geänderte Policies an die Proxys gehen.
	ReloadInterval time.Duration
	Logger         *slog.Logger
}

// Manager verwaltet die Container aller Workspaces.
type Manager struct {
	opts Options
	log  *slog.Logger

	mu     sync.Mutex
	locks  map[string]*sync.Mutex
	avail  *availability
	ensure map[string]time.Time // zuletzt als laufend gesehen
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

// New erzeugt einen Manager.
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
		// Nur 127.0.0.1, nie über einen Proxy aus der Umgebung des Daemons.
		http: &http.Client{Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4}},
	}
	m.bg, m.bgCancel = context.WithCancel(context.Background())
	return m
}

// Close beendet Hintergrundarbeit (Event-Abholung, Hot-Reload). Container bleiben, wie sie sind.
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

// Image liefert das Standard-Image.
func (m *Manager) Image() string { return m.opts.Image }

// ContainerName ist der Name des Containers eines Workspaces.
func ContainerName(workspace string) string { return "ade-ws-" + workspace }

// Error ist ein fehlgeschlagener docker-Aufruf mit dessen Fehlerausgabe.
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

// dockerEnv ist die Umgebung der Docker-CLI: nur, was sie zum Finden des Daemons braucht.
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

// reservedEnv meldet Variablen, die die Docker-CLI selbst nutzt. Sie werden an docker exec
// mit Wert übergeben, alle anderen ohne Wert (aus der Umgebung der CLI, damit Werte wie
// API-Keys nicht in der Prozessliste stehen).
func reservedEnv(k string) bool {
	switch k {
	case "PATH", "HOME", "USER", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME":
		return true
	}
	return strings.HasPrefix(k, "DOCKER_")
}

func (m *Manager) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, m.opts.Docker, args...) //nolint:gosec // G204: Argumente baut der Manager
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
			return nil, fmt.Errorf("Docker ist nicht installiert (%s nicht gefunden)", m.opts.Docker)
		}
		return stdout.Bytes(), &Error{Args: args, Stderr: stderr.String(), Err: err}
	}
	return stdout.Bytes(), nil
}

// Available prüft (höchstens alle 30 s), ob die Docker-CLI einen Docker-Daemon erreicht,
// und liefert dessen Version.
func (m *Manager) Available(ctx context.Context) (string, error) {
	m.mu.Lock()
	if a := m.avail; a != nil && time.Since(a.at) < 30*time.Second {
		m.mu.Unlock()
		return a.version, a.err
	}
	m.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := m.run(cctx, nil, "version", "--format", "{{.Server.Version}}")
	version := strings.TrimSpace(string(out))
	if err != nil {
		err = fmt.Errorf("Docker nicht erreichbar: %w", err)
	}
	m.mu.Lock()
	m.avail = &availability{at: time.Now(), version: version, err: err}
	m.mu.Unlock()
	return version, err
}

// Spec beschreibt den Container eines Workspaces.
type Spec struct {
	Workspace string
	// Path ist das Workspace-Verzeichnis auf dem Host (wird nach /workspace gemountet).
	Path   string
	Config Config
	// Env sind feste Variablen des Containers (z. B. Git-Identität), keine Secrets.
	Env []string
	// Grants sind Egress-Ausnahmen aus Freigaben im Dashboard (ergänzen egress.allow).
	Grants []string
}

// ImageFor liefert das Image für eine Konfiguration.
func (m *Manager) ImageFor(c Config) string {
	if c.Image != "" {
		return c.Image
	}
	return m.opts.Image
}

// Container beschreibt einen Container von ADE.
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
	// Net ist die Version des Netz-Aufbaus (leer: Container aus M5 mit normalem Netz).
	Net    string `json:"net,omitempty"`
	Binary string `json:"-"`
	token  string
	// Mounts sind die Ziele aller Mounts (für Tests und Diagnose).
	Mounts []Mount `json:"mounts,omitempty"`
}

// Mount ist ein Mount eines Containers.
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
		if errors.As(err, &de) && strings.Contains(strings.ToLower(de.Stderr), "no such") && len(refs) == 1 {
			return nil, nil
		}
		return nil, err
	}
	var raw []inspectJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("docker inspect lesen: %w", err)
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
			c.Role = RoleAgent // Container aus M5
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

// Get liefert den Container eines Workspaces oder nil, wenn es keinen gibt.
func (m *Manager) Get(ctx context.Context, workspace string) (*Container, error) {
	cs, err := m.inspect(ctx, ContainerName(workspace))
	if err != nil || len(cs) == 0 {
		return nil, err
	}
	c := cs[0]
	if c.DataDir != m.opts.DataDir || c.Workspace != workspace || c.Role != RoleAgent {
		return nil, fmt.Errorf("Container %s gehört nicht zu dieser ADE-Instanz", c.Name)
	}
	return &c, nil
}

// List liefert alle Container mit dem Label ade.managed – auch die anderer ADE-Instanzen.
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

// Ensure sorgt dafür, dass Netz, Proxy und Container des Workspaces existieren und laufen,
// und schickt die Policy an den Proxy. Fehlt ein Image lokal, lädt Docker es herunter (das
// kann dauern). Container aus M5 (normales Netz) werden dabei neu angelegt.
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

	c, err := m.Get(ctx, s.Workspace)
	if err != nil {
		return err
	}
	if c == nil {
		if _, err := m.Available(ctx); err != nil {
			return err
		}
	}
	netCreated, err := m.ensureNetworks(ctx, s.Workspace)
	if err != nil {
		return err
	}
	if c != nil && (c.Net != NetVersion || netCreated) {
		m.log.Info("Sandbox-Container wird mit dem aktuellen Aufbau neu angelegt", "workspace", s.Workspace, "bisher", c.Net)
		m.unlinkLocked(s.Workspace)
		if _, err := m.run(ctx, nil, "container", "rm", "--force", "--", c.ID); err != nil {
			return fmt.Errorf("alten Container entfernen: %w", err)
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
			return fmt.Errorf("Container starten: %w", err)
		}
		m.log.Info("Sandbox-Container gestartet", "workspace", s.Workspace)
	}
	if created {
		m.prepareUser(ctx, s.Workspace)
	}
	m.mu.Lock()
	m.ensure[s.Workspace] = time.Now()
	m.mu.Unlock()
	return nil
}

// CreateArgs liefert die Argumente für docker create (exportiert für Tests).
func (m *Manager) CreateArgs(s Spec) []string {
	r := s.Config.Resources
	args := []string{
		"container", "create",
		"--name", ContainerName(s.Workspace),
		"--label", LabelManaged + "=true",
		"--label", LabelWorkspace + "=" + s.Workspace,
		"--label", LabelDataDir + "=" + m.opts.DataDir,
		"--hostname", "ws-" + s.Workspace,
		"--user", strconv.Itoa(m.opts.UID) + ":" + strconv.Itoa(m.opts.GID),
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--init",
		"--mount", "type=bind,source=" + s.Path + ",target=" + Workdir,
		"--label", LabelRole + "=" + RoleAgent,
		"--label", LabelNet + "=" + NetVersion,
		"--network", NetworkName(s.Workspace),
		"--network-alias", AgentAlias,
		"--dns", noDNS,
		"--workdir", Workdir,
		"--env", "HOME=" + Home,
	}
	for _, kv := range proxyEnv() {
		args = append(args, "--env", kv)
	}
	if m.opts.ProxyBinary != "" {
		// Für ade hook pretooluse (Freigaben); nur lesend, das Binary enthält keine Secrets.
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

func (m *Manager) create(ctx context.Context, s Spec) error {
	if strings.ContainsAny(s.Path, ",\n") || !strings.HasPrefix(s.Path, "/") {
		return fmt.Errorf("Workspace-Pfad %q ist für einen Mount ungeeignet", s.Path)
	}
	m.log.Info("Sandbox-Container wird angelegt", "workspace", s.Workspace, "image", m.ImageFor(s.Config))
	if _, err := m.run(ctx, nil, m.CreateArgs(s)...); err != nil {
		return fmt.Errorf("Container anlegen: %w", err)
	}
	return nil
}

// prepareUser legt einen passwd-Eintrag an, falls die UID im Image fehlt (sonst zeigen
// Shells „I have no name!“ und manche Werkzeuge scheitern). Fehler sind nicht fatal.
func (m *Manager) prepareUser(ctx context.Context, workspace string) {
	uid, gid := strconv.Itoa(m.opts.UID), strconv.Itoa(m.opts.GID)
	script := `getent group "$2" >/dev/null || echo "ade$2:x:$2:" >> /etc/group
getent passwd "$1" >/dev/null || echo "ade$1:x:$1:$2:ADE:` + Home + `:/bin/bash" >> /etc/passwd`
	if _, err := m.run(ctx, nil, "exec", "--user", "0", ContainerName(workspace), "sh", "-c", script, "sh", uid, gid); err != nil {
		m.log.Debug("passwd-Eintrag im Container", "workspace", workspace, "err", err)
	}
}

// Exec beschreibt einen Prozess im Container.
type Exec struct {
	Argv []string
	// Env (KEY=VALUE) kommt zur Umgebung des Images hinzu.
	Env []string
	// TTY: mit Pseudo-Terminal (der Aufrufer startet die CLI selbst in einem PTY).
	TTY bool
	// Stdin: Eingaben weiterreichen.
	Stdin bool
	// Root: als root statt als Nutzer (nur für Vorbereitungen von ADE).
	Root bool
}

// ExecArgs liefert die Argumente für docker exec und die Umgebung der CLI.
func (m *Manager) ExecArgs(workspace string, e Exec) ([]string, []string) {
	args := []string{"exec"}
	if e.Stdin || e.TTY {
		args = append(args, "--interactive")
	}
	if e.TTY {
		args = append(args, "--tty", "--detach-keys", DetachKeys)
	}
	user := strconv.Itoa(m.opts.UID) + ":" + strconv.Itoa(m.opts.GID)
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
	args = append(args, ContainerName(workspace))
	return append(args, e.Argv...), env
}

// Command baut einen docker-exec-Aufruf. Endet ctx, wird nur die CLI beendet; Prozesse im
// Container beendet Signal.
func (m *Manager) Command(ctx context.Context, workspace string, e Exec) *exec.Cmd {
	args, env := m.ExecArgs(workspace, e)
	cmd := m.command(ctx, args...)
	cmd.Env = env
	return cmd
}

// signalScript sendet ein Signal an alle Prozesse im Container, deren Umgebung die Zeile
// $1 (KEY=VALUE) enthält – wie ein Signal an eine Prozessgruppe, nur ohne setsid.
const signalScript = `for d in /proc/[0-9]*; do
  p=${d#/proc/}
  [ "$p" = "$$" ] && continue
  if tr '\0' '\n' < "$d/environ" 2>/dev/null | grep -qxF -- "$1"; then kill -s "$2" "$p" 2>/dev/null; fi
done
exit 0`

// Signal sendet sig an alle Prozesse im Container mit der Umgebungsvariable marker
// (KEY=VALUE), z. B. ADE_SESSION_ID=<id>.
func (m *Manager) Signal(ctx context.Context, workspace, marker string, sig syscall.Signal) error {
	name := map[syscall.Signal]string{syscall.SIGTERM: "TERM", syscall.SIGKILL: "KILL", syscall.SIGINT: "INT", syscall.SIGHUP: "HUP"}[sig]
	if name == "" {
		return fmt.Errorf("Signal %v wird nicht unterstützt", sig)
	}
	if !strings.Contains(marker, "=") || strings.ContainsAny(marker, "\n\x00") {
		return fmt.Errorf("ungültige Markierung %q", marker)
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := m.run(cctx, nil, "exec", "--user", strconv.Itoa(m.opts.UID)+":"+strconv.Itoa(m.opts.GID),
		ContainerName(workspace), "sh", "-c", signalScript, "sh", marker, name)
	return err
}

// WriteFile schreibt eine Datei im Container (z. B. den Connection-Token der IDE), ohne
// dass ihr Inhalt in einer Prozessliste erscheint.
func (m *Manager) WriteFile(ctx context.Context, workspace, path string, data []byte) error {
	_, err := m.run(ctx, data, "exec", "--interactive", "--user", strconv.Itoa(m.opts.UID)+":"+strconv.Itoa(m.opts.GID),
		ContainerName(workspace), "sh", "-c", `umask 077 && mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", path)
	return err
}

// hostPort liefert den Port auf 127.0.0.1, auf den Docker port des Containers name abbildet.
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
	return 0, fmt.Errorf("Port %d des Containers ist nicht auf 127.0.0.1 veröffentlicht", port)
}

// Stop stoppt Container und Proxy eines Workspaces (die Dateien bleiben im Workspace).
func (m *Manager) Stop(ctx context.Context, workspace string) error {
	unlock := m.lock(workspace)
	defer unlock()
	m.unlinkLocked(workspace)
	return m.stop(ctx, ContainerName(workspace), ProxyName(workspace))
}

// unlinkLocked vergisst den Zustand eines Workspaces (Aufrufer hält dessen Lock).
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
		return nil // z. B. Workspace aus M5 ohne Proxy
	}
	return err
}

func (m *Manager) forget(workspace string) {
	m.mu.Lock()
	delete(m.ensure, workspace)
	m.mu.Unlock()
}

// Remove entfernt Container, Proxy und Netz eines Workspaces (soweit vorhanden).
func (m *Manager) Remove(ctx context.Context, workspace string) error {
	unlock := m.lock(workspace)
	defer unlock()
	m.unlinkLocked(workspace)
	c, err := m.Get(ctx, workspace)
	if err != nil {
		return err
	}
	p, err := m.getProxy(ctx, workspace)
	if err != nil {
		return err
	}
	var ids []string
	for _, x := range []*Container{c, p} {
		if x != nil {
			ids = append(ids, x.ID)
		}
	}
	if len(ids) > 0 {
		if _, err := m.run(ctx, nil, append([]string{"container", "rm", "--force", "--volumes", "--"}, ids...)...); err != nil {
			return fmt.Errorf("Container entfernen: %w", err)
		}
		m.log.Info("Sandbox-Container entfernt", "workspace", workspace)
	}
	nets, err := m.orphanNetworks(ctx, func(ws string) bool { return ws != workspace })
	if err != nil {
		return err
	}
	if err := m.removeNetworks(ctx, nets...); err != nil {
		return fmt.Errorf("Netz entfernen: %w", err)
	}
	return nil
}

// Orphans liefert verwaiste Container: solche dieser Instanz ohne Workspace (known liefert
// false) und solche anderer Instanzen, deren Datenverzeichnis nicht mehr existiert.
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
			// Ohne Instanz-Label nicht anfassen.
		default:
			if _, err := os.Stat(c.DataDir); errors.Is(err, os.ErrNotExist) {
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// GC entfernt verwaiste Container (Agent und Proxy, siehe Orphans) und danach verwaiste
// Workspace-Netze. Mit dryRun wird nur aufgelistet.
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
			return nil, fmt.Errorf("Container entfernen: %w", err)
		}
		m.log.Info("verwaiste Sandbox-Container entfernt", "anzahl", len(orphans))
	}
	nets, err := m.orphanNetworks(ctx, known)
	if err != nil {
		return orphans, err
	}
	if err := m.removeNetworks(ctx, nets...); err != nil {
		return orphans, fmt.Errorf("Netze entfernen: %w", err)
	}
	return orphans, nil
}

// Recover gleicht nach einem Daemon-Start ab: verwaiste Container entfernen, laufende
// Container eigener Workspaces stoppen (beendet Agent- und IDE-Prozesse des letzten Laufs).
// Sie starten wieder, sobald sie gebraucht werden.
func (m *Manager) Recover(ctx context.Context, known func(workspace string) bool) error {
	if _, err := m.Available(ctx); err != nil {
		return err
	}
	if _, err := m.GC(ctx, known, false); err != nil {
		return err
	}
	return m.StopAll(ctx)
}

// StopAll stoppt alle laufenden Container dieser Instanz.
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
		m.log.Info("Sandbox-Container werden gestoppt", "anzahl", len(running))
	}
	return m.stop(ctx, running...)
}

// HasMount meldet, ob c ein Mount mit Ziel dst hat (Hilfe für Tests).
func (c *Container) HasMount(dst string) bool {
	return slices.ContainsFunc(c.Mounts, func(mt Mount) bool { return mt.Destination == dst })
}
