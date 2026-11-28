package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/DavidW475/ade/internal/apperr"
)

func TestOpenMigratesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sub", "ade.db")

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v < 1 {
		t.Fatalf("SchemaVersion = %d, want >= 1", v)
	}
	if _, err := s.DB().ExecContext(ctx,
		`INSERT INTO projects (id, name, repo_path, default_branch) VALUES ('p1', 'demo', '/repo', 'main')`); err != nil {
		t.Fatalf("projects table missing: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Opening a second time must not apply anything again or lose data.
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	v2, _ := s.SchemaVersion(ctx)
	if v2 != v {
		t.Fatalf("SchemaVersion after reopening = %d, want %d", v2, v)
	}
	var n int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM projects`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("projects = %d, %v", n, err)
	}
}

func TestOpenMakesDatabasePrivate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ade.db")
	// An older version created the file with the umask's permissions.
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO projects (id, name, repo_path, default_branch) VALUES ('p1', 'demo', '/repo', 'main')`); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if m := fi.Mode().Perm(); m != 0o600 {
			t.Errorf("%s: Rechte %o, want 600", filepath.Base(p), m)
		}
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ade.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO schema_migrations (version, name) VALUES (9999, 'future')`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	if s, err := Open(ctx, path); err == nil {
		_ = s.Close()
		t.Fatal("expected an error for a newer schema")
	}
}

func TestMigrationsAreWellFormed(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range ms {
		if m.version != i+1 {
			t.Errorf("migration %s: number %d, want gapless %d", m.name, m.version, i+1)
		}
	}
}

func TestProjectsAndWorkspaces(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "ade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	p := &Project{ID: "p1", Name: "demo", RepoPath: "/repo/demo", DefaultBranch: "main"}
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if p.CreatedAt.IsZero() {
		t.Error("CreatedAt not set")
	}
	if err := s.CreateProject(ctx, &Project{ID: "p2", Name: "x", RepoPath: "/repo/demo", DefaultBranch: "main"}); !errors.Is(err, apperr.ErrConflict) {
		t.Errorf("duplicate path: err = %v", err)
	}
	if err := s.CreateProject(ctx, &Project{ID: "p3", Name: "demo", RepoPath: "/repo/demo2", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}

	for ref, want := range map[string]string{"p1": "p1", "/repo/demo2": "p3"} {
		got, err := s.FindProject(ctx, ref)
		if err != nil || got.ID != want {
			t.Errorf("FindProject(%q) = %v, %v", ref, got, err)
		}
	}
	if _, err := s.FindProject(ctx, "demo"); !errors.Is(err, apperr.ErrConflict) {
		t.Errorf("ambiguous name: err = %v", err)
	}
	if _, err := s.FindProject(ctx, "does-not-exist"); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("unknown: err = %v", err)
	}

	w := &Workspace{ID: "w1", ProjectID: "p1", Path: "/ws/w1", Branch: "ade/w1", BaseBranch: "main", Status: WorkspaceCreating}
	if err := s.CreateWorkspace(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWorkspace(ctx, &Workspace{ID: "w2", ProjectID: "missing", Path: "/ws/w2", Branch: "b", BaseBranch: "main", Status: WorkspaceCreating}); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("workspace without project: err = %v", err)
	}
	if err := s.UpdateWorkspace(ctx, "w1", WorkspaceUpdate{Status: Ptr(WorkspaceSetup), BaseCommit: Ptr("abc")}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWorkspace(ctx, "w1")
	if err != nil || got.Status != WorkspaceSetup || got.BaseCommit != "abc" || got.Branch != "ade/w1" {
		t.Fatalf("GetWorkspace = %#v, %v", got, err)
	}

	if err := s.DeleteProject(ctx, "p1"); !errors.Is(err, apperr.ErrConflict) {
		t.Errorf("delete project with workspaces: err = %v", err)
	}

	if n, err := s.MarkInterruptedWorkspaces(ctx); err != nil || n != 1 {
		t.Fatalf("MarkInterrupted = %d, %v", n, err)
	}
	got, _ = s.GetWorkspace(ctx, "w1")
	if got.Status != WorkspaceSetupFailed || got.Error == "" {
		t.Errorf("after interruption: %#v", got)
	}

	list, err := s.ListWorkspaces(ctx, "p1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListWorkspaces = %v, %v", list, err)
	}
	if err := s.DeleteWorkspace(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, "p1"); !errors.Is(err, apperr.ErrNotFound) {
		t.Errorf("second delete: err = %v", err)
	}
}
