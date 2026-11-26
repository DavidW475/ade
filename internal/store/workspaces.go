package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/DavidW475/ade/internal/apperr"
)

// Workspace-Status.
const (
	WorkspaceCreating    = "creating"
	WorkspaceSetup       = "setup"
	WorkspaceReady       = "ready"
	WorkspaceSetupFailed = "setup_failed"
	WorkspaceError       = "error"
)

// Workspace ist eine isolierte Arbeitskopie eines Projekts.
type Workspace struct {
	ID         string    `json:"id"`
	ProjectID  string    `json:"project_id"`
	Path       string    `json:"path"`
	Branch     string    `json:"branch"`
	BaseBranch string    `json:"base_branch"`
	BaseCommit string    `json:"base_commit"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	SetupLog   string    `json:"setup_log,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

const workspaceCols = `id, project_id, path, branch, base_branch, base_commit, status, error, setup_log, created_at, updated_at`

func scanWorkspace(row interface{ Scan(...any) error }) (*Workspace, error) {
	var w Workspace
	var created, updated string
	if err := row.Scan(&w.ID, &w.ProjectID, &w.Path, &w.Branch, &w.BaseBranch, &w.BaseCommit,
		&w.Status, &w.Error, &w.SetupLog, &created, &updated); err != nil {
		return nil, err
	}
	w.CreatedAt, w.UpdatedAt = parseTime(created), parseTime(updated)
	return &w, nil
}

// CreateWorkspace legt einen Workspace-Datensatz an.
func (s *Store) CreateWorkspace(ctx context.Context, w *Workspace) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workspaces (id, project_id, path, branch, base_branch, base_commit, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.ProjectID, w.Path, w.Branch, w.BaseBranch, w.BaseCommit, w.Status)
	if isForeignKeyViolation(err) {
		return apperr.NotFound("Projekt %s nicht gefunden", w.ProjectID)
	}
	if err != nil {
		return fmt.Errorf("Workspace speichern: %w", err)
	}
	got, err := s.GetWorkspace(ctx, w.ID)
	if err != nil {
		return err
	}
	*w = *got
	return nil
}

// GetWorkspace liefert einen Workspace.
func (s *Store) GetWorkspace(ctx context.Context, id string) (*Workspace, error) {
	w, err := scanWorkspace(s.db.QueryRowContext(ctx, `SELECT `+workspaceCols+` FROM workspaces WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.NotFound("Workspace %s nicht gefunden", id)
	}
	return w, err
}

// ListWorkspaces liefert die Workspaces eines Projekts (projectID leer: alle), neueste zuerst.
func (s *Store) ListWorkspaces(ctx context.Context, projectID string) ([]*Workspace, error) {
	q := `SELECT ` + workspaceCols + ` FROM workspaces`
	var args []any
	if projectID != "" {
		q += ` WHERE project_id = ?`
		args = append(args, projectID)
	}
	q += ` ORDER BY created_at DESC, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Workspace{}
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// WorkspaceUpdate enthält die änderbaren Felder; nil bedeutet unverändert.
type WorkspaceUpdate struct {
	BaseCommit *string
	Status     *string
	Error      *string
	SetupLog   *string
}

// UpdateWorkspace ändert die gesetzten Felder.
func (s *Store) UpdateWorkspace(ctx context.Context, id string, u WorkspaceUpdate) error {
	res, err := s.db.ExecContext(ctx, `UPDATE workspaces SET
		base_commit = COALESCE(?, base_commit),
		status      = COALESCE(?, status),
		error       = COALESCE(?, error),
		setup_log   = COALESCE(?, setup_log),
		updated_at  = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
		WHERE id = ?`, u.BaseCommit, u.Status, u.Error, u.SetupLog, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.NotFound("Workspace %s nicht gefunden", id)
	}
	return nil
}

// DeleteWorkspace löscht einen Workspace-Datensatz.
func (s *Store) DeleteWorkspace(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.NotFound("Workspace %s nicht gefunden", id)
	}
	return nil
}

// MarkInterruptedWorkspaces setzt Workspaces, deren Anlegen oder Setup durch einen
// Daemon-Neustart unterbrochen wurde, auf einen Fehlerstatus. Liefert die Anzahl.
func (s *Store) MarkInterruptedWorkspaces(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE workspaces SET
		status = CASE status WHEN 'setup' THEN 'setup_failed' ELSE 'error' END,
		error = 'durch Neustart des Daemons unterbrochen',
		updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
		WHERE status IN ('creating', 'setup')`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Ptr liefert einen Zeiger auf v (für WorkspaceUpdate).
func Ptr[T any](v T) *T { return &v }
