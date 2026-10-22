package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/wire"
)

func TestRollupsAreIncrementalAndIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	hour := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)

	first := testResult("run_1", "p1", 1, hour.Add(5*time.Minute))
	second := testResult("run_2", "p1", 1, hour.Add(20*time.Minute))
	second.Ok = false
	second.LatencyMs = nil
	second.Error = "timeout"
	third := testResult("run_3", "p2", 1, hour.Add(70*time.Minute))
	if _, err := s.RecordResults(ctx, []wire.Result{first, second, third}); err != nil {
		t.Fatalf("RecordResults: %v", err)
	}

	stats, err := s.Maintain(ctx, MaintainOptions{})
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if stats.RolledUpResults != 3 {
		t.Fatalf("rolled up = %d, want 3", stats.RolledUpResults)
	}

	rows := readRollups(t, s)
	if len(rows) != 2 {
		t.Fatalf("rollup rows = %d, want 2 hourly buckets", len(rows))
	}
	p1 := rows[0]
	if p1.probeID != "p1" || p1.checks != 2 || p1.ok != 1 || p1.errors != 1 {
		t.Fatalf("p1 rollup = %+v", p1)
	}
	if p1.latencyCount != 1 || p1.latencySum != 12500 || p1.latencyMin.Int64 != 12500 || p1.latencyMax.Int64 != 12500 {
		t.Fatalf("p1 latency rollup = %+v", p1)
	}
	p2 := rows[1]
	if p2.probeID != "p2" || p2.bucketMS != hour.Add(time.Hour).UnixMilli() {
		t.Fatalf("p2 rollup = %+v", p2)
	}
	queried, err := s.Rollups(ctx, "p1", 0, hour.Add(24*time.Hour).UnixMilli(), 10)
	if err != nil {
		t.Fatalf("Rollups: %v", err)
	}
	if len(queried) != 1 || queried[0].Checks != 2 || queried[0].LatencyCount != 1 ||
		queried[0].LatencyMinUS == nil || *queried[0].LatencyMinUS != 12500 {
		t.Fatalf("queried rollups = %+v", queried)
	}
	storeStats, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if storeStats.Rollups != 2 {
		t.Fatalf("stats rollups = %d, want 2", storeStats.Rollups)
	}

	stats, err = s.Maintain(ctx, MaintainOptions{})
	if err != nil {
		t.Fatalf("second Maintain: %v", err)
	}
	if stats.RolledUpResults != 0 {
		t.Fatalf("second maintain rolled up %d, want 0", stats.RolledUpResults)
	}

	// A late result in an already-rolled hour must accumulate, not
	// replace or double-count the existing bucket.
	late := testResult("run_4", "p1", 2, hour.Add(45*time.Minute))
	if _, err := s.RecordResults(ctx, []wire.Result{late}); err != nil {
		t.Fatalf("late RecordResults: %v", err)
	}
	if _, err := s.Maintain(ctx, MaintainOptions{}); err != nil {
		t.Fatalf("third Maintain: %v", err)
	}
	rows = readRollups(t, s)
	if rows[0].checks != 3 || rows[0].ok != 2 || rows[0].latencyCount != 2 {
		t.Fatalf("p1 after late result = %+v", rows[0])
	}
}

func TestRollupsSurviveRawRetention(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old := time.Now().Add(-60 * 24 * time.Hour).UTC()
	if _, err := s.RecordResults(ctx, []wire.Result{testResult("run_old", "p1", 1, old)}); err != nil {
		t.Fatalf("RecordResults: %v", err)
	}
	pending, err := s.PendingResults(ctx, 10)
	if err != nil {
		t.Fatalf("PendingResults: %v", err)
	}
	if err := s.MarkOutcomes(ctx, []Outcome{{Seq: pending[0].Seq, Accepted: true}}); err != nil {
		t.Fatalf("MarkOutcomes: %v", err)
	}
	if _, err := s.Maintain(ctx, MaintainOptions{Retention: 30 * 24 * time.Hour}); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	var results int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM journal WHERE kind = 'result'").Scan(&results); err != nil {
		t.Fatalf("count results: %v", err)
	}
	if results != 0 {
		t.Fatalf("raw results = %d, want 0 after retention", results)
	}
	rows := readRollups(t, s)
	if len(rows) != 1 || rows[0].checks != 1 {
		t.Fatalf("rollups after retention = %+v", rows)
	}
}

func TestSizeCapPrunesRollupsLast(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-5 * time.Hour).UTC()
	for i := 0; i < 3; i++ {
		if _, err := s.RecordResults(ctx, []wire.Result{
			testResult("run_"+string(rune('a'+i)), "p1", 1, base.Add(time.Duration(i)*time.Hour)),
		}); err != nil {
			t.Fatalf("RecordResults: %v", err)
		}
	}
	if _, err := s.Maintain(ctx, MaintainOptions{}); err != nil {
		t.Fatalf("Maintain rollups: %v", err)
	}
	if rows := readRollups(t, s); len(rows) != 3 {
		t.Fatalf("rollups = %d, want 3", len(rows))
	}

	stats, err := s.Maintain(ctx, MaintainOptions{MaxBytes: 1})
	if err != nil {
		t.Fatalf("Maintain size cap: %v", err)
	}
	if stats.DeletedRollups == 0 {
		t.Fatal("expected rollups to be pruned once raw history could not satisfy the cap")
	}
	if rows := readRollups(t, s); len(rows) != 0 {
		t.Fatalf("rollups after cap = %d, want 0", len(rows))
	}
	pending, err := s.PendingResults(ctx, 10)
	if err != nil {
		t.Fatalf("PendingResults: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("pending after cap = %d, want 3 (never pruned)", len(pending))
	}
	if !stats.OverBudget {
		t.Fatal("expected OverBudget with an impossible 1-byte cap")
	}
}

type rollupTestRow struct {
	probeID      string
	bucketMS     int64
	checks       int64
	ok           int64
	errors       int64
	latencySum   int64
	latencyCount int64
	latencyMin   sql.NullInt64
	latencyMax   sql.NullInt64
}

func readRollups(t *testing.T, s *Store) []rollupTestRow {
	t.Helper()
	rows, err := s.db.Query(`
		SELECT probe_id, bucket_ms, checks, ok, errors,
		       latency_sum_us, latency_count, latency_min_us, latency_max_us
		  FROM rollups
		 ORDER BY bucket_ms, probe_id`)
	if err != nil {
		t.Fatalf("read rollups: %v", err)
	}
	defer rows.Close()
	var out []rollupTestRow
	for rows.Next() {
		var row rollupTestRow
		if err := rows.Scan(&row.probeID, &row.bucketMS, &row.checks, &row.ok, &row.errors,
			&row.latencySum, &row.latencyCount, &row.latencyMin, &row.latencyMax); err != nil {
			t.Fatalf("scan rollup: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rollup rows: %v", err)
	}
	return out
}
