package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DavidW475/ade/internal/apperr"
)

// Kinds of approvals.
const (
	ApprovalTool   = "tool"   // an agent's tool call (PreToolUse hook)
	ApprovalEgress = "egress" // blocked request to the egress proxy
	ApprovalBudget = "budget" // budget exceeded, session paused (M8)
	ApprovalGate   = "gate"   // gate of a factory run (gate: human, M9)
)

// Status of an approval.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalDenied   = "denied"
	ApprovalExpired  = "expired"
	ApprovalCanceled = "canceled"
)

// Decisions.
const (
	DecisionAllow          = "allow"           // allow this request
	DecisionAllowWorkspace = "allow_workspace" // allow the target for this workspace (egress)
	DecisionAllowProject   = "allow_project"   // allow the target for the project (egress)
	DecisionDeny           = "deny"
)

// Approval is an approval request.
type Approval struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	ProjectID   string          `json:"project_id"`
	WorkspaceID string          `json:"workspace_id"`
	SessionID   string          `json:"session_id,omitempty"`
	Title       string          `json:"title"`
	Detail      json.RawMessage `json:"detail"`
	Sandbox     bool            `json:"sandbox"`
	Status      string          `json:"status"`
	Decision    string          `json:"decision,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	ExpiresAt   time.Time       `json:"expires_at"`
	DecidedAt   *time.Time      `json:"decided_at,omitempty"`
}

const approvalCols = `id, kind, project_id, workspace_id, session_id, title, detail, sandbox, status, decision, reason,
	created_at, expires_at, decided_at`

func scanApproval(row interface{ Scan(...any) error }) (*Approval, error) {
	var a Approval
	var detail, created, expires string
	var decided sql.NullString
	if err := row.Scan(&a.ID, &a.Kind, &a.ProjectID, &a.WorkspaceID, &a.SessionID, &a.Title, &detail, &a.Sandbox,
		&a.Status, &a.Decision, &a.Reason, &created, &expires, &decided); err != nil {
		return nil, err
	}
	a.Detail = json.RawMessage(detail)
	a.CreatedAt, a.ExpiresAt, a.DecidedAt = parseTime(created), parseTime(expires), nullTime(decided)
	return &a, nil
}

// CreateApproval stores a new, pending approval.
func (s *Store) CreateApproval(ctx context.Context, a *Approval) error {
	detail := string(a.Detail)
	if detail == "" {
		detail = "{}"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO approvals
		(id, kind, project_id, workspace_id, session_id, title, detail, sandbox, status, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Kind, a.ProjectID, a.WorkspaceID, a.SessionID, a.Title, detail, a.Sandbox, ApprovalPending,
		a.ExpiresAt.UTC().Format(timeLayout))
	if isForeignKeyViolation(err) {
		return apperr.NotFound("workspace %s not found", a.WorkspaceID)
	}
	if err != nil {
		return fmt.Errorf("store approval: %w", err)
	}
	got, err := s.GetApproval(ctx, a.ID)
	if err != nil {
		return err
	}
	*a = *got
	return nil
}

// GetApproval returns an approval.
func (s *Store) GetApproval(ctx context.Context, id string) (*Approval, error) {
	a, err := scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalCols+` FROM approvals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.NotFound("approval %s not found", id)
	}
	return a, err
}

// ApprovalFilter restricts ListApprovals.
type ApprovalFilter struct {
	Status      string
	SessionID   string
	WorkspaceID string
	// Limit (default 100, at most 500).
	Limit int
}

