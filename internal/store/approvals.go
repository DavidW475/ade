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

// Arten von Freigaben.
const (
	ApprovalTool   = "tool"   // Werkzeug-Aufruf eines Agenten (PreToolUse-Hook)
	ApprovalEgress = "egress" // geblockte Anfrage an den Egress-Proxy
	ApprovalBudget = "budget" // Budget überschritten, Session pausiert (M8)
)

// Status einer Freigabe.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalDenied   = "denied"
	ApprovalExpired  = "expired"
	ApprovalCanceled = "canceled"
)

// Entscheidungen.
const (
	DecisionAllow          = "allow"           // diese Anfrage erlauben
	DecisionAllowWorkspace = "allow_workspace" // Ziel für diesen Workspace erlauben (egress)
	DecisionAllowProject   = "allow_project"   // Ziel für das Projekt erlauben (egress)
	DecisionDeny           = "deny"
)

// Approval ist eine Freigabe-Anfrage.
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

// CreateApproval speichert eine neue, offene Freigabe.
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
		return apperr.NotFound("Workspace %s nicht gefunden", a.WorkspaceID)
	}
	if err != nil {
		return fmt.Errorf("Freigabe speichern: %w", err)
	}
	got, err := s.GetApproval(ctx, a.ID)
	if err != nil {
		return err
	}
	*a = *got
	return nil
}

// GetApproval liefert eine Freigabe.
func (s *Store) GetApproval(ctx context.Context, id string) (*Approval, error) {
	a, err := scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalCols+` FROM approvals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.NotFound("Freigabe %s nicht gefunden", id)
	}
	return a, err
}

// ApprovalFilter schränkt ListApprovals ein.
type ApprovalFilter struct {
	Status      string
	SessionID   string
	WorkspaceID string
	// Limit (Standard 100, höchstens 500).
	Limit int
}

// ListApprovals liefert Freigaben, neueste zuerst.
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
		q += " WHERE " + strings.Join(where, " AND ") //nolint:gosec // G202: nur feste Bedingungen, Werte als Parameter
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

// ResolveApproval setzt Status, Entscheidung und Begründung einer offenen Freigabe. Ist sie
// nicht mehr offen, liefert es apperr.Conflict.
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
		return a, apperr.Conflict("Freigabe %s ist bereits entschieden (%s)", id, a.Status)
	}
	return s.GetApproval(ctx, id)
}

// ExpirePendingApprovals schließt alle offenen Freigaben (nach einem Daemon-Neustart wartet
// niemand mehr auf sie). Liefert die Anzahl.
func (s *Store) ExpirePendingApprovals(ctx context.Context, reason string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET status = ?, reason = ?,
		decided_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE status = ?`, ApprovalExpired, reason, ApprovalPending)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// EgressGrant ist eine Egress-Ausnahme aus einer Freigabe (ergänzt .ade/sandbox.yaml).
type EgressGrant struct {
	ID        int64  `json:"id"`
	ProjectID string `json:"project_id"`
	// WorkspaceID ist leer, wenn die Ausnahme für alle Workspaces des Projekts gilt.
	WorkspaceID string    `json:"workspace_id,omitempty"`
	Rule        string    `json:"rule"`
	ApprovalID  string    `json:"approval_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// AddEgressGrant speichert eine Ausnahme; eine vorhandene gleiche bleibt unverändert.
func (s *Store) AddEgressGrant(ctx context.Context, g *EgressGrant) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO egress_grants (project_id, workspace_id, rule, approval_id)
		VALUES (?, ?, ?, ?) ON CONFLICT (project_id, workspace_id, rule) DO NOTHING`,
		g.ProjectID, g.WorkspaceID, g.Rule, g.ApprovalID)
	if isForeignKeyViolation(err) {
		return apperr.NotFound("Projekt %s nicht gefunden", g.ProjectID)
	}
	if err != nil {
		return fmt.Errorf("Egress-Ausnahme speichern: %w", err)
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

// ListEgressGrants liefert die Ausnahmen, die für einen Workspace eines Projekts gelten
// (projektweite und die des Workspaces). Ohne workspaceID nur die projektweiten.
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

// GetEgressGrant liefert eine Ausnahme.
func (s *Store) GetEgressGrant(ctx context.Context, id int64) (*EgressGrant, error) {
	var g EgressGrant
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT id, project_id, workspace_id, rule, approval_id, created_at FROM egress_grants WHERE id = ?`, id).
		Scan(&g.ID, &g.ProjectID, &g.WorkspaceID, &g.Rule, &g.ApprovalID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.NotFound("Egress-Ausnahme %d nicht gefunden", id)
	}
	if err != nil {
		return nil, err
	}
	g.CreatedAt = parseTime(created)
	return &g, nil
}

// DeleteEgressGrant entfernt eine Ausnahme.
func (s *Store) DeleteEgressGrant(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM egress_grants WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.NotFound("Egress-Ausnahme %d nicht gefunden", id)
	}
	return nil
}
