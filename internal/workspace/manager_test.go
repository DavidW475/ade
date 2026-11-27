package workspace

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/store"
)

type env struct {
	t        *testing.T
	repo     string
	dir      string
	store    *store.Store
	projects *project.Service
	m        *Manager
	project  *project.View
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T, projectYAML string) *env {
	t.Helper()
	ctx := context.Background()
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(repo, "README.md"), "hallo\n")
	if projectYAML != "" {
		writeFile(t, filepath.Join(repo, project.ConfigFile), projectYAML)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "ade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ps := project.NewService(st)
	p, err := ps.Add(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "workspaces")
	m := NewManager(st, ps, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(m.Close)
	return &env{t: t, repo: repo, dir: dir, store: st, projects: ps, m: m, project: p}
}

// repoState fasst zusammen, was sich im Haupt-Repo nicht ändern darf.
func repoState(t *testing.T, repo string) string {
	return gitRun(t, repo, "for-each-ref") + "\n" + gitRun(t, repo, "status", "--porcelain") +
		"\n" + gitRun(t, repo, "config", "--local", "--list") + "\n" + gitRun(t, repo, "worktree", "list")
}

func waitStatus(t *testing.T, m *Manager, id string, want ...string) *store.Workspace {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		w, err := m.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range want {
			if w.Status == s {
				return w
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Status = %s, want %v", w.Status, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCreateDiffDelete(t *testing.T) {
	e := setup(t, "")
	ctx := context.Background()
	before := repoState(t, e.repo)

	w, err := e.m.Create(ctx, e.project.ID, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != store.WorkspaceReady || w.Branch != "ade/"+w.ID || w.BaseBranch != "main" {
		t.Fatalf("Workspace = %#v", w)
	}
	if head := gitRun(t, e.repo, "rev-parse", "main"); w.BaseCommit != head {
		t.Errorf("BaseCommit = %s, want %s", w.BaseCommit, head)
	}
	if b := gitRun(t, w.Path, "branch", "--show-current"); b != w.Branch {
		t.Errorf("Branch im Workspace = %q", b)
	}
	if remotes := gitRun(t, w.Path, "remote"); remotes != "" {
		t.Errorf("Workspace hat noch Remotes: %q", remotes)
	}

	// Manuelle Änderungen erscheinen im Diff.
	writeFile(t, filepath.Join(w.Path, "README.md"), "hallo welt\n")
	writeFile(t, filepath.Join(w.Path, "neu.txt"), "neu\n")
	d, err := e.m.Diff(ctx, w.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != 2 || d.Base != w.BaseCommit {
		t.Fatalf("Diff = %#v", d)
	}
	// Committete Änderungen ebenfalls.
	gitRun(t, w.Path, "add", ".")
	gitRun(t, w.Path, "commit", "-q", "-m", "arbeit")
	writeFile(t, filepath.Join(w.Path, "dritte.txt"), "x\n")
	list, err := e.m.List(ctx, e.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Changes == nil || list[0].Changes.Files != 3 {
		t.Fatalf("List = %#v", list[0])
	}

	if after := repoState(t, e.repo); after != before {
		t.Errorf("Haupt-Repo verändert:\nvorher:\n%s\nnachher:\n%s", before, after)
	}

	if err := e.m.Delete(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
		t.Errorf("Workspace-Verzeichnis existiert noch: %v", err)
	}
	entries, _ := os.ReadDir(e.dir)
	if len(entries) != 0 {
		t.Errorf("Workspace-Verzeichnis nicht leer: %v", entries)
	}
	if _, err := e.m.Get(ctx, w.ID); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("Datensatz existiert noch: %v", err)
	}
	if after := repoState(t, e.repo); after != before {
		t.Errorf("Haupt-Repo nach Löschen verändert")
	}
}

func TestCreateRejectsUnknownBranch(t *testing.T) {
	e := setup(t, "")
	_, err := e.m.Create(context.Background(), e.project.ID, CreateOptions{BaseBranch: "gibtsnicht"})
	if !errors.Is(err, apperr.ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	if ws, _ := e.store.ListWorkspaces(context.Background(), ""); len(ws) != 0 {
		t.Errorf("Datensatz angelegt: %v", ws)
	}
	if _, err := e.m.Create(context.Background(), "fehlt", CreateOptions{}); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("unbekanntes Projekt: err = %v", err)
	}
}

func TestSetupCommands(t *testing.T) {
	e := setup(t, "setup:\n  - echo vorbereitet > setup.txt\n  - echo \"id=$ADE_WORKSPACE_ID\"\n")
	ctx := context.Background()

	w, err := e.m.Create(ctx, e.project.ID, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w = waitStatus(t, e.m, w.ID, store.WorkspaceReady, store.WorkspaceSetupFailed)
	if w.Status != store.WorkspaceReady {
		t.Fatalf("Status = %s, Fehler = %s, Log:\n%s", w.Status, w.Error, w.SetupLog)
	}
	if !strings.Contains(w.SetupLog, "$ echo vorbereitet") || !strings.Contains(w.SetupLog, "id="+w.ID) {
		t.Errorf("SetupLog = %q", w.SetupLog)
	}
	if _, err := os.Stat(filepath.Join(w.Path, "setup.txt")); err != nil {
		t.Errorf("Setup-Befehl lief nicht im Workspace: %v", err)
	}
}

func TestSetupFailure(t *testing.T) {
	e := setup(t, "setup:\n  - echo erst\n  - exit 3\n  - echo nie\n")
	w, err := e.m.Create(context.Background(), e.project.ID, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w = waitStatus(t, e.m, w.ID, store.WorkspaceReady, store.WorkspaceSetupFailed)
	if w.Status != store.WorkspaceSetupFailed || !strings.Contains(w.Error, "Setup-Befehl 2") || strings.Contains(w.SetupLog, "$ echo nie") {
		t.Fatalf("Workspace = %#v", w)
	}
}

func TestSetupCommandNotFound(t *testing.T) {
	e := setup(t, "setup:\n  - ade-gibt-es-nicht --version\n")
	w, err := e.m.Create(context.Background(), e.project.ID, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w = waitStatus(t, e.m, w.ID, store.WorkspaceReady, store.WorkspaceSetupFailed)
	if w.Status != store.WorkspaceSetupFailed || !strings.Contains(w.Error, "exit status 127") || !strings.Contains(w.Error, "Programm nicht gefunden") {
		t.Fatalf("Workspace = %#v", w)
	}
}

func TestDeleteCancelsSetup(t *testing.T) {
	e := setup(t, "setup:\n  - sleep 30\n")
	ctx := context.Background()
	w, err := e.m.Create(ctx, e.project.ID, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := e.m.Delete(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("Löschen hat auf das Setup gewartet")
	}
	if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
		t.Errorf("Verzeichnis existiert noch")
	}
}

func TestCreateRejectsInvalidProjectConfig(t *testing.T) {
	e := setup(t, "")
	writeFile(t, filepath.Join(e.repo, project.ConfigFile), "adapter: falsch\n")
	if _, err := e.m.Create(context.Background(), e.project.ID, CreateOptions{}); !errors.Is(err, apperr.ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckPath(t *testing.T) {
	m := &Manager{dir: "/data/ws"}
	ok := &store.Workspace{ID: "abc", Path: "/data/ws/abc"}
	if err := m.checkPath(ok); err != nil {
		t.Errorf("gültiger Pfad abgelehnt: %v", err)
	}
	for _, w := range []*store.Workspace{
		{ID: "abc", Path: "/data/ws"},
		{ID: "abc", Path: "/home/user"},
		{ID: "abc", Path: "/data/ws/abc/.."},
		{ID: "", Path: "/data/ws/"},
		{ID: "..", Path: "/data"},
	} {
		if err := m.checkPath(w); err == nil {
			t.Errorf("%#v: Fehler erwartet", w)
		}
	}
}

func TestRecover(t *testing.T) {
	e := setup(t, "")
	ctx := context.Background()
	w := &store.Workspace{ID: "x1", ProjectID: e.project.ID, Path: filepath.Join(e.dir, "x1"), Branch: "ade/x1", BaseBranch: "main", Status: store.WorkspaceCreating}
	if err := e.store.CreateWorkspace(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := e.m.Get(ctx, "x1")
	if got.Status != store.WorkspaceError {
		t.Errorf("Status = %s", got.Status)
	}
	// Ein Workspace ohne Verzeichnis lässt sich trotzdem löschen.
	if err := e.m.Delete(ctx, "x1"); err != nil {
		t.Fatal(err)
	}
}
