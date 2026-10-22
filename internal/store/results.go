package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/mehrnet/radar-node/internal/wire"
)

type RecordSummary struct {
	Inserted   int
	Duplicates int
	Skipped    int
}

// RecordResults appends local results to the journal and marks them
// pending delivery. It is idempotent: replaying the same batch inserts
// nothing and reports the duplicates. The whole batch is one transaction.
func (s *Store) RecordResults(ctx context.Context, results []wire.Result) (RecordSummary, error) {
	var summary RecordSummary
	if len(results) == 0 {
		return summary, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return summary, fmt.Errorf("store: begin results: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	start, err := nextOriginSeq(ctx, tx, s.origin, len(results))
	if err != nil {
		return summary, err
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO journal (
			kind, origin, origin_seq, at_ms,
			run_id, probe_id, prober, target, check_seq, ok, latency_us,
			error_code, error, extra, delivery_state
		) VALUES ('result', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT DO NOTHING`)
	if err != nil {
		return summary, fmt.Errorf("store: prepare result insert: %w", err)
	}
	defer stmt.Close()

	nowMS := time.Now().UnixMilli()
	for index, result := range results {
		if result.RunID == "" || result.ProbeID == "" {
			summary.Skipped++
			continue
		}
		checkSeq := result.Seq
		if checkSeq < 1 {
			checkSeq = 1
		}
		atMS := observedAtMS(result.ObservedAt, nowMS)
		latencyUS, err := latencyMicros(result.LatencyMs)
		if err != nil {
			return summary, err
		}
		extra, err := extraJSON(result.Extra)
		if err != nil {
			return summary, fmt.Errorf("store: encode result extra: %w", err)
		}
		ok := 0
		if result.Ok {
			ok = 1
		}
		inserted, err := stmt.ExecContext(ctx,
			s.origin, start+int64(index), atMS,
			result.RunID, result.ProbeID, result.Type, result.Target, checkSeq, ok, latencyUS,
			nullableString(result.ErrorCode), nullableString(result.Error), extra,
		)
		if err != nil {
			return summary, fmt.Errorf("store: insert result: %w", err)
		}
		affected, err := inserted.RowsAffected()
		if err != nil {
			return summary, fmt.Errorf("store: result insert count: %w", err)
		}
		if affected == 0 {
			summary.Duplicates++
		} else {
			summary.Inserted++
		}
	}
	if err := tx.Commit(); err != nil {
		return summary, fmt.Errorf("store: commit results: %w", err)
	}
	return summary, nil
}

// PendingResult is one undelivered local result and its journal cursor.
type PendingResult struct {
	Seq    int64
	Result wire.Result
}

func (s *Store) PendingResults(ctx context.Context, limit int) ([]PendingResult, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, run_id, probe_id, prober, target, check_seq, ok, latency_us,
		       error_code, error, extra, at_ms
		  FROM journal
		 WHERE kind = 'result' AND origin = ? AND delivery_state = 0
		 ORDER BY seq
		 LIMIT ?`, s.origin, limit)
	if err != nil {
		return nil, fmt.Errorf("store: pending results: %w", err)
	}
	defer rows.Close()

	var pending []PendingResult
	for rows.Next() {
		var (
			seq       int64
			atMS      int64
			runID     sql.NullString
			probeID   sql.NullString
			prober    sql.NullString
			target    sql.NullString
			checkSeq  sql.NullInt64
			ok        sql.NullInt64
			latencyUS sql.NullInt64
			errorCode sql.NullString
			errorText sql.NullString
			extra     []byte
		)
		if err := rows.Scan(
			&seq, &runID, &probeID, &prober, &target, &checkSeq, &ok, &latencyUS,
			&errorCode, &errorText, &extra, &atMS,
		); err != nil {
			return nil, fmt.Errorf("store: scan pending result: %w", err)
		}
		result, err := resultFromRow(runID, probeID, prober, target, checkSeq, ok, latencyUS, errorCode, errorText, extra, atMS)
		if err != nil {
			return nil, err
		}
		pending = append(pending, PendingResult{Seq: seq, Result: result})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: pending result rows: %w", err)
	}
	return pending, nil
}

type Outcome struct {
	Seq      int64
	Accepted bool
	Reason   string
}

// MarkOutcomes settles a posted batch. Accepted rows become delivered;
// rejected rows are retained with the server's reason but never retried.
func (s *Store) MarkOutcomes(ctx context.Context, outcomes []Outcome) error {
	if len(outcomes) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin outcomes: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	accepted, err := tx.PrepareContext(ctx, `
		UPDATE journal
		   SET delivery_state = 1, delivered_at_ms = ?, delivery_reason = NULL
		 WHERE seq = ? AND kind = 'result' AND delivery_state = 0`)
	if err != nil {
		return fmt.Errorf("store: prepare accepted outcome: %w", err)
	}
	defer accepted.Close()
	rejected, err := tx.PrepareContext(ctx, `
		UPDATE journal
		   SET delivery_state = 2, delivered_at_ms = ?, delivery_reason = ?
		 WHERE seq = ? AND kind = 'result' AND delivery_state = 0`)
	if err != nil {
		return fmt.Errorf("store: prepare rejected outcome: %w", err)
	}
	defer rejected.Close()

	nowMS := time.Now().UnixMilli()
	for _, outcome := range outcomes {
		if outcome.Seq == 0 {
			continue
		}
		if outcome.Accepted {
			if _, err := accepted.ExecContext(ctx, nowMS, outcome.Seq); err != nil {
				return fmt.Errorf("store: mark result delivered: %w", err)
			}
			continue
		}
		if _, err := rejected.ExecContext(ctx, nowMS, outcome.Reason, outcome.Seq); err != nil {
			return fmt.Errorf("store: mark result rejected: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit outcomes: %w", err)
	}
	return nil
}

// MarkAttempts increments the retry counter for a batch before it is
// posted. It is diagnostic only; delivery state remains pending.
func (s *Store) MarkAttempts(ctx context.Context, seqs []int64) error {
	for start := 0; start < len(seqs); start += 500 {
		end := start + 500
		if end > len(seqs) {
			end = len(seqs)
		}
		chunk := seqs[start:end]
		args := make([]any, len(chunk))
		placeholders := make([]string, len(chunk))
		for i, seq := range chunk {
			args[i] = seq
			placeholders[i] = "?"
		}
		query := "UPDATE journal SET attempts = attempts + 1 WHERE kind = 'result' AND delivery_state = 0 AND seq IN (" + strings.Join(placeholders, ",") + ")"
		if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("store: mark attempts: %w", err)
		}
	}
	return nil
}

func nextOriginSeq(ctx context.Context, tx *sql.Tx, origin string, count int) (int64, error) {
	if count < 1 {
		return 0, errors.New("store: sequence count must be positive")
	}
	var next int64
	err := tx.QueryRowContext(ctx, "SELECT next_seq FROM sequences WHERE origin = ?", origin).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO sequences (origin, next_seq) VALUES (?, ?)", origin, int64(count)+1); err != nil {
			return 0, fmt.Errorf("store: initialize origin sequence: %w", err)
		}
		return 1, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: read origin sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE sequences SET next_seq = ? WHERE origin = ?", next+int64(count), origin); err != nil {
		return 0, fmt.Errorf("store: advance origin sequence: %w", err)
	}
	return next, nil
}

func observedAtMS(raw string, fallbackMS int64) int64 {
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || parsed.IsZero() {
		return fallbackMS
	}
	return parsed.UnixMilli()
}

func latencyMicros(ms *float64) (any, error) {
	if ms == nil {
		return nil, nil
	}
	if math.IsNaN(*ms) || math.IsInf(*ms, 0) {
		return nil, errors.New("store: non-finite latency")
	}
	value := int64(math.Round(*ms * 1000))
	if value < 0 {
		value = 0
	}
	return value, nil
}

func extraJSON(extra map[string]any) ([]byte, error) {
	if len(extra) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(extra)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