// ListApprovals returns approvals, newest first.
func (s *Store) ListApprovals(ctx context.Context, f ApprovalFilter) ([]*Approval, error) {
	var where []string
	var args []any
	if f.Status != "" {
		where, args = append(where, "status = ?"), append(args, f.Status)
	}
	if f.SessionID != "" {
		where, args = append(where, "session_id = ?"), append(args, f.SessionID)
	}
	if f.WorkspaceID != "" {
		where, args = append(where, "workspace_id = ?"), append(args, f.WorkspaceID)
	}
	if f.Limit <= 0 {
		f.Limit = 100
	}
	q := `SELECT ` + approvalCols + ` FROM approvals`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ") //nolint:gosec // G202: only fixed conditions, values as parameters
	}
	q += " ORDER BY created_at DESC, id LIMIT ?"
	args = append(args, min(f.Limit, 500))
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Approval{}
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ResolveApproval sets status, decision and reason of a pending approval. If it is no
// longer pending, it returns apperr.Conflict.
func (s *Store) ResolveApproval(ctx context.Context, id, status, decision, reason string) (*Approval, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET status = ?, decision = ?, reason = ?,
		decided_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ? AND status = ?`,
		status, decision, reason, id, ApprovalPending)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		a, err := s.GetApproval(ctx, id)
		if err != nil {
			return nil, err
		}
		return a, apperr.Conflict("approval %s has already been decided (%s)", id, a.Status)
	}
	return s.GetApproval(ctx, id)
}

// ExpirePendingApprovals closes all pending approvals (after a daemon restart nobody waits
// for them any more). Returns the count.
func (s *Store) ExpirePendingApprovals(ctx context.Context, reason string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET status = ?, reason = ?,
		decided_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE status = ?`, ApprovalExpired, reason, ApprovalPending)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// EgressGrant is an egress exception from an approval (extends .ade/sandbox.yaml).
type EgressGrant struct {
	ID        int64  `json:"id"`
	ProjectID string `json:"project_id"`
	// WorkspaceID is empty if the exception applies to all workspaces of the project.
	WorkspaceID string    `json:"workspace_id,omitempty"`
	Rule        string    `json:"rule"`
	ApprovalID  string    `json:"approval_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// AddEgressGrant stores an exception; an existing equal one stays unchanged.
func (s *Store) AddEgressGrant(ctx context.Context, g *EgressGrant) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO egress_grants (project_id, workspace_id, rule, approval_id)
		VALUES (?, ?, ?, ?) ON CONFLICT (project_id, workspace_id, rule) DO NOTHING`,
		g.ProjectID, g.WorkspaceID, g.Rule, g.ApprovalID)
	if isForeignKeyViolation(err) {
		return apperr.NotFound("project %s not found", g.ProjectID)
	}
	if err != nil {
		return fmt.Errorf("store egress exception: %w", err)
	}
	row := s.db.QueryRowContext(ctx, `SELECT id, project_id, workspace_id, rule, approval_id, created_at FROM egress_grants
		WHERE project_id = ? AND workspace_id = ? AND rule = ?`, g.ProjectID, g.WorkspaceID, g.Rule)
	var created string
	if err := row.Scan(&g.ID, &g.ProjectID, &g.WorkspaceID, &g.Rule, &g.ApprovalID, &created); err != nil {
		return err
	}
	g.CreatedAt = parseTime(created)
	return nil
}

// ListEgressGrants returns the exceptions that apply to a workspace of a project
// (project-wide ones and the workspace's). Without workspaceID only the project-wide ones.
func (s *Store) ListEgressGrants(ctx context.Context, projectID, workspaceID string) ([]EgressGrant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, project_id, workspace_id, rule, approval_id, created_at FROM egress_grants
		WHERE project_id = ? AND (workspace_id = '' OR workspace_id = ?) ORDER BY id`, projectID, workspaceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []EgressGrant{}
	for rows.Next() {
		var g EgressGrant
		var created string
		if err := rows.Scan(&g.ID, &g.ProjectID, &g.WorkspaceID, &g.Rule, &g.ApprovalID, &created); err != nil {
			return nil, err
		}
		g.CreatedAt = parseTime(created)
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetEgressGrant returns an exception.
func (s *Store) GetEgressGrant(ctx context.Context, id int64) (*EgressGrant, error) {
	var g EgressGrant
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT id, project_id, workspace_id, rule, approval_id, created_at FROM egress_grants WHERE id = ?`, id).
		Scan(&g.ID, &g.ProjectID, &g.WorkspaceID, &g.Rule, &g.ApprovalID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.NotFound("egress exception %d not found", id)
	}
	if err != nil {
		return nil, err
	}
	g.CreatedAt = parseTime(created)
	return &g, nil
}

// DeleteEgressGrant removes an exception.
func (s *Store) DeleteEgressGrant(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM egress_grants WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.NotFound("egress exception %d not found", id)
	}
	return nil
}
