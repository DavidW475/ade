// Package workspace legt Workspaces als lokale Git-Clones an (ADR-005), führt deren
// Setup-Befehle aus, berechnet Diffs und räumt sie wieder ab.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/git"
	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/sandbox"
	"github.com/DavidW475/ade/internal/store"
)

// Grenzen für Setup-Befehle.
const (
	SetupTimeout = 15 * time.Minute
	MaxSetupLog  = 64 << 10
)

// BranchPrefix ist das Präfix der Workspace-Branches.
const BranchPrefix = "ade/"

// Manager verwaltet die Workspaces aller Projekte.
type Manager struct {
	store    *store.Store
	projects *project.Service
	dir      string
	log      *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu    sync.Mutex
	setup map[string]*setupRun // laufende Setups je Workspace

	// beforeDelete läuft vor dem Löschen eines Workspaces (z. B. Agent-Sessions beenden).
	beforeDelete func(ctx context.Context, id string) error
	// busyCheck läuft vor Mergen und PR (review.go).
	busyCheck func(ctx context.Context, id string) error
	gh        string
	locks     map[string]*wsLock

	// sandbox ist nil, wenn der Daemon ohne Docker-Sandbox läuft.
	sandbox        *sandbox.Manager
	sandboxDefault bool
}

type setupRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// NewManager erzeugt einen Manager, der Workspaces unter dir anlegt.
func NewManager(st *store.Store, projects *project.Service, dir string, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{store: st, projects: projects, dir: dir, log: log, ctx: ctx, cancel: cancel,
		setup: map[string]*setupRun{}, gh: "gh", locks: map[string]*wsLock{}}
}

// Recover markiert Workspaces, deren Anlegen oder Setup beim letzten Lauf unterbrochen wurde.
func (m *Manager) Recover(ctx context.Context) error {
	n, err := m.store.MarkInterruptedWorkspaces(ctx)
	if n > 0 {
		m.log.Warn("unterbrochene Workspaces markiert", "anzahl", n)
	}
	return err
}

