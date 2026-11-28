package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/DavidW475/ade/internal/apperr"
)

// Workspace status.
const (
	WorkspaceCreating    = "creating"
	WorkspaceSetup       = "setup"
	WorkspaceReady       = "ready"
	WorkspaceSetupFailed = "setup_failed"
	WorkspaceError       = "error"
)

// Workspace is an isolated working copy of a project.
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

	// HeadCommit is the state of the branch most recently fetched into the main repository.
	HeadCommit  string     `json:"head_commit,omitempty"`
	CollectedAt *time.Time `json:"collected_at,omitempty"`
	// MergeCommit is the merge commit in the base branch after “Merge”.
	MergeCommit string     `json:"merge_commit,omitempty"`
	MergedAt    *time.Time `json:"merged_at,omitempty"`
	// PRURL is the pull request after “Create PR”.
	PRURL string `json:"pr_url,omitempty"`

	// Sandbox: agent, setup and IDE run in the workspace's Docker container (M5).
	Sandbox bool `json:"sandbox"`
	// SandboxImage is the image the container is or was created with.
	SandboxImage string `json:"sandbox_image,omitempty"`
}

const workspaceCols = `id, project_id, path, branch, base_branch, base_commit, status, error, setup_log, created_at, updated_at,
	head_commit, collected_at, merge_commit, merged_at, pr_url, sandbox, sandbox_image`

func scanWorkspace(row interface{ Scan(...any) error }) (*Workspace, error) {
	var w Workspace
	var created, updated string
	var collected, merged sql.NullString
	if err := row.Scan(&w.ID, &w.ProjectID, &w.Path, &w.Branch, &w.BaseBranch, &w.BaseCommit,
		&w.Status, &w.Error, &w.SetupLog, &created, &updated,
		&w.HeadCommit, &collected, &w.MergeCommit, &merged, &w.PRURL, &w.Sandbox, &w.SandboxImage); err != nil {
		return nil, err
	}
	w.CreatedAt, w.UpdatedAt = parseTime(created), parseTime(updated)
	w.CollectedAt, w.MergedAt = nullTime(collected), nullTime(merged)
	return &w, nil
}

// CreateWorkspace creates a workspace record.
func (s *Store) CreateWorkspace(ctx context.Context, w *Workspace) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workspaces (id, project_id, path, branch, base_branch, base_commit, status, sandbox, sandbox_image)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.ProjectID, w.Path, w.Branch, w.BaseBranch, w.BaseCommit, w.Status, w.Sandbox, w.SandboxImage)
	if isForeignKeyViolation(err) {
		return apperr.NotFound("project %s not found", w.ProjectID)
	}
	if err != nil {
		return fmt.Errorf("store workspace: %w", err)
	}
	got, err := s.GetWorkspace(ctx, w.ID)
	if err != nil {
		return err
	}
	*w = *got
	return nil
}

// GetWorkspace returns a workspace.
func (s *Store) GetWorkspace(ctx context.Context, id string) (*Workspace, error) {
	w, err := scanWorkspace(s.db.QueryRowContext(ctx, `SELECT `+workspaceCols+` FROM workspaces WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.NotFound("workspace %s not found", id)
	}
	return w, err
}

// ListWorkspaces returns the workspaces of a project (projectID empty: all), newest first.
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

// WorkspaceUpdate contains the changeable fields; nil means unchanged.
type WorkspaceUpdate struct {
	BaseCommit *string
	Status     *string
	Error      *string
	SetupLog   *string
	// HeadCommit also sets collected_at to now.
	HeadCommit *string
	// MergeCommit also sets merged_at to now.
	MergeCommit *string
	PRURL       *string
}

// UpdateWorkspace changes the set fields.
func (s *Store) UpdateWorkspace(ctx context.Context, id string, u WorkspaceUpdate) error {
	res, err := s.db.ExecContext(ctx, `UPDATE workspaces SET
		base_commit  = COALESCE(?1, base_commit),
		status       = COALESCE(?2, status),
		error        = COALESCE(?3, error),
		setup_log    = COALESCE(?4, setup_log),
		head_commit  = COALESCE(?5, head_commit),
		collected_at = CASE WHEN ?5 IS NULL THEN collected_at ELSE strftime('%Y-%m-%dT%H:%M:%SZ', 'now') END,
		merge_commit = COALESCE(?6, merge_commit),
		merged_at    = CASE WHEN ?6 IS NULL THEN merged_at ELSE strftime('%Y-%m-%dT%H:%M:%SZ', 'now') END,
		pr_url       = COALESCE(?7, pr_url),
		updated_at   = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
		WHERE id = ?8`, u.BaseCommit, u.Status, u.Error, u.SetupLog, u.HeadCommit, u.MergeCommit, u.PRURL, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.NotFound("workspace %s not found", id)
	}
	// The workspace's egress exceptions are not attached to it by a foreign key.
	_, err = s.db.ExecContext(ctx, `DELETE FROM egress_grants WHERE workspace_id = ?`, id)
	return err
}

// DeleteWorkspace deletes a workspace record.
func (s *Store) DeleteWorkspace(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.NotFound("workspace %s not found", id)
	}
	return nil
}

// MarkInterruptedWorkspaces sets workspaces whose creation or setup was interrupted by a
// daemon restart to an error status. Returns the count.
func (s *Store) MarkInterruptedWorkspaces(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE workspaces SET
		status = CASE status WHEN 'setup' THEN 'setup_failed' ELSE 'error' END,
		error = 'interrupted by a daemon restart',
		updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
		WHERE status IN ('creating', 'setup')`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func nullTime(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t := parseTime(s.String)
	return &t
}

// Ptr returns a pointer to v (for WorkspaceUpdate).
func Ptr[T any](v T) *T { return &v }
