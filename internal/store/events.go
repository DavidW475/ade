package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Event ist ein gespeichertes Ereignis einer Session (Schema: doc/architecture.md#event-schema).
// seq ist pro Session fortlaufend und beginnt bei 1.
type Event struct {
	Seq       int64           `json:"seq"`
	SessionID string          `json:"session_id"`
	TS        time.Time       `json:"ts"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// AppendEvents speichert Events in einer Transaktion (gedrosseltes Schreiben: der Aufrufer
// sammelt Events und schreibt sie gebündelt).
func (s *Store) AppendEvents(ctx context.Context, evs []Event) error {
	if len(evs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO events (session_id, seq, ts, type, payload) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, e := range evs {
		payload := e.Payload
		if len(payload) == 0 {
			payload = json.RawMessage("{}")
		}
		if _, err := stmt.ExecContext(ctx, e.SessionID, e.Seq, e.TS.UTC().Format(timeLayout), e.Type, string(payload)); err != nil {
			return fmt.Errorf("Event %s/%d speichern: %w", e.SessionID, e.Seq, err)
		}
	}
	return tx.Commit()
}

// ListEvents liefert bis zu limit Events einer Session mit seq > since, aufsteigend.
func (s *Store) ListEvents(ctx context.Context, sessionID string, since int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, ts, type, payload FROM events WHERE session_id = ? AND seq > ? ORDER BY seq LIMIT ?`,
		sessionID, since, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Event{}
	for rows.Next() {
		e := Event{SessionID: sessionID}
		var ts, payload string
		if err := rows.Scan(&e.Seq, &ts, &e.Type, &payload); err != nil {
			return nil, err
		}
		e.TS = parseTime(ts)
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

// MaxEventSeq liefert die höchste seq einer Session (0, wenn es keine Events gibt).
func (s *Store) MaxEventSeq(ctx context.Context, sessionID string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events WHERE session_id = ?`, sessionID).Scan(&n)
	return n, err
}
