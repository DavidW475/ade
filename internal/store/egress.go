package store

import (
	"context"
	"fmt"
	"time"
)

// EgressEvent is a request from a sandbox container to the egress proxy.
type EgressEvent struct {
	ID          int64     `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	TS          time.Time `json:"ts"`
	Kind        string    `json:"kind"`
	Method      string    `json:"method,omitempty"`
	Host        string    `json:"host"`
	Port        int       `json:"port"`
	Path        string    `json:"path,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	Allowed     bool      `json:"allowed"`
	Reason      string    `json:"reason"`
	Status      int       `json:"status,omitempty"`
	BytesOut    int64     `json:"bytes_out"`
	BytesIn     int64     `json:"bytes_in"`
	DurationMS  int64     `json:"duration_ms"`
	Error       string    `json:"error,omitempty"`
}

// EgressKeep is the number of events kept per workspace.
const EgressKeep = 2000

// AppendEgress stores events of a workspace and deletes the oldest beyond EgressKeep.
// Events of a workspace that does not exist (any more) are discarded.
func (s *Store) AppendEgress(ctx context.Context, workspaceID string, evs []EgressEvent) error {
	if len(evs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workspaces WHERE id = ?`, workspaceID).Scan(&n); err != nil || n == 0 {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO egress_events
		(workspace_id, ts, kind, method, host, port, path, provider, allowed, reason, status, bytes_out, bytes_in, duration_ms, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, e := range evs {
		if _, err := stmt.ExecContext(ctx, workspaceID, e.TS.UTC().Format(timeLayout), e.Kind, e.Method, e.Host, e.Port, e.Path,
			e.Provider, e.Allowed, e.Reason, e.Status, e.BytesOut, e.BytesIn, e.DurationMS, e.Error); err != nil {
			return fmt.Errorf("store egress event: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM egress_events WHERE workspace_id = ? AND id <= (
		SELECT id FROM egress_events WHERE workspace_id = ? ORDER BY id DESC LIMIT 1 OFFSET ?)`,
		workspaceID, workspaceID, EgressKeep); err != nil {
		return err
	}
	return tx.Commit()
}

// EgressFilter restricts ListEgress.
type EgressFilter struct {
	// BlockedOnly: only blocked requests.
	BlockedOnly bool
	// Limit (default 200, at most EgressKeep).
	Limit int
}

// ListEgress returns the newest events of a workspace (newest first).
func (s *Store) ListEgress(ctx context.Context, workspaceID string, f EgressFilter) ([]EgressEvent, error) {
	if f.Limit <= 0 {
		f.Limit = 200
	}
	f.Limit = min(f.Limit, EgressKeep)
	q := `SELECT id, ts, kind, method, host, port, path, provider, allowed, reason, status, bytes_out, bytes_in, duration_ms, error
		FROM egress_events WHERE workspace_id = ?`
	if f.BlockedOnly {
		q += ` AND allowed = 0`
	}
	q += ` ORDER BY id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, workspaceID, f.Limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []EgressEvent{}
	for rows.Next() {
		e := EgressEvent{WorkspaceID: workspaceID}
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.Kind, &e.Method, &e.Host, &e.Port, &e.Path, &e.Provider, &e.Allowed, &e.Reason,
			&e.Status, &e.BytesOut, &e.BytesIn, &e.DurationMS, &e.Error); err != nil {
			return nil, err
		}
		e.TS = parseTime(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// EgressHost summarizes the events of a workspace for one host.
type EgressHost struct {
	Host     string    `json:"host"`
	Allowed  int       `json:"allowed"`
	Blocked  int       `json:"blocked"`
	BytesOut int64     `json:"bytes_out"`
	BytesIn  int64     `json:"bytes_in"`
	Last     time.Time `json:"last"`
	// Reason is the reason of the last blocked request.
	Reason string `json:"reason,omitempty"`
}

// EgressHosts summarizes the kept events of a workspace per host (most recently used
// first).
func (s *Store) EgressHosts(ctx context.Context, workspaceID string) ([]EgressHost, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT host,
			SUM(allowed), SUM(1 - allowed), SUM(bytes_out), SUM(bytes_in), MAX(ts),
			COALESCE((SELECT reason FROM egress_events b WHERE b.workspace_id = a.workspace_id AND b.host = a.host AND b.allowed = 0 ORDER BY id DESC LIMIT 1), '')
		FROM egress_events a WHERE workspace_id = ? GROUP BY host ORDER BY MAX(id) DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []EgressHost{}
	for rows.Next() {
		var h EgressHost
		var last string
		if err := rows.Scan(&h.Host, &h.Allowed, &h.Blocked, &h.BytesOut, &h.BytesIn, &last, &h.Reason); err != nil {
			return nil, err
		}
		h.Last = parseTime(last)
		out = append(out, h)
	}
	return out, rows.Err()
}
