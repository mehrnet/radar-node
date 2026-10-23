package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

const (
	rollupBucketMS = int64(60 * 60 * 1000)
	rollupChunk    = 2000
)

type rollupKey struct {
	origin  string
	probeID string
	bucket  int64
}

type rollupAgg struct {
	checks       int64
	ok           int64
	errors       int64
	latencySum   int64
	latencyCount int64
	latencyMin   sql.NullInt64
	latencyMax   sql.NullInt64
}

type rollupRow struct {
	seq       int64
	origin    string
	probeID   string
	atMS      int64
	ok        int64
	latencyUS sql.NullInt64
}

// Rollup is one hourly bucket, suitable for long-range charts.
type Rollup struct {
	Origin       string
	ProbeID      string
	BucketMS     int64
	Checks       int64
	OK           int64
	Errors       int64
	LatencySumUS int64
	LatencyCount int64
	LatencyMinUS *int64
	LatencyMaxUS *int64
}

// Rollups returns hourly buckets in [fromMS, toMS). An empty probeID
// returns every probe; limit is capped at 10000 buckets.
func (s *Store) Rollups(ctx context.Context, probeID string, fromMS, toMS int64, limit int) ([]Rollup, error) {
	if limit <= 0 {
		limit = 1000
	}
	if limit > 10000 {
		limit = 10000
	}
	query := `
		SELECT origin, probe_id, bucket_ms, checks, ok, errors,
		       latency_sum_us, latency_count, latency_min_us, latency_max_us
		  FROM rollups
		 WHERE bucket_ms >= ? AND bucket_ms < ?`
	args := []any{fromMS, toMS}
	if probeID != "" {
		query += " AND probe_id = ?"
		args = append(args, probeID)
	}
	query += " ORDER BY bucket_ms, probe_id, origin LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query rollups: %w", err)
	}
	defer rows.Close()
	var out []Rollup
	for rows.Next() {
		var (
			rollup   Rollup
			minValue sql.NullInt64
			maxValue sql.NullInt64
		)
		if err := rows.Scan(&rollup.Origin, &rollup.ProbeID, &rollup.BucketMS,
			&rollup.Checks, &rollup.OK, &rollup.Errors,
			&rollup.LatencySumUS, &rollup.LatencyCount, &minValue, &maxValue); err != nil {
			return nil, fmt.Errorf("store: scan rollup: %w", err)
		}
		if minValue.Valid {
			value := minValue.Int64
			rollup.LatencyMinUS = &value
		}
		if maxValue.Valid {
			value := maxValue.Int64
			rollup.LatencyMaxUS = &value
		}
		out = append(out, rollup)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: rollup rows: %w", err)
	}
	return out, nil
}

// rollupResults folds journal results into hourly buckets exactly once,
// advancing a cursor in the same transaction as each chunk's upserts.
// It runs before retention pruning so raw results can be discarded
// without losing their long-term aggregates.
var errRollupCursorMoved = errors.New("store: rollup cursor moved")

func (s *Store) rollupResults(ctx context.Context) (int64, error) {
	after, err := s.metaInt(ctx, "rollup_seq")
	if err != nil {
		return 0, err
	}
	var total int64
	moves := 0
	for {
		rows, err := s.readRollupChunk(ctx, after)
		if err != nil {
			return total, err
		}
		if len(rows) == 0 {
			return total, nil
		}
		aggregated := map[rollupKey]*rollupAgg{}
		lastSeq := after
		for _, row := range rows {
			bucket := row.atMS / rollupBucketMS * rollupBucketMS
			if bucket < 0 {
				bucket = 0
			}
			key := rollupKey{origin: row.origin, probeID: row.probeID, bucket: bucket}
			agg := aggregated[key]
			if agg == nil {
				agg = &rollupAgg{}
				aggregated[key] = agg
			}
			agg.checks++
			if row.ok != 0 {
				agg.ok++
			} else {
				agg.errors++
			}
			if row.latencyUS.Valid {
				value := row.latencyUS.Int64
				agg.latencySum += value
				agg.latencyCount++
				if !agg.latencyMin.Valid || value < agg.latencyMin.Int64 {
					agg.latencyMin = sql.NullInt64{Int64: value, Valid: true}
				}
				if !agg.latencyMax.Valid || value > agg.latencyMax.Int64 {
					agg.latencyMax = sql.NullInt64{Int64: value, Valid: true}
				}
			}
			if row.seq > lastSeq {
				lastSeq = row.seq
			}
		}
		err = s.writeRollups(ctx, aggregated, after, lastSeq)
		if errors.Is(err, errRollupCursorMoved) {
			// Another maintainer advanced the cursor while this chunk
			// was being aggregated. Re-read from its cursor instead of
			// writing a second copy of rows it already counted.
			moves++
			if moves > 5 {
				return total, fmt.Errorf("store: rollup cursor kept moving: %w", err)
			}
			after, err = s.metaInt(ctx, "rollup_seq")
			if err != nil {
				return total, err
			}
			continue
		}
		if err != nil {
			return total, err
		}
		total += int64(len(rows))
		after = lastSeq
	}
}

