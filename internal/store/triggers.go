package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/DavidW475/ade/internal/apperr"
)

// TriggerState is the state of a trigger.
type TriggerState struct {
	ProjectID string     `json:"project_id"`
	Name      string     `json:"name"`
	Paused    bool       `json:"paused"`
	LastCheck *time.Time `json:"last_check,omitempty"`
	LastFire  *time.Time `json:"last_fire,omitempty"`
	LastError string     `json:"last_error,omitempty"`
}

// GetTriggerState returns the state of a trigger (without an entry: empty state).
func (s *Store) GetTriggerState(ctx context.Context, projectID, name string) (*TriggerState, error) {
	st := &TriggerState{ProjectID: projectID, Name: name}
	var check, fire sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT paused, last_check, last_fire, last_error FROM trigger_state
		WHERE project_id = ? AND name = ?`, projectID, name).Scan(&st.Paused, &check, &fire, &st.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	st.LastCheck, st.LastFire = nullTime(check), nullTime(fire)
	return st, nil
}

// TriggerUpdate are changes to the state; nil fields stay unchanged.
type TriggerUpdate struct {
	Paused    *bool
	LastCheck *time.Time
	LastFire  *time.Time
	LastError *string
}

func fmtTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// UpdateTriggerState creates the state or changes the set fields.
func (s *Store) UpdateTriggerState(ctx context.Context, projectID, name string, u TriggerUpdate) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO trigger_state (project_id, name, paused, last_check, last_fire, last_error)
		VALUES (?1, ?2, COALESCE(?3, 0), ?4, ?5, COALESCE(?6, ''))
		ON CONFLICT (project_id, name) DO UPDATE SET
			paused     = COALESCE(?3, paused),
			last_check = COALESCE(?4, last_check),
			last_fire  = COALESCE(?5, last_fire),
			last_error = COALESCE(?6, last_error)`,
		projectID, name, u.Paused, fmtTime(u.LastCheck), fmtTime(u.LastFire), u.LastError)
	if isForeignKeyViolation(err) {
		return apperr.NotFound("project %s not found", projectID)
	}
	return err
}

// TriggerRun is a triggered run.
type TriggerRun struct {
	ProjectID string    `json:"project_id"`
	Trigger   string    `json:"trigger"`
	Key       string    `json:"key"`
	RunID     string    `json:"run_id"`
	Reported  bool      `json:"reported"`
	CreatedAt time.Time `json:"created_at"`
}

// HasTriggerRun reports whether the trigger has already started a run for key.
func (s *Store) HasTriggerRun(ctx context.Context, projectID, trigger, key string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM trigger_runs WHERE project_id = ? AND trigger = ? AND key = ?`,
		projectID, trigger, key).Scan(&n)
	return n > 0, err
}

// AddTriggerRun records a triggered run; if key already exists, that is a conflict.
func (s *Store) AddTriggerRun(ctx context.Context, tr *TriggerRun) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO trigger_runs (project_id, trigger, key, run_id) VALUES (?, ?, ?, ?)`,
		tr.ProjectID, tr.Trigger, tr.Key, tr.RunID)
	if isUniqueViolation(err) {
		return apperr.Conflict("trigger %s already has a run for %s", tr.Trigger, tr.Key)
	}
	return err
}

// TriggerRunFilter restricts ListTriggerRuns; empty fields do not filter.
type TriggerRunFilter struct {
	ProjectID  string
	Trigger    string
	Unreported bool
	Limit      int
}

// ListTriggerRuns returns triggered runs, newest first.
func (s *Store) ListTriggerRuns(ctx context.Context, f TriggerRunFilter) ([]TriggerRun, error) {
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT project_id, trigger, key, run_id, reported, created_at FROM trigger_runs
		WHERE (?1 = '' OR project_id = ?1) AND (?2 = '' OR trigger = ?2) AND (NOT ?3 OR reported = 0)
		ORDER BY created_at DESC, key DESC LIMIT ?4`, f.ProjectID, f.Trigger, f.Unreported, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TriggerRun
	for rows.Next() {
		var tr TriggerRun
		var created string
		if err := rows.Scan(&tr.ProjectID, &tr.Trigger, &tr.Key, &tr.RunID, &tr.Reported, &created); err != nil {
			return nil, err
		}
		tr.CreatedAt = parseTime(created)
		out = append(out, tr)
	}
	return out, rows.Err()
}

// MarkTriggerRunReported vermerkt, dass das Ende des Runs gemeldet ist.
func (s *Store) MarkTriggerRunReported(ctx context.Context, projectID, trigger, key string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE trigger_runs SET reported = 1 WHERE project_id = ? AND trigger = ? AND key = ?`,
		projectID, trigger, key)
	return err
}
