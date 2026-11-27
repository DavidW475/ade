// Package store kapselt die SQLite-Datenbank des Daemons inklusive Migrationen.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // SQLite-Treiber ohne cgo
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store ist der Zugriff auf die ADE-Datenbank.
type Store struct {
	db *sql.DB
}

// Open öffnet (oder erzeugt) die Datenbank unter path und spielt ausstehende Migrationen ein.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("Datenverzeichnis anlegen: %w", err)
	}
	if err := privateFiles(path); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("Datenbank öffnen: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// privateFiles legt die Datenbankdatei nur für den Nutzer lesbar an und setzt die Rechte
// bestehender Dateien (auch -wal und -shm) auf 0600. Die Datenbank enthält Aufgaben,
// Agent-Ausgaben und Egress-Protokolle; SQLite übernimmt für -wal und -shm die Rechte der
// Hauptdatei.
func privateFiles(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: Pfad aus der eigenen Konfiguration
	if err != nil {
		return fmt.Errorf("Datenbank anlegen: %w", err)
	}
	_ = f.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("Rechte der Datenbank setzen: %w", err)
		}
	}
	return nil
}

// DB liefert die zugrunde liegende Datenbankverbindung.
func (s *Store) DB() *sql.DB { return s.db }

// Close schließt die Datenbank.
func (s *Store) Close() error { return s.db.Close() }

// Ping prüft, ob die Datenbank erreichbar ist.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var ms []migration
	seen := map[int]string{}
	for _, e := range entries {
		name := e.Name()
		prefix, _, ok := strings.Cut(name, "_")
		v, err := strconv.Atoi(prefix)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("Migration %q: Name muss mit <nummer>_ beginnen", name)
		}
		if other, dup := seen[v]; dup {
			return nil, fmt.Errorf("Migrationen %q und %q haben dieselbe Nummer", other, name)
		}
		seen[v] = name
		data, err := fs.ReadFile(migrationFS, "migrations/"+name)
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: v, name: name, sql: string(data)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	return ms, nil
}

func (s *Store) migrate(ctx context.Context) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
	)`); err != nil {
		return fmt.Errorf("schema_migrations anlegen: %w", err)
	}

	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("Schema-Version lesen: %w", err)
	}
	if n := len(ms); n > 0 && current > ms[n-1].version {
		return fmt.Errorf("Datenbank hat Schema-Version %d, dieses Binary kennt nur bis %d – bitte ADE aktualisieren", current, ms[n-1].version)
	}

	for _, m := range ms {
		if m.version <= current {
			continue
		}
		if err := s.apply(ctx, m); err != nil {
			return fmt.Errorf("Migration %s: %w", m.name, err)
		}
	}
	return nil
}

func (s *Store) apply(ctx context.Context, m migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name) VALUES (?, ?)`, m.version, m.name); err != nil {
		return err
	}
	return tx.Commit()
}

// SchemaVersion liefert die höchste eingespielte Migration.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}
