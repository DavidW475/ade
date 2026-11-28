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

// Session status.
const (
	// SessionStarting: created, waits e.g. for the workspace setup.
	SessionStarting = "starting"
	// SessionRunning: the agent is working.
	SessionRunning = "running"
	// SessionIdle: the process is running and waits for a follow-up instruction.
	SessionIdle = "idle"
	// SessionDone: the process ended regularly.
	SessionDone = "done"
	// SessionFailed: the process ended with an error or could not be started.
	SessionFailed = "failed"
	// SessionStopped: stopped by the user.
	SessionStopped = "stopped"
	// SessionInterrupted: interrupted by the daemon stopping or crashing.
	SessionInterrupted = "interrupted"
	// SessionPaused: budget exceeded; waits for an approval (M8).
	SessionPaused = "paused"
)

// ActiveSessionStates are states in which a process belongs to a session (or will shortly).
var ActiveSessionStates = []string{SessionStarting, SessionRunning, SessionIdle}

// SessionActive reports whether status is an active status.
func SessionActive(status string) bool {
	switch status {
	case SessionStarting, SessionRunning, SessionIdle:
		return true
	}
	return false
}

// Session is a run of an agent (or terminal) in a workspace.
type Session struct {
	ID          string   `json:"id"`
	WorkspaceID string   `json:"workspace_id"`
	ProjectID   string   `json:"project_id"`
	Adapter     string   `json:"adapter"`
	Task        string   `json:"task,omitempty"`
	Command     []string `json:"command,omitempty"`
	Model       string   `json:"model,omitempty"`
	// ModelID is the resolved model ID if Model is an alias.
	ModelID        string  `json:"model_id,omitempty"`
	PermissionMode string  `json:"permission_mode,omitempty"`
	Role           string  `json:"role,omitempty"`
	Status         string  `json:"status"`
	Error          string  `json:"error,omitempty"`
	ExitCode       *int    `json:"exit_code,omitempty"`
	ResumeID       string  `json:"resume_id,omitempty"`
	PID            int     `json:"-"`
	CostUSD        float64 `json:"cost_usd"`
	// Tokens are the sums of the usage (table usage).
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	// Unpriced: no price was known for some of the tokens (costs incomplete).
	Unpriced bool `json:"unpriced,omitempty"`
	// Metered: the egress proxy measures the usage (sandbox with secret injection).
	Metered bool `json:"metered,omitempty"`
	// BudgetUSD is the session's budget (0: none).
	BudgetUSD float64 `json:"budget_usd,omitempty"`
	// MonthlyAllowUSD: after an approval the session may continue up to these monthly costs
	// of the project.
	MonthlyAllowUSD float64 `json:"monthly_allow_usd,omitempty"`
	// ReadOnly: session of a read-only role of a factory (workspace read-only, M9).
	ReadOnly  bool       `json:"read_only,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

const sessionCols = `s.id, s.workspace_id, w.project_id, s.adapter, s.task, s.command, s.model, s.permission_mode,
	s.role, s.status, s.error, s.exit_code, s.resume_id, s.pid, s.cost_usd, s.created_at, s.updated_at, s.ended_at,
	s.model_id, s.input_tokens, s.output_tokens, s.cache_read_tokens, s.cache_write_tokens, s.unpriced, s.metered,
	s.budget_usd, s.monthly_allow_usd, s.read_only`

const sessionFrom = ` FROM sessions s JOIN workspaces w ON w.id = s.workspace_id`

func scanSession(row interface{ Scan(...any) error }) (*Session, error) {
	var s Session
	var command, created, updated string
	var exit sql.NullInt64
	var ended sql.NullString
	if err := row.Scan(&s.ID, &s.WorkspaceID, &s.ProjectID, &s.Adapter, &s.Task, &command, &s.Model,
		&s.PermissionMode, &s.Role, &s.Status, &s.Error, &exit, &s.ResumeID, &s.PID, &s.CostUSD,
		&created, &updated, &ended, &s.ModelID, &s.InputTokens, &s.OutputTokens, &s.CacheReadTokens,
		&s.CacheWriteTokens, &s.Unpriced, &s.Metered, &s.BudgetUSD, &s.MonthlyAllowUSD, &s.ReadOnly); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(command), &s.Command)
	if exit.Valid {
		c := int(exit.Int64)
		s.ExitCode = &c
	}
	s.CreatedAt, s.UpdatedAt = parseTime(created), parseTime(updated)
	if ended.Valid {
		t := parseTime(ended.String)
		s.EndedAt = &t
	}
	return &s, nil
}

// CreateSession creates a session.
func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	cmd, err := json.Marshal(sess.Command)
	if err != nil {
		return err
	}
	if sess.Command == nil {
		cmd = []byte("[]")
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, workspace_id, adapter, task, command, model, permission_mode, role, status, resume_id,
		 model_id, metered, budget_usd, read_only)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.WorkspaceID, sess.Adapter, sess.Task, string(cmd), sess.Model, sess.PermissionMode,
		sess.Role, sess.Status, sess.ResumeID, sess.ModelID, sess.Metered, sess.BudgetUSD, sess.ReadOnly)
	if isForeignKeyViolation(err) {
		return apperr.NotFound("workspace %s not found", sess.WorkspaceID)
	}
	if err != nil {
		return fmt.Errorf("store session: %w", err)
	}
	got, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		return err
	}
	*sess = *got
	return nil
}

// GetSession returns a session.
func (s *Store) GetSession(ctx context.Context, id string) (*Session, error) {
	sess, err := scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionCols+sessionFrom+` WHERE s.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.NotFound("session %s not found", id)
	}
	return sess, err
}