func (s *Store) readRollupChunk(ctx context.Context, after int64) ([]rollupRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, origin, probe_id, at_ms, ok, latency_us
		  FROM journal
		 WHERE kind = 'result' AND seq > ?
		 ORDER BY seq
		 LIMIT ?`, after, rollupChunk)
	if err != nil {
		return nil, fmt.Errorf("store: read rollup chunk: %w", err)
	}
	defer rows.Close()

	out := make([]rollupRow, 0, rollupChunk)
	for rows.Next() {
		var row rollupRow
		if err := rows.Scan(&row.seq, &row.origin, &row.probeID, &row.atMS, &row.ok, &row.latencyUS); err != nil {
			return nil, fmt.Errorf("store: scan rollup row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: rollup rows: %w", err)
	}
	return out, nil
}

func (s *Store) writeRollups(ctx context.Context, aggregated map[rollupKey]*rollupAgg, expectedCursor, nextCursor int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin rollups: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var raw sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'rollup_seq'").Scan(&raw)
	current := int64(0)
	if err == nil {
		current, err = strconv.ParseInt(raw.String, 10, 64)
		if err != nil {
			return fmt.Errorf("store: parse rollup cursor: %w", err)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: read rollup cursor: %w", err)
	}
	if current != expectedCursor {
		return errRollupCursorMoved
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO rollups (
			origin, probe_id, bucket_ms, checks, ok, errors,
			latency_sum_us, latency_count, latency_min_us, latency_max_us
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(origin, probe_id, bucket_ms) DO UPDATE SET
			checks = checks + excluded.checks,
			ok = ok + excluded.ok,
			errors = errors + excluded.errors,
			latency_sum_us = latency_sum_us + excluded.latency_sum_us,
			latency_count = latency_count + excluded.latency_count,
			latency_min_us = CASE
				WHEN excluded.latency_count = 0 THEN latency_min_us
				WHEN latency_min_us IS NULL OR excluded.latency_min_us < latency_min_us THEN excluded.latency_min_us
				ELSE latency_min_us END,
			latency_max_us = CASE
				WHEN excluded.latency_count = 0 THEN latency_max_us
				WHEN latency_max_us IS NULL OR excluded.latency_max_us > latency_max_us THEN excluded.latency_max_us
				ELSE latency_max_us END`)
	if err != nil {
		return fmt.Errorf("store: prepare rollup upsert: %w", err)
	}
	defer stmt.Close()

	for key, agg := range aggregated {
		if _, err := stmt.ExecContext(ctx,
			key.origin, key.probeID, key.bucket, agg.checks, agg.ok, agg.errors,
			agg.latencySum, agg.latencyCount, agg.latencyMin, agg.latencyMax,
		); err != nil {
			return fmt.Errorf("store: upsert rollup: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO meta (key, value) VALUES ('rollup_seq', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", strconv.FormatInt(nextCursor, 10)); err != nil {
		return fmt.Errorf("store: advance rollup cursor: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit rollups: %w", err)
	}
	return nil
}

func (s *Store) metaInt(ctx context.Context, key string) (int64, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: read meta %s: %w", key, err)
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("store: parse meta %s: %w", key, err)
	}
	return value, nil
}
