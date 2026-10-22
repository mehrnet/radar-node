package store

import (
	"context"
	"fmt"
	"os"
	"time"
)

const (
	DefaultRetention = 30 * 24 * time.Hour
	DefaultMaxBytes  = 32 << 20
	pruneChunk       = 2000
	vacuumChunkPages = 512
	maxVacuumChunks  = 64
)

type MaintainOptions struct {
	Retention time.Duration
	MaxBytes  int64
}

type MaintainStats struct {
	DeletedRows     int64
	DeletedRollups  int64
	RolledUpResults int64
	PrunedTriggers  int64
	VacuumedPages   int64
	LiveBytes       int64
	DBBytes         int64
	WALBytes        int64
	OverBudget      bool
}

// Maintain bounds the database by age and size. Delivered and rejected
// results are prunable; pending results are never deleted, even when
// that leaves the database over its byte budget.
func (s *Store) Maintain(ctx context.Context, opts MaintainOptions) (MaintainStats, error) {
	var stats MaintainStats
	rolled, err := s.rollupResults(ctx)
	if err != nil {
		return stats, err
	}
	stats.RolledUpResults = rolled
	if opts.Retention > 0 {
		cutoff := time.Now().Add(-opts.Retention).UnixMilli()
		for {
			result, err := s.db.ExecContext(ctx, `
				DELETE FROM journal WHERE seq IN (
					SELECT j.seq FROM journal j
					 WHERE (
						(j.kind = 'result' AND j.delivery_state <> 0) OR
						(j.kind IN ('trigger', 'trigger_done') AND NOT EXISTS (
							SELECT 1 FROM pending_triggers p WHERE p.event_seq = j.seq
						))
					 ) AND j.at_ms < ?
					 ORDER BY j.seq
					 LIMIT ?)`, cutoff, pruneChunk)
			if err != nil {
				return stats, fmt.Errorf("store: prune journal: %w", err)
			}
			affected, _ := result.RowsAffected()
			stats.DeletedRows += affected
			if affected < pruneChunk {
				break
			}
		}
		result, err := s.db.ExecContext(ctx, "DELETE FROM completed_triggers WHERE completed_at_ms < ?", cutoff)
		if err != nil {
			return stats, fmt.Errorf("store: prune completed triggers: %w", err)
		}
		stats.PrunedTriggers = rowsAffected(result)
	}

	// Only the newest assignment is needed to rebuild current state;
	// older snapshots would just bloat the journal.
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM journal
		 WHERE kind = 'assignment'
		   AND seq NOT IN (
		       SELECT max(seq) FROM journal WHERE kind = 'assignment' GROUP BY origin
		   )`)
	if err != nil {
		return stats, fmt.Errorf("store: collapse assignments: %w", err)
	}
	stats.DeletedRows += rowsAffected(result)

	if opts.MaxBytes > 0 {
		for chunk := 0; chunk < 100; chunk++ {
			live, err := s.liveBytes(ctx)
			if err != nil {
				return stats, err
			}
			if live <= opts.MaxBytes {
				break
			}
			result, err := s.db.ExecContext(ctx, `
				DELETE FROM journal WHERE seq IN (
					SELECT j.seq FROM journal j
					 WHERE (
						(j.kind = 'result' AND j.delivery_state <> 0) OR
						(j.kind IN ('trigger', 'trigger_done') AND NOT EXISTS (
							SELECT 1 FROM pending_triggers p WHERE p.event_seq = j.seq
						))
					 )
					 ORDER BY j.seq
					 LIMIT ?)`, pruneChunk)
			if err != nil {
				return stats, fmt.Errorf("store: enforce size: %w", err)
			}
			affected, _ := result.RowsAffected()
			stats.DeletedRows += affected
			if affected == 0 {
				// Raw history is exhausted; give up the oldest hourly
				// rollups before declaring the cap impossible.
				result, err := s.db.ExecContext(ctx, `
					DELETE FROM rollups
					 WHERE (origin, probe_id, bucket_ms) IN (
					     SELECT origin, probe_id, bucket_ms FROM rollups
					      ORDER BY bucket_ms
					      LIMIT ?)`, pruneChunk)
				if err != nil {
					return stats, fmt.Errorf("store: prune rollups: %w", err)
				}
				rolled := rowsAffected(result)
				stats.DeletedRollups += rolled
				if rolled == 0 {
					stats.OverBudget = true
					break
				}
			}
		}
	}

	freed, err := s.incrementalVacuum(ctx)
	if err != nil {
		return stats, err
	}
	stats.VacuumedPages = freed

	var busy, logPages, checkpointed int
	_ = s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logPages, &checkpointed)
	_, _ = s.db.ExecContext(ctx, "PRAGMA optimize")

	live, err := s.liveBytes(ctx)
	if err != nil {
		return stats, err
	}
	stats.LiveBytes = live
	if opts.MaxBytes > 0 && live > opts.MaxBytes {
		stats.OverBudget = true
	}
	if info, err := os.Stat(s.path); err == nil {
		stats.DBBytes = info.Size()
	}
	if info, err := os.Stat(s.path + "-wal"); err == nil {
		stats.WALBytes = info.Size()
	}
	return stats, nil
}

func (s *Store) liveBytes(ctx context.Context) (int64, error) {
	var pageSize, pageCount, freePages int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("store: page size: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
		return 0, fmt.Errorf("store: page count: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&freePages); err != nil {
		return 0, fmt.Errorf("store: free pages: %w", err)
	}
	live := pageCount - freePages
	if live < 0 {
		live = 0
	}
	return live * pageSize, nil
}

func (s *Store) incrementalVacuum(ctx context.Context) (int64, error) {
	var current int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&current); err != nil {
		return 0, fmt.Errorf("store: free pages before vacuum: %w", err)
	}
	initial := current
	for i := 0; i < maxVacuumChunks && current > 0; i++ {
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", vacuumChunkPages)); err != nil {
			return 0, fmt.Errorf("store: incremental vacuum: %w", err)
		}
		var after int64
		if err := s.db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&after); err != nil {
			return 0, fmt.Errorf("store: free pages after vacuum: %w", err)
		}
		if after >= current {
			break
		}
		current = after
	}
	return initial - current, nil
}

func rowsAffected(result interface{ RowsAffected() (int64, error) }) int64 {
	affected, _ := result.RowsAffected()
	return affected
}
