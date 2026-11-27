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

// Session-Status.
const (
	// SessionStarting: angelegt, wartet z. B. auf das Setup des Workspaces.
	SessionStarting = "starting"
	// SessionRunning: der Agent arbeitet.
	SessionRunning = "running"
	// SessionIdle: der Prozess läuft und wartet auf eine Folgeanweisung.
	SessionIdle = "idle"
	// SessionDone: Prozess regulär beendet.
	SessionDone = "done"
	// SessionFailed: Prozess mit Fehler beendet oder nicht startbar.
	SessionFailed = "failed"
	// SessionStopped: vom Nutzer gestoppt.
	SessionStopped = "stopped"
	// SessionInterrupted: durch Beenden oder Absturz des Daemons unterbrochen.
	SessionInterrupted = "interrupted"
	// SessionPaused: Budget überschritten; wartet auf eine Freigabe (M8).
	SessionPaused = "paused"
)

// ActiveSessionStates sind Status, in denen zu einer Session ein Prozess gehört (oder gleich gehören wird).
var ActiveSessionStates = []string{SessionStarting, SessionRunning, SessionIdle}

// SessionActive meldet, ob status ein aktiver Status ist.
func SessionActive(status string) bool {
	switch status {
	case SessionStarting, SessionRunning, SessionIdle:
		return true
	}
	return false
}

// Session ist ein Lauf eines Agenten (oder Terminals) in einem Workspace.
type Session struct {
	ID          string   `json:"id"`
	WorkspaceID string   `json:"workspace_id"`
	ProjectID   string   `json:"project_id"`
	Adapter     string   `json:"adapter"`
	Task        string   `json:"task,omitempty"`
	Command     []string `json:"command,omitempty"`
	Model       string   `json:"model,omitempty"`
	// ModelID ist die aufgelöste Modell-ID, wenn Model ein Alias ist.
	ModelID        string  `json:"model_id,omitempty"`
	PermissionMode string  `json:"permission_mode,omitempty"`
	Role           string  `json:"role,omitempty"`
	Status         string  `json:"status"`
	Error          string  `json:"error,omitempty"`
	ExitCode       *int    `json:"exit_code,omitempty"`
	ResumeID       string  `json:"resume_id,omitempty"`
	PID            int     `json:"-"`
	CostUSD        float64 `json:"cost_usd"`
	// Tokens sind die Summen der Nutzung (Tabelle usage).
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	// Unpriced: Für einen Teil der Tokens war kein Preis bekannt (Kosten unvollständig).
	Unpriced bool `json:"unpriced,omitempty"`
	// Metered: Der Egress-Proxy misst die Nutzung (Sandbox mit Secret-Injection).
	Metered bool `json:"metered,omitempty"`
	// BudgetUSD ist das Budget der Session (0: keins).
	BudgetUSD float64 `json:"budget_usd,omitempty"`
	// MonthlyAllowUSD: Nach Freigabe darf die Session bis zu diesen Monatskosten des
	// Projekts weiterlaufen.
	MonthlyAllowUSD float64 `json:"monthly_allow_usd,omitempty"`
	// ReadOnly: Session einer Lese-Rolle einer Fabrik (Workspace nur lesend, M9).
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

// CreateSession legt eine Session an.
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
		return apperr.NotFound("Workspace %s nicht gefunden", sess.WorkspaceID)
	}
	if err != nil {
		return fmt.Errorf("Session speichern: %w", err)
	}
	got, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		return err
	}
	*sess = *got
	return nil
}

// GetSession liefert eine Session.
func (s *Store) GetSession(ctx context.Context, id string) (*Session, error) {
	sess, err := scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionCols+sessionFrom+` WHERE s.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.NotFound("Session %s nicht gefunden", id)
	}
	return sess, err
}

// SessionFilter schränkt ListSessions ein; leere Felder filtern nicht.
type SessionFilter struct {
	ProjectID   string
	WorkspaceID string
	// Active liefert nur Sessions in ActiveSessionStates.
	Active bool
	// Limit begrenzt die Anzahl (0: 200).
	Limit int
}

// ListSessions liefert Sessions, neueste zuerst.
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
		q += ` WHERE ` + strings.Join(where, " AND ") //nolint:gosec // G202: nur feste Bedingungen, Werte als Parameter
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

// SessionUpdate enthält die änderbaren Felder; nil bedeutet unverändert.
type SessionUpdate struct {
	Status   *string
	Error    *string
	ExitCode *int
	ResumeID *string
	PID      *int
	// Metered, BudgetUSD und MonthlyAllowUSD setzen die gleichnamigen Felder.
	Metered         *bool
	BudgetUSD       *float64
	MonthlyAllowUSD *float64
	// Ended setzt ended_at auf jetzt; ClearEnded setzt es zurück (Fortsetzen).
	Ended      bool
	ClearEnded bool
	// ClearExitCode löscht exit_code (Fortsetzen).
	ClearExitCode bool
}

// UpdateSession ändert die gesetzten Felder.
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
		return apperr.NotFound("Session %s nicht gefunden", id)
	}
	return nil
}

// timeLayout ist das Zeitformat in der Datenbank (UTC, Millisekunden).
const timeLayout = "2006-01-02T15:04:05.000Z"
