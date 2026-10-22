package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mehrnet/radar-node/internal/wire"
)

type assignmentPayload struct {
	Hash   string               `json:"hash"`
	Probes []wire.ProbeSnapshot `json:"probes"`
}

// SaveAssignment records a full server assignment and replaces the
// probe projection in one transaction. Per-probe schedule state is
// carried across the replace so a resync never makes every probe look
// overdue at once.
func (s *Store) SaveAssignment(ctx context.Context, hash string, probes []wire.ProbeSnapshot) error {
	if hash == "" {
		return nil
	}
	payload, err := json.Marshal(assignmentPayload{Hash: hash, Probes: probes})
	if err != nil {
		return fmt.Errorf("store: encode assignment: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin assignment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	seq, err := nextOriginSeq(ctx, tx, s.origin, 1)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO journal (kind, origin, origin_seq, at_ms, payload)
		VALUES ('assignment', ?, ?, ?, ?)`,
		s.origin, seq, time.Now().UnixMilli(), payload,
	); err != nil {
		return fmt.Errorf("store: insert assignment: %w", err)
	}
	if err := applyAssignmentTx(ctx, tx, probes, true); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO meta (key, value) VALUES ('probe_hash', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", hash); err != nil {
		return fmt.Errorf("store: save probe hash: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit assignment: %w", err)
	}
	return nil
}

type probeScheduleState struct {
	lastRunAt        sql.NullInt64
	lastTriggerRunID sql.NullString
}

func applyAssignmentTx(ctx context.Context, tx *sql.Tx, probes []wire.ProbeSnapshot, preserve bool) error {
	existing := map[string]probeScheduleState{}
	if preserve {
		rows, err := tx.QueryContext(ctx, "SELECT id, last_run_at_ms, last_trigger_run_id FROM probes")
		if err != nil {
			return fmt.Errorf("store: read existing probes: %w", err)
		}
		for rows.Next() {
			var id string
			var state probeScheduleState
			if err := rows.Scan(&id, &state.lastRunAt, &state.lastTriggerRunID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: scan existing probe: %w", err)
			}
			existing[id] = state
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: existing probe rows: %w", err)
		}
		_ = rows.Close()
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM probes"); err != nil {
		return fmt.Errorf("store: clear probes: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO probes (
			id, target, prober, probe_count, timeout_ms, schedule_type,
			interval_seconds, starts_at_ms, ends_at_ms, params_json, status,
			last_run_at_ms, last_trigger_run_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("store: prepare probe insert: %w", err)
	}
	defer stmt.Close()
	for _, snapshot := range probes {
		var params []byte
		if len(snapshot.Params) > 0 {
			params, err = json.Marshal(snapshot.Params)
			if err != nil {
				return fmt.Errorf("store: encode probe %s params: %w", snapshot.ID, err)
			}
		}
		state := existing[snapshot.ID]
		if _, err := stmt.ExecContext(ctx,
			snapshot.ID, snapshot.Target, snapshot.Prober, snapshot.ProbeCount,
			snapshot.TimeoutMs, snapshot.ScheduleType, snapshot.IntervalSeconds,
			snapshot.StartsAt, snapshot.EndsAt, params, snapshot.Status,
			state.lastRunAt, state.lastTriggerRunID,
		); err != nil {
			return fmt.Errorf("store: insert probe %s: %w", snapshot.ID, err)
		}
	}
	return nil
}

// ProbeState is the projection row plus the local schedule state the
// cache needs after a restart.
type ProbeState struct {
	Snapshot         wire.ProbeSnapshot
	LastRunAt        time.Time
	LastTriggerRunID string
}

// LoadAssignment restores the last synced assignment and the local
// schedule state it was carrying.
func (s *Store) LoadAssignment(ctx context.Context) (string, []wire.ProbeSnapshot, error) {
	hash, states, err := s.LoadProbeStates(ctx)
	if err != nil {
		return "", nil, err
	}
	probes := make([]wire.ProbeSnapshot, len(states))
	for i, state := range states {
		probes[i] = state.Snapshot
	}
	return hash, probes, nil
}

func (s *Store) LoadProbeStates(ctx context.Context) (string, []ProbeState, error) {
	var hash string
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'probe_hash'").Scan(&hash); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", nil, fmt.Errorf("store: read probe hash: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, target, prober, probe_count, timeout_ms, schedule_type,
		       interval_seconds, starts_at_ms, ends_at_ms, params_json, status,
		       last_run_at_ms, last_trigger_run_id
		  FROM probes
		 ORDER BY id`)
	if err != nil {
		return "", nil, fmt.Errorf("store: load probes: %w", err)
	}
	defer rows.Close()

	var states []ProbeState
	for rows.Next() {
		var (
			state          ProbeState
			snapshot       wire.ProbeSnapshot
			params         []byte
			lastRunAtMS    sql.NullInt64
			lastTriggerRun sql.NullString
		)
		if err := rows.Scan(
			&snapshot.ID, &snapshot.Target, &snapshot.Prober, &snapshot.ProbeCount,
			&snapshot.TimeoutMs, &snapshot.ScheduleType, &snapshot.IntervalSeconds,
			&snapshot.StartsAt, &snapshot.EndsAt, &params, &snapshot.Status,
			&lastRunAtMS, &lastTriggerRun,
		); err != nil {
			return "", nil, fmt.Errorf("store: scan probe: %w", err)
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &snapshot.Params); err != nil {
				return "", nil, fmt.Errorf("store: decode probe %s params: %w", snapshot.ID, err)
			}
		}
		state.Snapshot = snapshot
		if lastRunAtMS.Valid {
			state.LastRunAt = time.UnixMilli(lastRunAtMS.Int64)
		}
		if lastTriggerRun.Valid {
			state.LastTriggerRunID = lastTriggerRun.String
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return "", nil, fmt.Errorf("store: probe rows: %w", err)
	}
	return hash, states, nil
}

// MarkProbesRun persists due-probe bookkeeping after a scheduler tick.
// It is intentionally not journaled: it is local scheduling state, and
// Rebuild can derive it from the newest result per probe.
func (s *Store) MarkProbesRun(ctx context.Context, ids []string, at time.Time) error {
	atMS := at.UnixMilli()
	for start := 0; start < len(ids); start += 500 {
		end := start + 500
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, atMS)
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query := "UPDATE probes SET last_run_at_ms = ? WHERE id IN (" + strings.Join(placeholders, ",") + ")"
		if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("store: mark probes run: %w", err)
		}
	}
	return nil
}

