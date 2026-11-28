package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/DavidW475/ade/internal/apperr"
)

// Status of a run (software factory, M9).
const (
	RunRunning  = "running"  // a role is working (or about to start)
	RunWaiting  = "waiting"  // waits for an approval at a gate
	RunPaused   = "paused"   // budget of the running session exceeded
	RunDone     = "done"     // all steps done
	RunFailed   = "failed"   // a step failed
	RunCanceled = "canceled" // canceled or denied at a gate
)

// RunActive reports whether a run is still going on.
func RunActive(status string) bool {
	switch status {
	case RunRunning, RunWaiting, RunPaused:
		return true
	}
	return false
}

// Status of a step.
const (
	StepPending  = "pending"
	StepRunning  = "running"
	StepWaiting  = "waiting"
	StepDone     = "done"
	StepFailed   = "failed"
	StepCanceled = "canceled"
	// StepSkipped: the condition (when) was false.
	StepSkipped = "skipped"
	// StepLooped: a decision or a denied gate jumped back (Error: reason).
	StepLooped = "looped"
)

// StepFinished reports whether a step is done and the run continues after it.
func StepFinished(status string) bool {
	switch status {
	case StepDone, StepSkipped, StepLooped:
		return true
	}
	return false
}

// Run is a run of a factory.
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
	// MaxDuration is the allowed working time of the agents (0: no limit).
	MaxDuration time.Duration `json:"max_duration_ns,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
	EndedAt     *time.Time    `json:"ended_at,omitempty"`
	// Steps is the latest execution per position in the flow (Steps[Current] is the
	// current step).
	Steps []RunStep `json:"steps"`
	// History are earlier passes (loops), by position and pass.
	History []RunStep `json:"history,omitempty"`
	// Trigger is the trigger (empty: started by hand), TriggerRef its reference (e.g. the
	// issue's URL), Closes the issue the pull request closes (0: none).
	Trigger    string `json:"trigger,omitempty"`
	TriggerRef string `json:"trigger_ref,omitempty"`
	Closes     int    `json:"closes,omitempty"`
	// CostUSD is the sum of the usage of all sessions of the run (table usage, kept after
	// the workspace is discarded).
	CostUSD float64 `json:"cost_usd"`
}

// RunStep is one execution of a step.
type RunStep struct {
	// Index is the position in the flow.
	Index int `json:"index"`
	// Row is the row (unique within the run, for UpdateRunStep).
	Row int `json:"row"`
	// Group is, for members of a parallel group, the position of the first member.
	Group *int `json:"group,omitempty"`
	// Iteration is the pass (1, 2, … after jumps back).
	Iteration  int    `json:"iteration"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Role       string `json:"role,omitempty"`
	ReadOnly   bool   `json:"read_only,omitempty"`
	Status     string `json:"status"`
	SessionID  string `json:"session_id,omitempty"`
	ApprovalID string `json:"approval_id,omitempty"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
	// Fixes counts the correction rounds for an invalid structured artifact.
	Fixes     int        `json:"fixes,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

const runCols = `r.id, r.project_id, r.workspace_id, r.factory, r.source, r.definition, r.inputs, r.task, r.sandbox,
	r.status, r.current, r.error, r.budget_usd, r.max_duration_s, r.created_at, r.updated_at, r.ended_at,
	r.trigger, r.trigger_ref, r.closes,
	COALESCE((SELECT SUM(u.cost_usd) FROM usage u WHERE u.session_id IN
		(SELECT rs.session_id FROM run_steps rs WHERE rs.run_id = r.id AND rs.session_id != '')), 0)`

