package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mehrnet/radar-node/internal/wire"
)

type PendingTrigger struct {
	ProbeID  string
	RunID    string
	EventSeq int64
	Probe    wire.ProbeSnapshot
}

type triggerDonePayload struct {
	ProbeID string `json:"probe_id"`
	RunID   string `json:"run_id"`
}

// RecordTrigger journals a server trigger once. Redeliveries of the
// same run are ignored until the trigger completes, and a completed run
// is remembered so a late redelivery cannot execute it again.
func (s *Store) RecordTrigger(ctx context.Context, event wire.Event) (bool, error) {
	if event.EventType != "triggered" || event.RunID == "" || event.Probe.ID == "" {
		return false, nil
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return false, fmt.Errorf("store: encode trigger event: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: begin trigger: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	seen, err := triggerAlreadySeenTx(ctx, tx, event.Probe.ID, event.RunID)
	if err != nil {
		return false, err
	}
	if seen {
		return false, nil
	}
	seq, err := nextOriginSeq(ctx, tx, s.origin, 1)
	if err != nil {
		return false, err
	}
	atMS := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO journal (kind, origin, origin_seq, at_ms, run_id, probe_id, payload)
		VALUES ('trigger', ?, ?, ?, ?, ?, ?)`,
		s.origin, seq, atMS, event.RunID, event.Probe.ID, payload,
	); err != nil {
		return false, fmt.Errorf("store: insert trigger event: %w", err)
	}
	if err := queueTriggerTx(ctx, tx, event, seq, atMS); err != nil {
		return false, err
	}
	if err := updateExistingProbeSnapshotTx(ctx, tx, event.Probe); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit trigger: %w", err)
	}
	return true, nil
}

func triggerAlreadySeenTx(ctx context.Context, tx *sql.Tx, probeID, runID string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM completed_triggers WHERE probe_id = ? AND run_id = ?", probeID, runID).Scan(&one)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("store: check completed trigger: %w", err)
	}
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM pending_triggers WHERE probe_id = ? AND run_id = ?", probeID, runID).Scan(&one)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("store: check pending trigger: %w", err)
	}
	var lastRunID sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT last_trigger_run_id FROM probes WHERE id = ?", probeID).Scan(&lastRunID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("store: check probe trigger state: %w", err)
	}
	return lastRunID.Valid && lastRunID.String == runID, nil
}

func queueTriggerTx(ctx context.Context, tx *sql.Tx, event wire.Event, eventSeq, atMS int64) error {
	snapshot, err := json.Marshal(event.Probe)
	if err != nil {
		return fmt.Errorf("store: encode trigger probe: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pending_triggers (probe_id, run_id, event_seq, created_at_ms, snapshot_json)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(probe_id, run_id) DO NOTHING`,
		event.Probe.ID, event.RunID, eventSeq, atMS, snapshot,
	); err != nil {
		return fmt.Errorf("store: queue trigger: %w", err)
	}
	return nil
}

// PendingTriggers restores triggers that survived a restart.
func (s *Store) PendingTriggers(ctx context.Context) ([]PendingTrigger, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT probe_id, run_id, event_seq, snapshot_json
		  FROM pending_triggers
		 ORDER BY event_seq`)
	if err != nil {
		return nil, fmt.Errorf("store: pending triggers: %w", err)
	}
	defer rows.Close()

	var triggers []PendingTrigger
	for rows.Next() {
		var (
			trigger  PendingTrigger
			snapshot []byte
		)
		if err := rows.Scan(&trigger.ProbeID, &trigger.RunID, &trigger.EventSeq, &snapshot); err != nil {
			return nil, fmt.Errorf("store: scan pending trigger: %w", err)
		}
		if err := json.Unmarshal(snapshot, &trigger.Probe); err != nil {
			return nil, fmt.Errorf("store: decode trigger probe %s: %w", trigger.ProbeID, err)
		}
		triggers = append(triggers, trigger)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: pending trigger rows: %w", err)
	}
	return triggers, nil
}

// CompleteTriggers retires executed triggers and journals the completion
// so Rebuild can reproduce the queue exactly.
func (s *Store) CompleteTriggers(ctx context.Context, triggers []PendingTrigger) error {
	if len(triggers) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin trigger completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	nowMS := time.Now().UnixMilli()
	for _, trigger := range triggers {
		deleted, err := tx.ExecContext(ctx, "DELETE FROM pending_triggers WHERE probe_id = ? AND run_id = ?", trigger.ProbeID, trigger.RunID)
		if err != nil {
			return fmt.Errorf("store: delete pending trigger: %w", err)
		}
		if affected, _ := deleted.RowsAffected(); affected == 0 {
			continue
		}
		seq, err := nextOriginSeq(ctx, tx, s.origin, 1)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(triggerDonePayload{ProbeID: trigger.ProbeID, RunID: trigger.RunID})
		if err != nil {
			return fmt.Errorf("store: encode trigger completion: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO journal (kind, origin, origin_seq, at_ms, run_id, probe_id, payload)
			VALUES ('trigger_done', ?, ?, ?, ?, ?, ?)`,
			s.origin, seq, nowMS, trigger.RunID, trigger.ProbeID, payload,
		); err != nil {
			return fmt.Errorf("store: insert trigger completion: %w", err)
		}
		if err := completeTriggerTx(ctx, tx, trigger.ProbeID, trigger.RunID, nowMS); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit trigger completion: %w", err)
	}
	return nil
}

func completeTriggerTx(ctx context.Context, tx *sql.Tx, probeID, runID string, atMS int64) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM pending_triggers WHERE probe_id = ? AND run_id = ?", probeID, runID); err != nil {
		return fmt.Errorf("store: clear completed trigger: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO completed_triggers (probe_id, run_id, completed_at_ms)
		VALUES (?, ?, ?)
		ON CONFLICT(probe_id, run_id) DO UPDATE SET completed_at_ms = excluded.completed_at_ms`,
		probeID, runID, atMS,
	); err != nil {
		return fmt.Errorf("store: record completed trigger: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE probes SET last_trigger_run_id = ? WHERE id = ?", runID, probeID); err != nil {
		return fmt.Errorf("store: record probe trigger state: %w", err)
	}
	return nil
}