// Rebuild recreates every projection from the journal. It is used by
// recovery tooling and tests; results themselves are already durable
// rows in the journal.
func (s *Store) Rebuild(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin rebuild: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, table := range []string{"probes", "pending_triggers", "completed_triggers"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("store: clear %s: %w", table, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM meta WHERE key = 'probe_hash'"); err != nil {
		return fmt.Errorf("store: clear probe hash: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT seq, kind, at_ms, payload
		  FROM journal
		 WHERE kind IN ('assignment', 'trigger', 'trigger_done')
		 ORDER BY seq`)
	if err != nil {
		return fmt.Errorf("store: read control events: %w", err)
	}
	for rows.Next() {
		var (
			seq     int64
			kind    string
			atMS    int64
			payload []byte
		)
		if err := rows.Scan(&seq, &kind, &atMS, &payload); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: scan control event: %w", err)
		}
		switch kind {
		case "assignment":
			var assignment assignmentPayload
			if err := json.Unmarshal(payload, &assignment); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: decode assignment event: %w", err)
			}
			if err := applyAssignmentTx(ctx, tx, assignment.Probes, false); err != nil {
				_ = rows.Close()
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO meta (key, value) VALUES ('probe_hash', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", assignment.Hash); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: restore probe hash: %w", err)
			}
		case "trigger":
			var event wire.Event
			if err := json.Unmarshal(payload, &event); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: decode trigger event: %w", err)
			}
			if err := queueTriggerTx(ctx, tx, event, seq, atMS); err != nil {
				_ = rows.Close()
				return err
			}
		case "trigger_done":
			var done triggerDonePayload
			if err := json.Unmarshal(payload, &done); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: decode trigger_done event: %w", err)
			}
			if err := completeTriggerTx(ctx, tx, done.ProbeID, done.RunID, atMS); err != nil {
				_ = rows.Close()
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("store: control event rows: %w", err)
	}
	_ = rows.Close()

	if _, err := tx.ExecContext(ctx, `
		UPDATE probes
		   SET last_run_at_ms = (
		       SELECT max(j.at_ms) FROM journal j
		        WHERE j.kind = 'result' AND j.probe_id = probes.id
		   )
		 WHERE EXISTS (
		       SELECT 1 FROM journal j
		        WHERE j.kind = 'result' AND j.probe_id = probes.id
		 )`); err != nil {
		return fmt.Errorf("store: restore probe run times: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit rebuild: %w", err)
	}
	return nil
}
