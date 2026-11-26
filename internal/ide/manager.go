// Package ide verwaltet die Browser-IDE (openvscode-server) je Workspace: bei Bedarf
// herunterladen, starten, über das Gateway erreichbar machen und bei Inaktivität stoppen
// (doc/architecture.md#24-browser-ide). Ab M5 läuft sie im Sandbox-Container.
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

// Zustände einer IDE.
const (
	StateStopped    = "stopped"
	StateInstalling = "installing"
	StateStarting   = "starting"
	StateRunning    = "running"
	StateError      = "error"
)

// Umgebungsvariablen, die ADE dem IDE-Prozess setzt. EnvOwner erkennt verwaiste Prozesse
// nach einem Absturz (wie ADE_SESSION_ID bei Agenten) und trennt mehrere Daemons.
const (
	EnvOwner         = "ADE_IDE_OWNER"
	EnvWorkspaceID   = agent.EnvWorkspaceID
	EnvWorkspacePath = agent.EnvWorkspacePath
)

// Voreinstellungen für Zeitgrenzen.
const (
	DefaultStartTimeout = 60 * time.Second
	DefaultStopTimeout  = 5 * time.Second
	DefaultIdleTimeout  = 15 * time.Minute
)

// WorkspaceFunc liefert den Pfad des Arbeitsverzeichnisses eines Workspaces. Für unbekannte
// Workspaces liefert sie einen apperr.NotFound-Fehler, für solche ohne Arbeitsverzeichnis
// apperr.Conflict.
type WorkspaceFunc func(ctx context.Context, id string) (string, error)

// Sandbox führt Befehle im Docker-Container eines Workspaces aus (ab M5). Die IDE läuft
// dann dort: openvscode-server aus dem Image, Port per Docker auf 127.0.0.1 veröffentlicht.
type Sandbox interface {
	// Command baut einen Prozess im Container (läuft im Workspace-Verzeichnis).
	Command(ctx context.Context, argv, env []string) *exec.Cmd
	// Signal sendet sig an alle Prozesse im Container mit der Variable marker (KEY=VALUE).
	Signal(ctx context.Context, marker string, sig syscall.Signal) error
	// HostPort liefert den Port auf 127.0.0.1 für port im Container.
	HostPort(ctx context.Context, port int) (int, error)
	// WriteFile schreibt eine Datei im Container, nur für den Nutzer lesbar.
	WriteFile(ctx context.Context, path string, data []byte) error
}

// SandboxFunc liefert die Sandbox eines Workspaces (nil: IDE läuft lokal). Sie startet den
// Container bei Bedarf.
type SandboxFunc func(ctx context.Context, id string) (Sandbox, error)

// Pfade und Port der IDE im Container.
const (
	sandboxPort      = 3000
	sandboxTokenFile = "/home/ade/.ade/ide-connection-token" //nolint:gosec // G101: Pfad, kein Secret
)

// Options konfiguriert den Manager.
type Options struct {
	// Dir enthält die heruntergeladene IDE, gemeinsame Erweiterungen und Daten je Workspace.
	Dir string
	// Binary ist ein eigener openvscode-server; leer = festgelegte Version herunterladen.
	Binary     string
	Workspaces WorkspaceFunc
	// Sandbox ist optional; ohne sie laufen alle IDEs lokal.
	Sandbox SandboxFunc
	Logger  *slog.Logger

	IdleTimeout  time.Duration
	StartTimeout time.Duration
	StopTimeout  time.Duration
	// Env ist die Umgebung, aus der die IDE erbt (Default: os.Environ). ADE gibt davon nur
	// dieselben Variablen weiter wie an Agenten (agent.FilterEnv).
	Env []string
	// ExtraEnv wird ungefiltert angehängt (Tests).
	ExtraEnv []string
	// HTTPClient lädt das Release herunter (Tests setzen einen eigenen).
	HTTPClient *http.Client
	// release überschreibt das Download-Ziel (Tests).
	release *release
}