// SetBeforeDelete registriert eine Funktion, die vor dem Löschen eines Workspaces läuft.
// Liefert sie einen Fehler, wird nicht gelöscht.
func (m *Manager) SetBeforeDelete(fn func(ctx context.Context, id string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.beforeDelete = fn
}

// WaitSetup wartet, bis ein laufendes Setup des Workspaces fertig ist (sofort, wenn keins läuft).
func (m *Manager) WaitSetup(ctx context.Context, id string) error {
	m.mu.Lock()
	run := m.setup[id]
	m.mu.Unlock()
	if run == nil {
		return nil
	}
	select {
	case <-run.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close bricht laufende Setups ab und wartet auf sie.
func (m *Manager) Close() {
	m.cancel()
	m.wg.Wait()
}

// SetSandbox aktiviert die Docker-Sandbox; def ist die Voreinstellung für neue Workspaces
// (defaults.sandbox).
func (m *Manager) SetSandbox(sb *sandbox.Manager, def bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandbox, m.sandboxDefault = sb, def
}

// Sandbox liefert den Sandbox-Manager (nil ohne Sandbox).
func (m *Manager) Sandbox() *sandbox.Manager {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sandbox
}

// SandboxDefault meldet die Voreinstellung für neue Workspaces.
func (m *Manager) SandboxDefault() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sandboxDefault
}

// CreateOptions sind Optionen für einen neuen Workspace.
type CreateOptions struct {
	// BaseBranch überschreibt den Basis-Branch des Projekts.
	BaseBranch string `json:"base_branch,omitempty"`
	// Sandbox: Agent, Setup und IDE im Docker-Container (nil: defaults.sandbox).
	Sandbox *bool `json:"sandbox,omitempty"`
}

// Create legt einen Workspace für das Projekt an: lokaler Clone, eigener Branch
// ade/<id> ab dem Basis-Branch. Setup-Befehle laufen danach im Hintergrund.
func (m *Manager) Create(ctx context.Context, projectID string, opts CreateOptions) (*store.Workspace, error) {
	p, err := m.projects.Get(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if p.ConfigError != "" {
		return nil, apperr.Invalid("Projekt-Konfiguration ungültig: %s", p.ConfigError)
	}
	base := opts.BaseBranch
	if base == "" {
		base = p.BaseBranch
	}
	if !git.ValidBranchName(ctx, base) {
		return nil, apperr.Invalid("%q ist kein gültiger Branch-Name", base)
	}
	commit, err := git.ResolveCommit(ctx, p.RepoPath, "refs/heads/"+base)
	if err != nil {
		return nil, apperr.Invalid("Basis-Branch %q gibt es im Repository nicht", base)
	}

	useSandbox := m.SandboxDefault()
	if opts.Sandbox != nil {
		useSandbox = *opts.Sandbox
	}
	image := ""
	if useSandbox {
		sb := m.Sandbox()
		if sb == nil {
			return nil, apperr.Invalid("die Docker-Sandbox ist nicht aktiviert")
		}
		if p.SandboxError != "" {
			return nil, apperr.Invalid("Sandbox-Konfiguration ungültig: %s", p.SandboxError)
		}
		if _, err := sb.Available(ctx); err != nil {
			return nil, apperr.Invalid("%v – ohne Sandbox starten oder Docker einrichten", err)
		}
		image = sb.ImageFor(*p.Sandbox)
	}

	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return nil, fmt.Errorf("Workspace-Verzeichnis anlegen: %w", err)
	}
	id := project.NewID()
	w := &store.Workspace{
		ID:         id,
		ProjectID:  p.ID,
		Path:       filepath.Join(m.dir, id),
		Branch:     BranchPrefix + id,
		BaseBranch: base,
		BaseCommit: commit,
		Status:     store.WorkspaceCreating,

		Sandbox:      useSandbox,
		SandboxImage: image,
	}
	if err := m.store.CreateWorkspace(ctx, w); err != nil {
		return nil, err
	}

	if err := clone(ctx, p.RepoPath, w); err != nil {
		_ = removeAll(w.Path)
		_ = m.store.DeleteWorkspace(context.WithoutCancel(ctx), id)
		return nil, err
	}

	status := store.WorkspaceReady
	if len(p.Config.Setup) > 0 || w.Sandbox {
		// Bei der Sandbox gehört das Anlegen des Containers (ggf. mit Image-Download) zum Setup.
		status = store.WorkspaceSetup
	}
	if err := m.store.UpdateWorkspace(ctx, id, store.WorkspaceUpdate{Status: &status}); err != nil {
		return nil, err
	}
	if status == store.WorkspaceSetup {
		m.startSetup(w, p.Config.Setup)
	}

	m.log.Info("Workspace angelegt", "id", id, "projekt", p.Name, "branch", w.Branch, "basis", base)
	return m.store.GetWorkspace(ctx, id)
}

// clone legt den Clone an und entfernt danach die Verbindung zum Haupt-Repo, damit ein
// Agent nicht dorthin pushen kann. Ergebnisse holt später der Daemon per fetch (ADR-005).
//
// Liegen Repo und Workspace-Verzeichnis auf verschiedenen Dateisystemen, scheitern die
// Hardlinks von --local (EXDEV); dann kopiert git die Objekte (--no-hardlinks). Sandbox-
// Workspaces klonen immer ohne Hardlinks: Der Container könnte sonst Objektdateien
// überschreiben, die sich Workspace und Haupt-Repo teilen.
func clone(ctx context.Context, repo string, w *store.Workspace) error {
	cloneArgs := func(hardlinks bool) []string {
		args := []string{"clone", "--local", "--quiet"}
		if !hardlinks {
			args = append(args, "--no-hardlinks")
		}
		return append(args, "--branch", w.BaseBranch, "--", repo, w.Path)
	}
	hardlinks := !w.Sandbox && git.SameDevice(repo, filepath.Dir(w.Path))
	_, err := git.Run(ctx, git.Cmd{Args: cloneArgs(hardlinks)})
	if err != nil && hardlinks && isCrossDevice(err) {
		_ = removeAll(w.Path)
		_, err = git.Run(ctx, git.Cmd{Args: cloneArgs(false)})
	}
	if err != nil {
		return fmt.Errorf("Workspace anlegen: %w", err)
	}
	steps := []git.Cmd{
		{Dir: w.Path, Args: []string{"remote", "remove", "origin"}},
		{Dir: w.Path, Args: []string{"switch", "--quiet", "--create", w.Branch, w.BaseCommit}},
	}
	for _, c := range steps {
		if _, err := git.Run(ctx, c); err != nil {
			return fmt.Errorf("Workspace anlegen: %w", err)
		}
	}
	return nil
}

func isCrossDevice(err error) bool {
	var ge *git.Error
	return errors.As(err, &ge) && strings.Contains(ge.Stderr, "cross-device")
}

func (m *Manager) startSetup(w *store.Workspace, cmds []string) {
	ctx, cancel := context.WithCancel(m.ctx)
	run := &setupRun{cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	m.setup[w.ID] = run
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(run.done)
		defer cancel()
		defer func() {
			m.mu.Lock()
			delete(m.setup, w.ID)
			m.mu.Unlock()
		}()

		var log string
		var err error
		if w.Sandbox {
			if err = m.EnsureSandbox(ctx, w.ID); err != nil {
				err = fmt.Errorf("Sandbox-Container: %w", err)
				log = err.Error() + "\n"
			}
		}
		if err == nil {
			log, err = m.runSetup(ctx, w, cmds)
		}
		u := store.WorkspaceUpdate{SetupLog: &log, Status: store.Ptr(store.WorkspaceReady), Error: store.Ptr("")}
		if err != nil {
			u.Status, u.Error = store.Ptr(store.WorkspaceSetupFailed), store.Ptr(err.Error())
			m.log.Warn("Workspace-Setup fehlgeschlagen", "id", w.ID, "err", err)
		}
		// Auch beim Abbruch (Daemon beendet) den Stand speichern.
		if err := m.store.UpdateWorkspace(context.WithoutCancel(ctx), w.ID, u); err != nil && !errors.Is(err, apperr.ErrNotFound) {
			m.log.Error("Workspace-Status speichern", "id", w.ID, "err", err)
		}
	}()
}

// envSetup kennzeichnet Setup-Prozesse im Container (zum Beenden beim Abbruch).
const envSetup = "ADE_SETUP"

func (m *Manager) runSetup(ctx context.Context, w *store.Workspace, cmds []string) (string, error) {
	var out tailBuffer
	sb := m.Sandbox()
	for i, c := range cmds {
		_, _ = fmt.Fprintf(&out, "$ %s\n", c)
		cctx, cancel := context.WithTimeout(ctx, SetupTimeout)
		var cmd *exec.Cmd
		stop := func() bool { return false }
		if w.Sandbox {
			marker := envSetup + "=" + w.ID
			// Im Container: Setup-Befehle kommen aus dem Repo und gelten als nicht vertrauenswürdig.
			cmd = sb.Command(cctx, w.ID, sandbox.Exec{
				Argv: []string{"sh", "-c", c},
				Env:  []string{"ADE_WORKSPACE_ID=" + w.ID, "ADE_WORKSPACE_PATH=" + sandbox.Workdir, "CI=1", marker},
			})
			stop = context.AfterFunc(cctx, func() {
				_ = sb.Signal(context.Background(), w.ID, marker, syscall.SIGKILL)
			})
		} else {
			cmd = exec.CommandContext(cctx, "sh", "-c", c)
			cmd.Dir = w.Path
			cmd.Env = append(os.Environ(), "ADE_WORKSPACE_ID="+w.ID, "ADE_WORKSPACE_PATH="+w.Path, "CI=1")
		}
		cmd.Stdout, cmd.Stderr = &out, &out
		// Eigene Prozessgruppe: Beim Abbruch werden auch Kindprozesse beendet.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = 5 * time.Second
		err := cmd.Run()
		stop()
		timedOut := errors.Is(cctx.Err(), context.DeadlineExceeded)
		cancel()
		switch {
		case ctx.Err() != nil:
			return out.String(), fmt.Errorf("Setup abgebrochen bei Befehl %d (%s)", i+1, c)
		case timedOut:
			return out.String(), fmt.Errorf("Setup-Befehl %d (%s) nach %s abgebrochen", i+1, c, SetupTimeout)
		case err != nil:
			if ee, ok := errors.AsType[*exec.ExitError](err); ok && ee.ExitCode() == 127 {
				hint := "Programm nicht gefunden"
				if w.Sandbox {
					hint += " – fehlt das Werkzeug im Sandbox-Image? (image in .ade/sandbox.yaml)"
				}
				return out.String(), fmt.Errorf("Setup-Befehl %d (%s) fehlgeschlagen: %w (%s)", i+1, c, err, hint)
			}
			return out.String(), fmt.Errorf("Setup-Befehl %d (%s) fehlgeschlagen: %w", i+1, c, err)
		}
	}
	return out.String(), nil
}

// Get liefert einen Workspace.
func (m *Manager) Get(ctx context.Context, id string) (*store.Workspace, error) {
	return m.store.GetWorkspace(ctx, id)
}

// Summary fasst die Änderungen eines Workspaces zusammen.
type Summary struct {
	Files     int `json:"files"`
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`
}

// View ist ein Workspace mit Änderungsübersicht.
type View struct {
	*store.Workspace
	Changes   *Summary `json:"changes,omitempty"`
	DiffError string   `json:"diff_error,omitempty"`
}

// List liefert die Workspaces eines Projekts samt Anzahl geänderter Dateien.
func (m *Manager) List(ctx context.Context, projectID string) ([]*View, error) {
	if _, err := m.store.GetProject(ctx, projectID); err != nil {
		return nil, err
	}
	ws, err := m.store.ListWorkspaces(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]*View, len(ws))
	for i, w := range ws {
		out[i] = m.view(ctx, w)
	}
	return out, nil
}

// GetView liefert einen Workspace samt Änderungsübersicht.
func (m *Manager) GetView(ctx context.Context, id string) (*View, error) {
	w, err := m.store.GetWorkspace(ctx, id)
	if err != nil {
		return nil, err
	}
	return m.view(ctx, w), nil
}

func (m *Manager) view(ctx context.Context, w *store.Workspace) *View {
	v := &View{Workspace: w}
	if !hasWorktree(w) {
		return v
	}
	wt, err := m.worktree(ctx, w, false)
	if err != nil {
		v.DiffError = err.Error()
		return v
	}
	if wt == nil {
		return v // Sandbox gestoppt: nicht nur für die Übersicht starten
	}
	d, err := git.WorktreeDiff(ctx, *wt, w.BaseCommit, false)
	if err != nil {
		v.DiffError = err.Error()
		return v
	}
	v.Changes = &Summary{Files: len(d.Files), Additions: d.Additions, Deletions: d.Deletions}
	return v
}

func hasWorktree(w *store.Workspace) bool {
	switch w.Status {
	case store.WorkspaceReady, store.WorkspaceSetup, store.WorkspaceSetupFailed:
		_, err := os.Stat(filepath.Join(w.Path, ".git"))
		return err == nil
	}
	return false
}

// Diff vergleicht den Arbeitsbaum des Workspaces mit seinem Basis-Commit.
func (m *Manager) Diff(ctx context.Context, id string, withPatch bool) (*git.Diff, error) {
	w, err := m.store.GetWorkspace(ctx, id)
	if err != nil {
		return nil, err
	}
	if !hasWorktree(w) {
		return nil, apperr.Conflict("Workspace %s hat keinen Arbeitsbaum (Status %s)", id, w.Status)
	}
	wt, err := m.worktree(ctx, w, true)
	if err != nil {
		return nil, err
	}
	return git.WorktreeDiff(ctx, *wt, w.BaseCommit, withPatch)
}

// sandboxSpec baut die Container-Beschreibung eines Workspaces aus der aktuellen
// Projekt-Konfiguration (sandbox.yaml im Haupt-Repo, nie aus dem Workspace).
func (m *Manager) sandboxSpec(ctx context.Context, w *store.Workspace) (sandbox.Spec, error) {
	p, err := m.projects.Get(ctx, w.ProjectID)
	if err != nil {
		return sandbox.Spec{}, err
	}
	cfg := *p.Sandbox
	if w.SandboxImage != "" {
		cfg.Image = w.SandboxImage
	}
	var env []string
	// Git-Identität des Nutzers, damit Commits des Agenten im Container einen Autor haben.
	for _, kv := range [][2]string{{"user.name", "NAME"}, {"user.email", "EMAIL"}} {
		if v, _ := git.Output(ctx, p.RepoPath, "config", kv[0]); v != "" && !strings.ContainsAny(v, "\n\x00") {
			env = append(env, "GIT_AUTHOR_"+kv[1]+"="+v, "GIT_COMMITTER_"+kv[1]+"="+v)
		}
	}
	return sandbox.Spec{Workspace: w.ID, Path: w.Path, Config: cfg, Env: env}, nil
}

// EnsureSandbox legt den Container eines Sandbox-Workspaces an bzw. startet ihn.
func (m *Manager) EnsureSandbox(ctx context.Context, id string) error {
	w, err := m.store.GetWorkspace(ctx, id)
	if err != nil {
		return err
	}
	if !w.Sandbox {
		return nil
	}
	sb := m.Sandbox()
	if sb == nil {
		return apperr.Conflict("Workspace %s läuft in der Sandbox, aber Docker ist nicht aktiviert", id)
	}
	spec, err := m.sandboxSpec(ctx, w)
	if err != nil {
		return err
	}
	return sb.Ensure(ctx, spec)
}

// worktree liefert den Ort, an dem der Daemon git im Workspace ausführt: bei Sandbox-
// Workspaces im Container (Issue #41). Mit start=false liefert er nil, wenn der Container
// nicht läuft.
func (m *Manager) worktree(ctx context.Context, w *store.Workspace, start bool) (*git.Worktree, error) {
	if !w.Sandbox {
		wt := git.Local(w.Path)
		return &wt, nil
	}
	sb := m.Sandbox()
	if sb == nil {
		return nil, apperr.Conflict("Workspace %s läuft in der Sandbox, aber Docker ist nicht aktiviert", w.ID)
	}
	if start {
		if err := m.EnsureSandbox(ctx, w.ID); err != nil {
			return nil, err
		}
	} else if c, err := sb.Get(ctx, w.ID); err != nil || c == nil || !c.Running {
		return nil, err
	}
	return &git.Worktree{Path: w.Path, Runner: func(ctx context.Context, env, argv []string) *exec.Cmd {
		return sb.Command(ctx, w.ID, sandbox.Exec{Argv: argv, Env: env})
	}}, nil
}

// Delete („Verwerfen“) bricht ein laufendes Setup ab, löscht den Branch ade/<id> im
// Haupt-Repo (falls übernommen), das Verzeichnis und den Datensatz. Ein schon gepushter
// Branch und ein Pull Request bleiben auf dem Remote.
func (m *Manager) Delete(ctx context.Context, id string) error {
	w, err := m.store.GetWorkspace(ctx, id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	run, before := m.setup[id], m.beforeDelete
	m.mu.Unlock()
	if before != nil {
		if err := before(ctx, id); err != nil {
			return err
		}
	}
	if run != nil {
		run.cancel()
		<-run.done
	}

	if err := m.checkPath(w); err != nil {
		return err
	}
	// Nach beforeDelete: Ein WIP-Commit beendeter Sessions kann den Branch noch anlegen.
	unlock := m.lockWorkspace(id)
	defer unlock()
	if p, err := m.store.GetProject(ctx, w.ProjectID); err == nil {
		if wt, _ := git.CheckedOutIn(ctx, p.RepoPath, w.Branch); wt != "" {
			return apperr.Conflict("Branch %s ist in %s ausgecheckt – dort erst einen anderen Branch auschecken", w.Branch, wt)
		}
		if err := git.DeleteBranch(ctx, p.RepoPath, w.Branch); err != nil {
			if _, statErr := os.Stat(p.RepoPath); statErr == nil {
				return fmt.Errorf("Branch %s im Projekt-Repo löschen: %w", w.Branch, err)
			}
			m.log.Warn("Projekt-Repo nicht erreichbar, Branch bleibt", "id", id, "repo", p.RepoPath, "err", err)
		}
	}
	if w.Sandbox {
		if sb := m.Sandbox(); sb != nil {
			if err := sb.Remove(ctx, id); err != nil {
				return fmt.Errorf("Sandbox-Container entfernen: %w", err)
			}
		}
	}
	if err := removeAll(w.Path); err != nil {
		return fmt.Errorf("Workspace-Verzeichnis löschen: %w", err)
	}
	if err := m.store.DeleteWorkspace(ctx, id); err != nil {
		return err
	}
	m.log.Info("Workspace gelöscht", "id", id)
	return nil
}

// DeleteAll löscht alle Workspaces eines Projekts.
func (m *Manager) DeleteAll(ctx context.Context, projectID string) error {
	ws, err := m.store.ListWorkspaces(ctx, projectID)
	if err != nil {
		return err
	}
	for _, w := range ws {
		if err := m.Delete(ctx, w.ID); err != nil {
			return err
		}
	}
	return nil
}

// checkPath stellt sicher, dass nur Verzeichnisse direkt unter dem Workspace-Verzeichnis
// gelöscht werden – nie etwas anderes, auch nicht bei manipulierten Datensätzen.
func (m *Manager) checkPath(w *store.Workspace) error {
	rel, err := filepath.Rel(m.dir, w.Path)
	if err != nil || rel != w.ID || w.ID == "" || strings.ContainsAny(w.ID, `/\.`) {
		return fmt.Errorf("Workspace %s: Pfad %s liegt nicht im Workspace-Verzeichnis %s", w.ID, w.Path, m.dir)
	}
	return nil
}

// removeAll löscht ein Verzeichnis; schreibgeschützte Inhalte (z. B. Go-Modul-Cache)
// werden vorher beschreibbar gemacht.
func removeAll(path string) error {
	if err := os.RemoveAll(path); err == nil {
		return nil
	}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700) //nolint:gosec // G302: Verzeichnisse brauchen das x-Bit
		}
		return nil
	})
	return os.RemoveAll(path)
}

// tailBuffer behält die letzten MaxSetupLog Bytes.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	cut bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - MaxSetupLog; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
		t.cut = true
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cut {
		return "… (gekürzt)\n" + string(t.buf)
	}
	return string(t.buf)
}
