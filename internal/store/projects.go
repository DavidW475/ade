package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DavidW475/ade/internal/apperr"
)

// Project is a registered Git repository.
type Project struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	RepoPath      string    `json:"repo_path"`
	DefaultBranch string    `json:"default_branch"`
	CreatedAt     time.Time `json:"created_at"`
}

const projectCols = `id, name, repo_path, default_branch, created_at`

func scanProject(row interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	var created string
	if err := row.Scan(&p.ID, &p.Name, &p.RepoPath, &p.DefaultBranch, &created); err != nil {
		return nil, err
	}
	p.CreatedAt = parseTime(created)
	return &p, nil
}

// CreateProject creates a project. An already registered path is a conflict.
func (s *Store) CreateProject(ctx context.Context, p *Project) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (id, name, repo_path, default_branch) VALUES (?, ?, ?, ?)`,
		p.ID, p.Name, p.RepoPath, p.DefaultBranch)
	if isUniqueViolation(err) {
		return apperr.Conflict("repository %s is already registered", p.RepoPath)
	}
	if err != nil {
		return fmt.Errorf("store project: %w", err)
	}
	got, err := s.GetProject(ctx, p.ID)
	if err != nil {
		return err
	}
	*p = *got
	return nil
}

// GetProject returns a project by its ID.
func (s *Store) GetProject(ctx context.Context, id string) (*Project, error) {
	p, err := scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.NotFound("project %s not found", id)
	}
	return p, err
}

// FindProject looks up a project by ID, name or repository path.
func (s *Store) FindProject(ctx context.Context, ref string) (*Project, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+projectCols+` FROM projects WHERE id = ? OR name = ? OR repo_path = ? ORDER BY id = ? DESC`,
		ref, ref, ref, ref)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var found []*Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		found = append(found, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch {
	case len(found) == 0:
		return nil, apperr.NotFound("project %q not found", ref)
	case len(found) > 1 && found[0].ID != ref:
		ids := make([]string, len(found))
		for i, p := range found {
			ids[i] = p.ID
		}
		return nil, apperr.Conflict("%q is ambiguous, please give the ID: %s", ref, strings.Join(ids, ", "))
	}
	return found[0], nil
}

// ListProjects returns all projects, sorted by name.
func (s *Store) ListProjects(ctx context.Context) ([]*Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectCols+` FROM projects ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteProject deletes a project. If workspaces still exist, that is a conflict.
func (s *Store) DeleteProject(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	if isForeignKeyViolation(err) {
		return apperr.Conflict("project %s still has workspaces", id)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.NotFound("project %s not found", id)
	}
	return nil
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func isForeignKeyViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}