// Status beschreibt den Zustand der IDE eines Workspaces.
type Status struct {
	State     string     `json:"state"`
	Error     string     `json:"error,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	// Connections ist die Zahl offener Verbindungen über das Gateway.
	Connections int `json:"connections"`
}

// Manager verwaltet die IDEs aller Workspaces.
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
	sb      Sandbox // gesetzt, wenn die IDE im Container läuft

	// ctx endet mit Stop; er bricht Download und Startwarten ab, nicht den Prozess selbst.
	ctx    context.Context
	cancel context.CancelFunc

	ready    chan struct{} // geschlossen, sobald running oder fehlgeschlagen
	exited   chan struct{} // geschlossen, wenn der Prozess beendet ist
	stopping bool

	conns      int
	lastActive time.Time
}

// NewManager erzeugt einen Manager. Er startet nichts, bis eine IDE angefordert wird.
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

// Recover beendet IDE-Prozesse, die ein früherer Lauf dieses Daemons hinterlassen hat.
func (m *Manager) Recover() {
	if n := killOwned(m.opts.Dir); n > 0 {
		m.log.Warn("verwaiste IDE-Prozesse beendet", "anzahl", n)
	}
}

// Close stoppt alle IDEs und wartet auf ihr Ende.
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

// Status liefert den Zustand der IDE eines Workspaces.
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

// Start startet die IDE eines Workspaces, falls sie nicht schon läuft oder startet, und
// kehrt sofort zurück. Den Fortschritt liefert Status.
func (m *Manager) Start(ctx context.Context, id string) (Status, error) {
	if m.ctx.Err() != nil {
		return Status{}, errors.New("Daemon wird beendet")
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
	go m.run(in, path) //nolint:gosec // G118: Die IDE überdauert die Anfrage; sie endet mit in.ctx
	return m.statusLocked(id), nil
}

// Upstream ist eine laufende IDE, an die das Gateway eine Anfrage weiterreicht.
type Upstream struct {
	URL   *url.URL
	Token string
	// Release meldet das Ende der Anfrage bzw. der WebSocket-Verbindung.
	Release func()
}

// Acquire liefert die laufende IDE eines Workspaces und zählt die Verbindung, bis Release
// aufgerufen wird. Läuft die IDE nicht, startet Acquire sie und wartet, bis sie bereit ist
// oder ctx endet.
func (m *Manager) Acquire(ctx context.Context, id string) (*Upstream, error) {
	if _, err := m.Start(ctx, id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	in := m.inst[id]
	m.mu.Unlock()
	if in == nil {
		return nil, apperr.Conflict("IDE wurde gestoppt")
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
			msg = "IDE läuft nicht"
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

// Stop beendet die IDE eines Workspaces (SIGTERM an die Prozessgruppe, nach StopTimeout
// SIGKILL) und wartet auf ihr Ende.
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
		<-in.exited // Start läuft noch (Download o. Ä.) und bricht über in.ctx ab
	}
	m.mu.Lock()
	if m.inst[id] == in {
		delete(m.inst, id)
	}
	m.mu.Unlock()
}

// BeforeWorkspaceDelete stoppt die IDE und entfernt ihre Daten.
func (m *Manager) BeforeWorkspaceDelete(_ context.Context, id string) error {
	m.Stop(id)
	if err := os.RemoveAll(m.dataDir(id)); err != nil {
		return fmt.Errorf("IDE-Daten löschen: %w", err)
	}
	return nil
}

func (m *Manager) dataDir(id string) string { return filepath.Join(m.opts.Dir, "workspaces", id) }

// run installiert bei Bedarf, startet den Prozess, wartet auf Bereitschaft und danach
// auf das Prozessende.
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
		m.log.Error("IDE fehlgeschlagen", "workspace", in.wsID, "err", err)
	}

	var sb Sandbox
	if m.opts.Sandbox != nil {
		var err error
		if sb, err = m.opts.Sandbox(in.ctx, in.wsID); err != nil {
			fail(fmt.Errorf("Sandbox-Container: %w", err))
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
		fail(fmt.Errorf("openvscode-server starten: %w", err))
		return
	}
	in.pid = cmd.Process.Pid
	m.mu.Unlock()
	if sb != nil {
		// Den veröffentlichten Port kennt Docker erst nach dem Container-Start.
		port, err := sb.HostPort(in.ctx, sandboxPort)
		if err != nil {
			_ = sb.Signal(context.Background(), EnvOwner+"="+m.opts.Dir, syscall.SIGKILL)
			_ = syscall.Kill(-in.pid, syscall.SIGKILL)
			_ = cmd.Wait()
			fail(fmt.Errorf("IDE-Port: %w", err))
			return
		}
		m.mu.Lock()
		in.port, in.sb = port, sb
		m.mu.Unlock()
	}
	m.log.Info("IDE gestartet", "workspace", in.wsID, "pid", in.pid, "port", in.port, "sandbox", sb != nil)

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
	// Reste der Prozessgruppe (z. B. Extension-Hosts) mitnehmen.
	killAll()
	m.mu.Lock()
	defer m.mu.Unlock()
	if in.stopping {
		in.state = StateStopped
		m.log.Info("IDE gestoppt", "workspace", in.wsID)
		return
	}
	in.state = StateError
	in.err = "openvscode-server unerwartet beendet"
	if err != nil {
		in.err += ": " + err.Error()
	}
	if out := strings.TrimSpace(stderr.String()); out != "" {
		in.err += " – " + lastLine(out)
	}
	m.log.Error("IDE unerwartet beendet", "workspace", in.wsID, "err", in.err)
}

// waitReady fragt den Port ab, bis der Server antwortet, der Prozess endet oder
// StartTimeout abläuft.
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
			waitErr <- err // für run wieder einreihen
			return fmt.Errorf("openvscode-server beim Start beendet: %w", err)
		case <-deadline.C:
			return fmt.Errorf("openvscode-server antwortet nach %s nicht", m.opts.StartTimeout)
		case <-in.ctx.Done():
			return errors.New("Start abgebrochen")
		case <-tick.C:
			resp, err := client.Get(u) //nolint:gosec // G107: lokale Adresse
			if err == nil {
				_ = resp.Body.Close()
				return nil
			}
		}
	}
}

// binary liefert den Pfad des openvscode-servers und lädt ihn bei Bedarf herunter.
// Mehrere gleichzeitige Starts teilen sich einen Download.
func (m *Manager) binary(in *instance) (string, error) {
	if m.opts.Binary != "" {
		if !fileExists(m.opts.Binary) {
			return "", fmt.Errorf("ide.binary %s nicht gefunden", m.opts.Binary)
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
			m.log.Info("lade openvscode-server herunter", "version", Version, "url", rel.URL)
			run.path, run.err = install(m.ctx, m.opts.HTTPClient, m.opts.Dir, rel)
			if run.err == nil {
				m.log.Info("openvscode-server installiert", "pfad", run.path)
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
		return "", errors.New("Start abgebrochen")
	}

	m.mu.Lock()
	if in.state == StateInstalling {
		in.state = StateStarting
	}
	m.mu.Unlock()
	return run.path, run.err
}

// command baut den Aufruf. Den Connection-Token liest der Server aus einer Datei, damit er
// nicht in der Prozessliste steht.
func (m *Manager) command(in *instance, bin, workdir string) (*exec.Cmd, error) {
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("freien Port finden: %w", err)
	}
	data := m.dataDir(in.wsID)
	if err := os.MkdirAll(data, 0o700); err != nil {
		return nil, fmt.Errorf("IDE-Datenverzeichnis anlegen: %w", err)
	}
	ext := filepath.Join(m.opts.Dir, "extensions")
	if err := os.MkdirAll(ext, 0o700); err != nil {
		return nil, fmt.Errorf("Erweiterungsverzeichnis anlegen: %w", err)
	}
	token := randomToken()
	tokenFile := filepath.Join(data, "connection-token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		return nil, fmt.Errorf("Connection-Token schreiben: %w", err)
	}

	cmd := exec.CommandContext(m.ctx, bin, //nolint:gosec // G204: Binary aus Konfiguration bzw. geprüftem Download
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
	// Beim Daemon-Ende stoppt Close die IDE geordnet; CommandContext ist nur die Rückfallebene.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

	m.mu.Lock()
	in.port, in.token = port, token
	m.mu.Unlock()
	return cmd, nil
}

// sandboxCommand baut den Aufruf von openvscode-server im Container. Der Connection-Token
// liegt in einer Datei im Container; der Server lauscht auf allen Adressen des Containers,
// erreichbar ist er von außen nur über den auf 127.0.0.1 veröffentlichten Port.
func (m *Manager) sandboxCommand(in *instance, sb Sandbox) (*exec.Cmd, error) {
	token := randomToken()
	if err := sb.WriteFile(in.ctx, sandboxTokenFile, []byte(token)); err != nil {
		return nil, fmt.Errorf("Connection-Token im Container schreiben: %w", err)
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

// reaper stoppt IDEs ohne Verbindung, die länger als IdleTimeout inaktiv sind.
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
				delete(m.inst, id) // Fehler nicht ewig anzeigen
			}
		}
		m.mu.Unlock()
		for _, id := range idle {
			m.log.Info("IDE inaktiv, wird gestoppt", "workspace", id)
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

// tail behält die letzten 4 KiB der Ausgabe.
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

// procDir ist das proc-Dateisystem (Tests setzen ein eigenes Verzeichnis).
var procDir = "/proc"

// killOwned beendet alle Prozesse, deren Umgebung EnvOwner=dir enthält, und liefert ihre Zahl.
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
