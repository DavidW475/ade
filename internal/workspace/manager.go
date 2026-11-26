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
	return &Manager{store: st, projects: projects, dir: dir, log: log, ctx: ctx, cancel: cancel, setup: map[string]*setupRun{}}
}

// Recover markiert Workspaces, deren Anlegen oder Setup beim letzten Lauf unterbrochen wurde.
func (m *Manager) Recover(ctx context.Context) error {
	n, err := m.store.MarkInterruptedWorkspaces(ctx)
	if n > 0 {
		m.log.Warn("unterbrochene Workspaces markiert", "anzahl", n)
	}
	return err
}

// Close bricht laufende Setups ab und wartet auf sie.
func (m *Manager) Close() {
	m.cancel()
	m.wg.Wait()
}

// CreateOptions sind Optionen für einen neuen Workspace.
type CreateOptions struct {
	// BaseBranch überschreibt den Basis-Branch des Projekts.
	BaseBranch string `json:"base_branch,omitempty"`
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
	if len(p.Config.Setup) > 0 {
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
func clone(ctx context.Context, repo string, w *store.Workspace) error {
	steps := []git.Cmd{
		{Args: []string{"clone", "--local", "--quiet", "--branch", w.BaseBranch, "--", repo, w.Path}},
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

		log, err := runSetup(ctx, w, cmds)
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

func runSetup(ctx context.Context, w *store.Workspace, cmds []string) (string, error) {
	var out tailBuffer
	for i, c := range cmds {
		_, _ = fmt.Fprintf(&out, "$ %s\n", c)
		cctx, cancel := context.WithTimeout(ctx, SetupTimeout)
		cmd := exec.CommandContext(cctx, "sh", "-c", c)
		cmd.Dir = w.Path
		cmd.Env = append(os.Environ(), "ADE_WORKSPACE_ID="+w.ID, "ADE_WORKSPACE_PATH="+w.Path, "CI=1")
		cmd.Stdout, cmd.Stderr = &out, &out
		// Eigene Prozessgruppe: Beim Abbruch werden auch Kindprozesse beendet.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = 5 * time.Second
		err := cmd.Run()
		timedOut := errors.Is(cctx.Err(), context.DeadlineExceeded)
		cancel()
		switch {
		case ctx.Err() != nil:
			return out.String(), fmt.Errorf("Setup abgebrochen bei Befehl %d (%s)", i+1, c)
		case timedOut:
			return out.String(), fmt.Errorf("Setup-Befehl %d (%s) nach %s abgebrochen", i+1, c, SetupTimeout)
		case err != nil:
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
	d, err := git.WorktreeDiff(ctx, w.Path, w.BaseCommit, false)
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
	return git.WorktreeDiff(ctx, w.Path, w.BaseCommit, withPatch)
}

// Delete bricht ein laufendes Setup ab, löscht das Verzeichnis und den Datensatz.
// Das Haupt-Repo bleibt unverändert.
func (m *Manager) Delete(ctx context.Context, id string) error {
	w, err := m.store.GetWorkspace(ctx, id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	run := m.setup[id]
	m.mu.Unlock()
	if run != nil {
		run.cancel()
		<-run.done
	}

	if err := m.checkPath(w); err != nil {
		return err
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