func scanRun(row interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var def, inputs, created, updated string
	var ended sql.NullString
	var maxDur int64
	if err := row.Scan(&r.ID, &r.ProjectID, &r.WorkspaceID, &r.Factory, &r.Source, &def, &inputs, &r.Task, &r.Sandbox,
		&r.Status, &r.Current, &r.Error, &r.BudgetUSD, &maxDur, &created, &updated, &ended,
		&r.Trigger, &r.TriggerRef, &r.Closes, &r.CostUSD); err != nil {
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

// CreateRun creates a run with its steps.
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
		sandbox, status, budget_usd, max_duration_s, trigger, trigger_ref, closes) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.ProjectID, r.WorkspaceID, r.Factory, r.Source, string(r.Definition), string(inputs), r.Task,
		r.Sandbox, r.Status, r.BudgetUSD, int64(r.MaxDuration/time.Second), r.Trigger, r.TriggerRef, r.Closes)
	if isForeignKeyViolation(err) {
		return apperr.NotFound("project %s not found", r.ProjectID)
	}
	if err != nil {
		return fmt.Errorf("store run: %w", err)
	}
	for i, st := range r.Steps {
		status := st.Status
		if status == "" {
			status = StepPending
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO run_steps (run_id, idx, pos, grp, iteration, name, kind, role, read_only, status, output)
			VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?)`, r.ID, i, i, st.Group, st.Name, st.Kind, st.Role, st.ReadOnly, status, st.Output); err != nil {
			return fmt.Errorf("store step: %w", err)
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

// GetRun returns a run with its steps.
func (s *Store) GetRun(ctx context.Context, id string) (*Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs r WHERE r.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.NotFound("run %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	if err := s.runSteps(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *Store) runSteps(ctx context.Context, r *Run) error {
	rows, err := s.db.QueryContext(ctx, `SELECT pos, idx, grp, iteration, name, kind, role, read_only, status, session_id,
		approval_id, output, error, fixes, started_at, ended_at FROM run_steps WHERE run_id = ? ORDER BY pos, iteration, idx`, r.ID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	r.Steps, r.History = []RunStep{}, nil
	for rows.Next() {
		var st RunStep
		var started, ended sql.NullString
		var grp sql.NullInt64
		if err := rows.Scan(&st.Index, &st.Row, &grp, &st.Iteration, &st.Name, &st.Kind, &st.Role, &st.ReadOnly, &st.Status,
			&st.SessionID, &st.ApprovalID, &st.Output, &st.Error, &st.Fixes, &started, &ended); err != nil {
			return err
		}
		st.StartedAt, st.EndedAt = nullTime(started), nullTime(ended)
		if grp.Valid {
			g := int(grp.Int64)
			st.Group = &g
		}
		// Per position the latest pass wins; the older ones move to History.
		if n := len(r.Steps); n > 0 && r.Steps[n-1].Index == st.Index {
			r.History = append(r.History, r.Steps[n-1])
			r.Steps[n-1] = st
			continue
		}
		r.Steps = append(r.Steps, st)
	}
	return rows.Err()
}

// Executions returns all executions (history and steps) in the order they were created.
func (r *Run) Executions() []RunStep {
	out := make([]RunStep, 0, len(r.History)+len(r.Steps))
	out = append(out, r.History...)
	out = append(out, r.Steps...)
	slices.SortFunc(out, func(a, b RunStep) int { return a.Row - b.Row })
	return out
}

// RepeatSteps creates a new pass for each position from from to to (status pending) and
// resets the run to from (jump back of a loop).
func (s *Store) RepeatSteps(ctx context.Context, runID string, from, to int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var next int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(idx), -1) + 1 FROM run_steps WHERE run_id = ?`, runID).Scan(&next); err != nil {
		return err
	}
	for pos := from; pos <= to; pos++ {
		res, err := tx.ExecContext(ctx, `INSERT INTO run_steps (run_id, idx, pos, grp, iteration, name, kind, role, read_only, output)
			SELECT run_id, ?, pos, grp, iteration + 1, name, kind, role, read_only, output FROM run_steps
			WHERE run_id = ? AND pos = ? ORDER BY iteration DESC LIMIT 1`, next, runID, pos)
		if err != nil {
			return fmt.Errorf("create pass: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return apperr.NotFound("step %d of run %s not found", pos, runID)
		}
		next++
	}
	res, err := tx.ExecContext(ctx, `UPDATE runs SET current = ?, status = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?`, from, RunRunning, runID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.NotFound("run %s not found", runID)
	}
	return tx.Commit()
}

// RunFilter restricts ListRuns; empty fields do not filter.
type RunFilter struct {
	ProjectID   string
	WorkspaceID string
	Active      bool
	// Limit (default 100, at most 1000).
	Limit int
}

// ListRuns returns runs with steps, newest first.
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
		q += ` WHERE ` + strings.Join(where, " AND ") //nolint:gosec // G202: only fixed conditions, values as parameters
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
		if err := s.runSteps(ctx, r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// RunUpdate are changes to a run; nil fields stay unchanged.
type RunUpdate struct {
	WorkspaceID *string
	Status      *string
	Current     *int
	Error       *string
	// Ended sets ended_at to now.
	Ended bool
}

// UpdateRun changes the set fields.
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
		return apperr.NotFound("run %s not found", id)
	}
	return nil
}

// StepUpdate are changes to a step; nil fields stay unchanged.
type StepUpdate struct {
	Status     *string
	SessionID  *string
	ApprovalID *string
	Error      *string
	Fixes      *int
	// Started and Ended set started_at and ended_at to now.
	Started bool
	Ended   bool
}

// UpdateRunStep changes the set fields of an execution (row: RunStep.Row).
func (s *Store) UpdateRunStep(ctx context.Context, runID string, row int, u StepUpdate) error {
	res, err := s.db.ExecContext(ctx, `UPDATE run_steps SET
		status      = COALESCE(?1, status),
		session_id  = COALESCE(?2, session_id),
		approval_id = COALESCE(?3, approval_id),
		error       = COALESCE(?4, error),
		started_at  = CASE WHEN ?5 THEN strftime('%Y-%m-%dT%H:%M:%fZ', 'now') ELSE started_at END,
		ended_at    = CASE WHEN ?6 THEN strftime('%Y-%m-%dT%H:%M:%fZ', 'now') ELSE ended_at END,
		fixes       = COALESCE(?9, fixes)
		WHERE run_id = ?7 AND idx = ?8`, u.Status, u.SessionID, u.ApprovalID, u.Error, u.Started, u.Ended, runID, row, u.Fixes)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.NotFound("step %d of run %s not found", row, runID)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, runID); err != nil {
		return err
	}
	return nil
}

// RunOfSession returns the run a session belongs to (empty: none).
func (s *Store) RunOfSession(ctx context.Context, sessionID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT run_id FROM run_steps WHERE session_id = ? LIMIT 1`, sessionID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// RunOfApproval returns the run an approval (gate) belongs to (empty: none).
func (s *Store) RunOfApproval(ctx context.Context, approvalID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT run_id FROM run_steps WHERE approval_id = ? LIMIT 1`, approvalID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
