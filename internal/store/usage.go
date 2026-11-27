package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Quellen einer Nutzung.
const (
	UsageProxy = "proxy" // aus API-Antworten, gezählt vom Egress-Proxy
	UsageCLI   = "cli"   // gemeldet von der Agent-CLI
)

// Usage ist eine gemeldete Token-Nutzung.
type Usage struct {
	ID               int64     `json:"id"`
	TS               time.Time `json:"ts"`
	ProjectID        string    `json:"project_id"`
	WorkspaceID      string    `json:"workspace_id"`
	SessionID        string    `json:"session_id,omitempty"`
	Source           string    `json:"source"`
	Provider         string    `json:"provider,omitempty"`
	Model            string    `json:"model,omitempty"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	CacheReadTokens  int64     `json:"cache_read_tokens"`
	CacheWriteTokens int64     `json:"cache_write_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	Priced           bool      `json:"priced"`
}

// AddUsage speichert eine Nutzung und addiert sie zur Session (falls gesetzt) – in einer
// Transaktion, damit Summen und Einträge zusammenpassen.
func (s *Store) AddUsage(ctx context.Context, u *Usage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if u.TS.IsZero() {
		u.TS = time.Now()
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO usage (ts, project_id, workspace_id, session_id, source, provider, model,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd, priced)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.TS.UTC().Format(timeLayout), u.ProjectID, u.WorkspaceID, u.SessionID, u.Source, u.Provider, u.Model,
		u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.CostUSD, u.Priced)
	if isForeignKeyViolation(err) {
		return fmt.Errorf("Nutzung speichern: Projekt %s unbekannt", u.ProjectID)
	}
	if err != nil {
		return fmt.Errorf("Nutzung speichern: %w", err)
	}
	u.ID, _ = res.LastInsertId()
	if u.SessionID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET
			cost_usd = cost_usd + ?, input_tokens = input_tokens + ?, output_tokens = output_tokens + ?,
			cache_read_tokens = cache_read_tokens + ?, cache_write_tokens = cache_write_tokens + ?,
			unpriced = unpriced OR ?, updated_at = ?
			WHERE id = ?`,
			u.CostUSD, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, !u.Priced,
			time.Now().UTC().Format(timeLayout), u.SessionID); err != nil {
			return fmt.Errorf("Session-Kosten speichern: %w", err)
		}
	}
	return tx.Commit()
}

// UsageTotal ist eine Summe von Nutzungen.
type UsageTotal struct {
	CostUSD          float64 `json:"cost_usd"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	// Unpriced: Ein Teil der Tokens hatte keinen bekannten Preis.
	Unpriced bool `json:"unpriced,omitempty"`
	Count    int  `json:"count"`
}

// UsageFilter schränkt UsageTotals ein; leere Felder filtern nicht.
type UsageFilter struct {
	ProjectID   string
	WorkspaceID string
	SessionID   string
	// Since: nur Einträge ab diesem Zeitpunkt.
	Since time.Time
}

func (f UsageFilter) where() (string, []any) {
	var where []string
	var args []any
	if f.ProjectID != "" {
		where, args = append(where, "project_id = ?"), append(args, f.ProjectID)
	}
	if f.WorkspaceID != "" {
		where, args = append(where, "workspace_id = ?"), append(args, f.WorkspaceID)
	}
	if f.SessionID != "" {
		where, args = append(where, "session_id = ?"), append(args, f.SessionID)
	}
	if !f.Since.IsZero() {
		where, args = append(where, "ts >= ?"), append(args, f.Since.UTC().Format(timeLayout))
	}
	if len(where) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

// UsageTotal summiert die Nutzung.
func (s *Store) UsageTotal(ctx context.Context, f UsageFilter) (UsageTotal, error) {
	where, args := f.where()
	var t UsageTotal
	var unpriced sql.NullBool
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd), 0), COALESCE(SUM(input_tokens), 0),
		COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cache_read_tokens), 0), COALESCE(SUM(cache_write_tokens), 0),
		MAX(NOT priced), COUNT(*) FROM usage`+where, args...). //nolint:gosec // G202: nur feste Bedingungen
		Scan(&t.CostUSD, &t.InputTokens, &t.OutputTokens, &t.CacheReadTokens, &t.CacheWriteTokens, &unpriced, &t.Count)
	t.Unpriced = unpriced.Valid && unpriced.Bool
	return t, err
}

// UsageByProject liefert die Summen aller Projekte (Schlüssel: Projekt-ID).
func (s *Store) UsageByProject(ctx context.Context, since time.Time) (map[string]UsageTotal, error) {
	return s.usageGrouped(ctx, "project_id", UsageFilter{Since: since})
}

// UsageByWorkspace liefert die Summen der Workspaces eines Projekts (Schlüssel: Workspace-ID).
func (s *Store) UsageByWorkspace(ctx context.Context, projectID string) (map[string]UsageTotal, error) {
	return s.usageGrouped(ctx, "workspace_id", UsageFilter{ProjectID: projectID})
}

func (s *Store) usageGrouped(ctx context.Context, col string, f UsageFilter) (map[string]UsageTotal, error) {
	where, args := f.where()
	//nolint:gosec // G202: col ist fest, Werte als Parameter
	q := `SELECT ` + col + `, COALESCE(SUM(cost_usd), 0), COALESCE(SUM(input_tokens), 0),
		COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cache_read_tokens), 0), COALESCE(SUM(cache_write_tokens), 0),
		MAX(NOT priced), COUNT(*) FROM usage` + where + ` GROUP BY ` + col
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]UsageTotal{}
	for rows.Next() {
		var k string
		var t UsageTotal
		var unpriced sql.NullBool
		if err := rows.Scan(&k, &t.CostUSD, &t.InputTokens, &t.OutputTokens, &t.CacheReadTokens, &t.CacheWriteTokens, &unpriced, &t.Count); err != nil {
			return nil, err
		}
		t.Unpriced = unpriced.Valid && unpriced.Bool
		out[k] = t
	}
	return out, rows.Err()
}

// MonthStart ist der Beginn des Kalendermonats (UTC) von t.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}
