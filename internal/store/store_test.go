package store

import (
	"context"
	"path/filepath"
	"testing"
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
		t.Fatalf("projects-Tabelle fehlt: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Zweites Öffnen darf nichts erneut einspielen und keine Daten verlieren.
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	v2, _ := s.SchemaVersion(ctx)
	if v2 != v {
		t.Fatalf("SchemaVersion nach erneutem Öffnen = %d, want %d", v2, v)
	}
	var n int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM projects`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("projects = %d, %v", n, err)
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ade.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO schema_migrations (version, name) VALUES (9999, 'zukunft')`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	if s, err := Open(ctx, path); err == nil {
		_ = s.Close()
		t.Fatal("Fehler bei neuerem Schema erwartet")
	}
}

func TestMigrationsAreWellFormed(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range ms {
		if m.version != i+1 {
			t.Errorf("Migration %s: Nummer %d, erwartet lückenlos %d", m.name, m.version, i+1)
		}
	}
}
