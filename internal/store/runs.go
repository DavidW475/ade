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

// Status eines Runs (Software-Fabrik, M9).
const (
	RunRunning  = "running"  // eine Rolle arbeitet (oder startet gleich)
	RunWaiting  = "waiting"  // wartet an einem Gate auf eine Freigabe
	RunPaused   = "paused"   // Budget der laufenden Session überschritten
	RunDone     = "done"     // alle Schritte erledigt
	RunFailed   = "failed"   // ein Schritt ist fehlgeschlagen
	RunCanceled = "canceled" // abgebrochen oder an einem Gate abgelehnt
)

// RunActive meldet, ob ein Run noch weiterläuft.
func RunActive(status string) bool {
	switch status {
	case RunRunning, RunWaiting, RunPaused:
		return true
	}
	return false
}

// Status eines Schritts.
const (
	StepPending  = "pending"
	StepRunning  = "running"
	StepWaiting  = "waiting"
	StepDone     = "done"
	StepFailed   = "failed"
	StepCanceled = "canceled"
)

// Run ist ein Lauf einer Fabrik.
type Run struct {
	ID          string            `json:"id"`
	ProjectID   string            `json:"project_id"`
	WorkspaceID string            `json:"workspace_id,omitempty"`
	Factory     string            `json:"factory"`
	Source      string            `json:"source,omitempty"`
	Definition  json.RawMessage   `json:"-"`
	Inputs      map[string]string `json:"inputs"`
	Task        string            `json:"task,omitempty"`
	Sandbox     bool              `json:"sandbox"`
	Status      string            `json:"status"`
	Current     int               `json:"current"`
	Error       string            `json:"error,omitempty"`
	BudgetUSD   float64           `json:"budget_usd,omitempty"`
	// MaxDuration ist die erlaubte Arbeitszeit der Agenten (0: keine Grenze).
	MaxDuration time.Duration `json:"max_duration_ns,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
	EndedAt     *time.Time    `json:"ended_at,omitempty"`
	Steps       []RunStep     `json:"steps"`
	// CostUSD ist die Summe der Nutzung aller Sessions des Runs (Tabelle usage, bleibt nach
	// dem Verwerfen des Workspaces erhalten).
	CostUSD float64 `json:"cost_usd"`
}

// RunStep ist ein Schritt eines Runs.
type RunStep struct {
	Index      int    `json:"index"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Role       string `json:"role,omitempty"`
	ReadOnly   bool   `json:"read_only,omitempty"`
	Status     string `json:"status"`
	SessionID  string `json:"session_id,omitempty"`
	ApprovalID string `json:"approval_id,omitempty"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
	// Fixes zählt die Korrektur-Runden für ein ungültiges strukturiertes Artefakt.
	Fixes     int        `json:"fixes,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

const runCols = `r.id, r.project_id, r.workspace_id, r.factory, r.source, r.definition, r.inputs, r.task, r.sandbox,
	r.status, r.current, r.error, r.budget_usd, r.max_duration_s, r.created_at, r.updated_at, r.ended_at,
	COALESCE((SELECT SUM(u.cost_usd) FROM usage u WHERE u.session_id IN
		(SELECT rs.session_id FROM run_steps rs WHERE rs.run_id = r.id AND rs.session_id != '')), 0)`

func scanRun(row interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var def, inputs, created, updated string
	var ended sql.NullString
	var maxDur int64
	if err := row.Scan(&r.ID, &r.ProjectID, &r.WorkspaceID, &r.Factory, &r.Source, &def, &inputs, &r.Task, &r.Sandbox,
		&r.Status, &r.Current, &r.Error, &r.BudgetUSD, &maxDur, &created, &updated, &ended, &r.CostUSD); err != nil {
		return nil, err
	}
	r.Definition = json.RawMessage(def)
	_ = json.Unmarshal([]byte(inputs), &r.Inputs)
	if r.Inputs == nil {
		r.Inputs = map[string]string{}
	}
	r.MaxDuration = time.Duration(maxDur) * time.Second
	r.CreatedAt, r.UpdatedAt, r.EndedAt = parseTime(created), parseTime(updated), nullTime(ended)
	return &r, nil
}

// CreateRun legt einen Run mit seinen Schritten an.
func (s *Store) CreateRun(ctx context.Context, r *Run) error {
	inputs, err := json.Marshal(r.Inputs)
	if err != nil {
		return err
	}
	if r.Inputs == nil {
		inputs = []byte("{}")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO runs (id, project_id, workspace_id, factory, source, definition, inputs, task,
		sandbox, status, budget_usd, max_duration_s) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.ProjectID, r.WorkspaceID, r.Factory, r.Source, string(r.Definition), string(inputs), r.Task,
		r.Sandbox, r.Status, r.BudgetUSD, int64(r.MaxDuration/time.Second))
	if isForeignKeyViolation(err) {
		return apperr.NotFound("Projekt %s nicht gefunden", r.ProjectID)
	}
	if err != nil {
		return fmt.Errorf("Run speichern: %w", err)
	}
	for i, st := range r.Steps {
		status := st.Status
		if status == "" {
			status = StepPending
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO run_steps (run_id, idx, name, kind, role, read_only, status, output)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, r.ID, i, st.Name, st.Kind, st.Role, st.ReadOnly, status, st.Output); err != nil {
			return fmt.Errorf("Schritt speichern: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	got, err := s.GetRun(ctx, r.ID)
	if err != nil {
		return err
	}
	*r = *got
	return nil
}

// GetRun liefert einen Run mit Schritten.
func (s *Store) GetRun(ctx context.Context, id string) (*Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs r WHERE r.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.NotFound("Run %s nicht gefunden", id)
	}
	if err != nil {
		return nil, err
	}
	if r.Steps, err = s.runSteps(ctx, id); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *Store) runSteps(ctx context.Context, runID string) ([]RunStep, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT idx, name, kind, role, read_only, status, session_id, approval_id,
		output, error, fixes, started_at, ended_at FROM run_steps WHERE run_id = ? ORDER BY idx`, runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []RunStep{}
	for rows.Next() {
		var st RunStep
		var started, ended sql.NullString
		if err := rows.Scan(&st.Index, &st.Name, &st.Kind, &st.Role, &st.ReadOnly, &st.Status, &st.SessionID,
			&st.ApprovalID, &st.Output, &st.Error, &st.Fixes, &started, &ended); err != nil {
			return nil, err
		}
		st.StartedAt, st.EndedAt = nullTime(started), nullTime(ended)
		out = append(out, st)
	}
	return out, rows.Err()
}

// RunFilter schränkt ListRuns ein; leere Felder filtern nicht.
type RunFilter struct {
	ProjectID   string
	WorkspaceID string
	Active      bool
	// Limit (Standard 100, höchstens 1000).
	Limit int
}

// ListRuns liefert Runs mit Schritten, neueste zuerst.
func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]*Run, error) {
	var where []string
	var args []any
	if f.ProjectID != "" {
		where, args = append(where, "r.project_id = ?"), append(args, f.ProjectID)
	}
	if f.WorkspaceID != "" {
		where, args = append(where, "r.workspace_id = ?"), append(args, f.WorkspaceID)
	}
	if f.Active {
		where = append(where, "r.status IN ('running', 'waiting', 'paused')")
	}
	q := `SELECT ` + runCols + ` FROM runs r`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ") //nolint:gosec // G202: nur feste Bedingungen, Werte als Parameter
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	q += ` ORDER BY r.created_at DESC, r.id LIMIT ?`
	args = append(args, min(limit, 1000))
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := []*Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range out {
		if r.Steps, err = s.runSteps(ctx, r.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// RunUpdate sind Änderungen an einem Run; nil-Felder bleiben unverändert.
type RunUpdate struct {
	WorkspaceID *string
	Status      *string
	Current     *int
	Error       *string
	// Ended setzt ended_at auf jetzt.
	Ended bool
}

// UpdateRun ändert die gesetzten Felder.
func (s *Store) UpdateRun(ctx context.Context, id string, u RunUpdate) error {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET
		workspace_id = COALESCE(?1, workspace_id),
		status       = COALESCE(?2, status),
		current      = COALESCE(?3, current),
		error        = COALESCE(?4, error),
		ended_at     = CASE WHEN ?5 THEN strftime('%Y-%m-%dT%H:%M:%fZ', 'now') ELSE ended_at END,
		updated_at   = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?6`, u.WorkspaceID, u.Status, u.Current, u.Error, u.Ended, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.NotFound("Run %s nicht gefunden", id)
	}
	return nil
}

// StepUpdate sind Änderungen an einem Schritt; nil-Felder bleiben unverändert.
type StepUpdate struct {
	Status     *string
	SessionID  *string
	ApprovalID *string
	Error      *string
	Fixes      *int
	// Started bzw. Ended setzen started_at bzw. ended_at auf jetzt.
	Started bool
	Ended   bool
}

// UpdateRunStep ändert die gesetzten Felder eines Schritts.
func (s *Store) UpdateRunStep(ctx context.Context, runID string, idx int, u StepUpdate) error {
	res, err := s.db.ExecContext(ctx, `UPDATE run_steps SET
		status      = COALESCE(?1, status),
		session_id  = COALESCE(?2, session_id),
		approval_id = COALESCE(?3, approval_id),
		error       = COALESCE(?4, error),
		started_at  = CASE WHEN ?5 THEN strftime('%Y-%m-%dT%H:%M:%fZ', 'now') ELSE started_at END,
		ended_at    = CASE WHEN ?6 THEN strftime('%Y-%m-%dT%H:%M:%fZ', 'now') ELSE ended_at END,
		fixes       = COALESCE(?9, fixes)
		WHERE run_id = ?7 AND idx = ?8`, u.Status, u.SessionID, u.ApprovalID, u.Error, u.Started, u.Ended, runID, idx, u.Fixes)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.NotFound("Schritt %d von Run %s nicht gefunden", idx, runID)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, runID); err != nil {
		return err
	}
	return nil
}

// RunOfSession liefert den Run, zu dem eine Session gehört (leer: keiner).
func (s *Store) RunOfSession(ctx context.Context, sessionID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT run_id FROM run_steps WHERE session_id = ? LIMIT 1`, sessionID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// RunOfApproval liefert den Run, zu dem eine Freigabe (Gate) gehört (leer: keiner).
func (s *Store) RunOfApproval(ctx context.Context, approvalID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT run_id FROM run_steps WHERE approval_id = ? LIMIT 1`, approvalID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