// SessionFilter restricts ListSessions; empty fields do not filter.
type SessionFilter struct {
	ProjectID   string
	WorkspaceID string
	// Active only returns sessions in ActiveSessionStates.
	Active bool
	// Limit limits the count (0: 200).
	Limit int
}

// ListSessions returns sessions, newest first.
func (s *Store) ListSessions(ctx context.Context, f SessionFilter) ([]*Session, error) {
	var where []string
	var args []any
	if f.ProjectID != "" {
		where, args = append(where, "w.project_id = ?"), append(args, f.ProjectID)
	}
	if f.WorkspaceID != "" {
		where, args = append(where, "s.workspace_id = ?"), append(args, f.WorkspaceID)
	}
	if f.Active {
		where = append(where, "s.status IN ('starting', 'running', 'idle')")
	}
	q := `SELECT ` + sessionCols + sessionFrom
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ") //nolint:gosec // G202: only fixed conditions, values as parameters
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	q += ` ORDER BY s.created_at DESC, s.id LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// SessionUpdate contains the changeable fields; nil means unchanged.
type SessionUpdate struct {
	Status   *string
	Error    *string
	ExitCode *int
	ResumeID *string
	PID      *int
	// Metered, BudgetUSD and MonthlyAllowUSD set the fields of the same name.
	Metered         *bool
	BudgetUSD       *float64
	MonthlyAllowUSD *float64
	// Ended sets ended_at to now; ClearEnded resets it (resume).
	Ended      bool
	ClearEnded bool
	// ClearExitCode clears exit_code (resume).
	ClearExitCode bool
}

// UpdateSession changes the set fields.
func (s *Store) UpdateSession(ctx context.Context, id string, u SessionUpdate) error {
	now := time.Now().UTC().Format(timeLayout)
	var ended any
	switch {
	case u.Ended:
		ended = now
	case u.ClearEnded:
		ended = nil
	}
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET
		status    = COALESCE(?, status),
		error     = COALESCE(?, error),
		exit_code = CASE WHEN ? THEN NULL ELSE COALESCE(?, exit_code) END,
		resume_id = COALESCE(?, resume_id),
		pid       = COALESCE(?, pid),
		metered   = COALESCE(?, metered),
		budget_usd = COALESCE(?, budget_usd),
		monthly_allow_usd = COALESCE(?, monthly_allow_usd),
		ended_at  = CASE WHEN ? THEN ? ELSE ended_at END,
		updated_at = ?
		WHERE id = ?`,
		u.Status, u.Error, u.ClearExitCode, u.ExitCode, u.ResumeID, u.PID, u.Metered, u.BudgetUSD, u.MonthlyAllowUSD,
		u.Ended || u.ClearEnded, ended, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.NotFound("session %s not found", id)
	}
	return nil
}

// timeLayout is the time format in the database (UTC, milliseconds).
const timeLayout = "2006-01-02T15:04:05.000Z"
