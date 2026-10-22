package store

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/probe"
	"github.com/mehrnet/radar-node/internal/wire"
)

const testOrigin = "node_test"

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "radar.db")
	s, err := Open(path, Options{Origin: testOrigin, Sync: "normal"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testProbe(id string) wire.ProbeSnapshot {
	return wire.ProbeSnapshot{
		ID:              id,
		Target:          "example.com:443",
		Prober:          "tcp",
		ProbeCount:      1,
		TimeoutMs:       5000,
		ScheduleType:    "interval",
		IntervalSeconds: 60,
		StartsAt:        time.Now().Add(-time.Hour).UnixMilli(),
		Params:          map[string]any{"tls": true},
		Status:          wire.ProbeStatusActive,
	}
}

func testResult(runID, probeID string, seq int, at time.Time) wire.Result {
	latency := 12.5
	return wire.Result{
		RunID:   runID,
		ProbeID: probeID,
		Result: probe.Result{
			Ok:        true,
			Type:      "tcp",
			Target:    "example.com:443",
			Seq:       seq,
			LatencyMs: &latency,
			Extra:     map[string]any{"tls_version": "1.3"},
		},
		ObservedAt: at.UTC().Format(time.RFC3339Nano),
	}
}

func testTrigger(probeID, runID string) wire.Event {
	return wire.Event{
		Seq:       1,
		EventType: "triggered",
		Probe:     testProbe(probeID),
		RunID:     runID,
	}
}

func TestOpenPragmasAndStats(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	stats, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.JournalMode != "wal" {
		t.Errorf("journal mode = %q, want wal", stats.JournalMode)
	}
	if stats.AutoVacuum != 2 {
		t.Errorf("auto_vacuum = %d, want 2 (incremental)", stats.AutoVacuum)
	}
	if stats.PageSize != 4096 {
		t.Errorf("page size = %d, want 4096", stats.PageSize)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("db permissions = %o, want 600", perm)
	}
	var synchronous int
	if err := s.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatalf("synchronous: %v", err)
	}
	if synchronous != 1 {
		t.Errorf("synchronous = %d, want 1 (normal)", synchronous)
	}
}

func TestRecordReplayAndPending(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.SaveAssignment(ctx, "hash-1", []wire.ProbeSnapshot{testProbe("p1"), testProbe("p2")}); err != nil {
		t.Fatalf("SaveAssignment: %v", err)
	}
	now := time.Now().UTC()
	summary, err := s.RecordResults(ctx, []wire.Result{
		testResult("run_1", "p1", 1, now),
		testResult("run_2", "p2", 1, now.Add(time.Second)),
	})
	if err != nil {
		t.Fatalf("RecordResults: %v", err)
	}
	if summary.Inserted != 2 || summary.Duplicates != 0 {
		t.Fatalf("summary = %+v, want 2 inserted", summary)
	}

	pending, err := s.PendingResults(ctx, 10)
	if err != nil {
		t.Fatalf("PendingResults: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %d, want 2", len(pending))
	}
	if pending[0].Result.RunID != "run_1" || pending[1].Result.RunID != "run_2" {
		t.Errorf("pending order = %q, %q", pending[0].Result.RunID, pending[1].Result.RunID)
	}
	if pending[0].Result.LatencyMs == nil || *pending[0].Result.LatencyMs != 12.5 {
		t.Errorf("latency = %v, want 12.5", pending[0].Result.LatencyMs)
	}
	if got := pending[0].Result.Extra["tls_version"]; got != "1.3" {
		t.Errorf("extra tls_version = %v, want 1.3", got)
	}

	var kinds []string
	var replayed []Event
	cursor, err := s.Replay(ctx, 0, 100, func(event Event) error {
		kinds = append(kinds, event.Kind)
		replayed = append(replayed, event)
		return nil
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if cursor != replayed[len(replayed)-1].Seq {
		t.Errorf("cursor = %d, want last seq %d", cursor, replayed[len(replayed)-1].Seq)
	}
	if len(kinds) != 3 || kinds[0] != "assignment" || kinds[1] != "result" || kinds[2] != "result" {
		t.Errorf("replay kinds = %v", kinds)
	}
	if replayed[1].Result == nil || replayed[1].Result.RunID != "run_1" {
		t.Errorf("replayed result = %+v", replayed[1].Result)
	}

	hash, probes, err := s.LoadAssignment(ctx)
	if err != nil {
		t.Fatalf("LoadAssignment: %v", err)
	}
	if hash != "hash-1" || len(probes) != 2 {
		t.Errorf("assignment = %q, %d probes", hash, len(probes))
	}
	if probes[0].Params["tls"] != true {
		t.Errorf("params = %v", probes[0].Params)
	}
}

func TestRecordResultsSanitizesInvalidFields(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	nan := math.NaN()
	result := testResult("run_bad_fields", "p1", 1, time.Now())
	result.LatencyMs = &nan
	result.Extra = map[string]any{"value": math.NaN()}

	summary, err := s.RecordResults(ctx, []wire.Result{result})
	if err != nil {
		t.Fatalf("RecordResults: %v", err)
	}
	if summary.Inserted != 1 || summary.InvalidLatency != 1 || summary.InvalidExtra != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	pending, err := s.PendingResults(ctx, 10)
	if err != nil {
		t.Fatalf("PendingResults: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	if pending[0].Result.LatencyMs != nil || pending[0].Result.Extra != nil {
		t.Fatalf("invalid fields were not dropped: %+v", pending[0].Result)
	}
}

func TestLatencyMicrosRejectsOverflow(t *testing.T) {
	huge := math.MaxFloat64
	if _, ok := latencyMicros(&huge); ok {
		t.Fatal("a latency whose microsecond conversion overflows must be rejected")
	}
	normal := 12.5
	if got, ok := latencyMicros(&normal); !ok || got != int64(12500) {
		t.Fatalf("latencyMicros(12.5) = %v, %v; want 12500, true", got, ok)
	}
}

func TestPendingResultsToleratesCorruptExtra(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.RecordResults(ctx, []wire.Result{testResult("run_corrupt_extra", "p1", 1, time.Now())}); err != nil {
		t.Fatalf("RecordResults: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE journal SET extra = ? WHERE kind = 'result'", []byte("{not-json")); err != nil {
		t.Fatalf("corrupt extra: %v", err)
	}
	pending, err := s.PendingResults(ctx, 10)
	if err != nil {
		t.Fatalf("PendingResults must not fail on a corrupt extra: %v", err)
	}
	if len(pending) != 1 || pending[0].Result.Extra != nil {
		t.Fatalf("pending = %+v, want one result with nil extra", pending)
	}
}

func TestRecordResultsIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	batch := []wire.Result{testResult("run_1", "p1", 1, now), testResult("run_1", "p1", 2, now)}

	first, err := s.RecordResults(ctx, batch)
	if err != nil {
		t.Fatalf("first RecordResults: %v", err)
	}
	second, err := s.RecordResults(ctx, batch)
	if err != nil {
		t.Fatalf("second RecordResults: %v", err)
	}
	if first.Inserted != 2 || second.Inserted != 0 || second.Duplicates != 2 {
		t.Fatalf("first = %+v, second = %+v", first, second)
	}
	pending, err := s.PendingResults(ctx, 10)
	if err != nil {
		t.Fatalf("PendingResults: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %d, want 2", len(pending))
	}
}

func TestOutcomesPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "radar.db")
	s, err := Open(path, Options{Origin: testOrigin})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := s.RecordResults(ctx, []wire.Result{
		testResult("run_1", "p1", 1, now),
		testResult("run_2", "p2", 1, now),
	}); err != nil {
		t.Fatalf("RecordResults: %v", err)
	}
	pending, err := s.PendingResults(ctx, 10)
	if err != nil {
		t.Fatalf("PendingResults: %v", err)
	}
	if err := s.MarkAttempts(ctx, []int64{pending[0].Seq, pending[1].Seq}); err != nil {
		t.Fatalf("MarkAttempts: %v", err)
	}
	if err := s.MarkOutcomes(ctx, []Outcome{
		{Seq: pending[0].Seq, Accepted: true},
		{Seq: pending[1].Seq, Accepted: false, Reason: "duplicate"},
	}); err != nil {
		t.Fatalf("MarkOutcomes: %v", err)
	}
	pending, err = s.PendingResults(ctx, 10)
	if err != nil {
		t.Fatalf("PendingResults after outcomes: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after outcomes = %d, want 0", len(pending))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(path, Options{Origin: testOrigin})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	stats, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Results != 2 || stats.PendingResults != 0 {
		t.Errorf("stats = %+v", stats)
	}
	var attempts int
	var reason string
	if err := s.db.QueryRowContext(ctx, "SELECT attempts, delivery_reason FROM journal WHERE run_id = 'run_2'").Scan(&attempts, &reason); err != nil {
		t.Fatalf("read rejected row: %v", err)
	}
	if attempts != 1 || reason != "duplicate" {
		t.Errorf("attempts = %d, reason = %q", attempts, reason)
	}
}

func TestAssignmentPreservesScheduleState(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.SaveAssignment(ctx, "hash-1", []wire.ProbeSnapshot{testProbe("p1"), testProbe("p2")}); err != nil {
		t.Fatalf("SaveAssignment: %v", err)
	}
	ranAt := time.Now().Add(-30 * time.Second).UTC()
	if err := s.MarkProbesRun(ctx, []string{"p1"}, ranAt); err != nil {
		t.Fatalf("MarkProbesRun: %v", err)
	}
	if err := s.SaveAssignment(ctx, "hash-2", []wire.ProbeSnapshot{testProbe("p1"), testProbe("p3")}); err != nil {
		t.Fatalf("second SaveAssignment: %v", err)
	}
	hash, probes, err := s.LoadAssignment(ctx)
	if err != nil {
		t.Fatalf("LoadAssignment: %v", err)
	}
	if hash != "hash-2" || len(probes) != 2 {
		t.Fatalf("assignment = %q, %d probes", hash, len(probes))
	}
	var lastRun int64
	if err := s.db.QueryRowContext(ctx, "SELECT last_run_at_ms FROM probes WHERE id = 'p1'").Scan(&lastRun); err != nil {
		t.Fatalf("p1 last run: %v", err)
	}
	if lastRun != ranAt.UnixMilli() {
		t.Errorf("p1 last_run = %d, want %d", lastRun, ranAt.UnixMilli())
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM probes WHERE id = 'p2'").Scan(&count); err != nil {
		t.Fatalf("p2 lookup: %v", err)
	}
	if count != 0 {
		t.Error("p2 should have been removed")
	}
}

func TestTriggerQueueAndCompletion(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.SaveAssignment(ctx, "hash-1", []wire.ProbeSnapshot{testProbe("p1")}); err != nil {
		t.Fatalf("SaveAssignment: %v", err)
	}

	inserted, err := s.RecordTrigger(ctx, testTrigger("p1", "run_t1"))
	if err != nil {
		t.Fatalf("RecordTrigger: %v", err)
	}
	if !inserted {
		t.Fatal("first trigger should be inserted")
	}
	inserted, err = s.RecordTrigger(ctx, testTrigger("p1", "run_t1"))
	if err != nil {
		t.Fatalf("duplicate RecordTrigger: %v", err)
	}
	if inserted {
		t.Fatal("duplicate trigger should be ignored")
	}
	triggers, err := s.PendingTriggers(ctx)
	if err != nil {
		t.Fatalf("PendingTriggers: %v", err)
	}
	if len(triggers) != 1 || triggers[0].RunID != "run_t1" || triggers[0].Probe.ID != "p1" {
		t.Fatalf("pending triggers = %+v", triggers)
	}
	if err := s.CompleteTriggers(ctx, triggers); err != nil {
		t.Fatalf("CompleteTriggers: %v", err)
	}
	triggers, err = s.PendingTriggers(ctx)
	if err != nil {
		t.Fatalf("PendingTriggers after completion: %v", err)
	}
	if len(triggers) != 0 {
		t.Fatalf("pending after completion = %d", len(triggers))
	}
	inserted, err = s.RecordTrigger(ctx, testTrigger("p1", "run_t1"))
	if err != nil {
		t.Fatalf("completed RecordTrigger: %v", err)
	}
	if inserted {
		t.Fatal("completed trigger should be ignored")
	}
	inserted, err = s.RecordTrigger(ctx, testTrigger("p1", "run_t2"))
	if err != nil {
		t.Fatalf("new trigger: %v", err)
	}
	if !inserted {
		t.Fatal("new run id should be inserted")
	}

	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	triggers, err = s.PendingTriggers(ctx)
	if err != nil {
		t.Fatalf("PendingTriggers after rebuild: %v", err)
	}
	if len(triggers) != 1 || triggers[0].RunID != "run_t2" {
		t.Fatalf("pending after rebuild = %+v", triggers)
	}
	inserted, err = s.RecordTrigger(ctx, testTrigger("p1", "run_t1"))
	if err != nil {
		t.Fatalf("completed RecordTrigger after rebuild: %v", err)
	}
	if inserted {
		t.Fatal("completed trigger should stay deduped after rebuild")
	}
}

func TestPendingTriggersDropsCorruptSnapshot(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.SaveAssignment(ctx, "hash-1", []wire.ProbeSnapshot{testProbe("p1")}); err != nil {
		t.Fatalf("SaveAssignment: %v", err)
	}
	if inserted, err := s.RecordTrigger(ctx, testTrigger("p1", "run_corrupt")); err != nil || !inserted {
		t.Fatalf("RecordTrigger = %v, %v", inserted, err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE pending_triggers SET snapshot_json = ? WHERE probe_id = 'p1'", []byte("{bad-json")); err != nil {
		t.Fatalf("corrupt snapshot: %v", err)
	}
	triggers, err := s.PendingTriggers(ctx)
	if err != nil {
		t.Fatalf("PendingTriggers: %v", err)
	}
	if len(triggers) != 0 {
		t.Fatalf("pending triggers = %+v, want the corrupt row dropped", triggers)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM pending_triggers WHERE probe_id = 'p1'").Scan(&count); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if count != 0 {
		t.Fatalf("corrupt pending row was not deleted")
	}
	if inserted, err := s.RecordTrigger(ctx, testTrigger("p1", "run_corrupt")); err != nil || !inserted {
		t.Fatalf("redelivery after corrupt row should queue again: inserted=%v err=%v", inserted, err)
	}
}

func TestCompleteTriggersRecordsCompletionWithoutPendingRow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.SaveAssignment(ctx, "hash-1", []wire.ProbeSnapshot{testProbe("p1")}); err != nil {
		t.Fatalf("SaveAssignment: %v", err)
	}
	if inserted, err := s.RecordTrigger(ctx, testTrigger("p1", "run_memory_only")); err != nil || !inserted {
		t.Fatalf("RecordTrigger = %v, %v", inserted, err)
	}
	// Simulate the trigger having only been applied in memory because
	// its initial persistence failed.
	if _, err := s.db.ExecContext(ctx, "DELETE FROM pending_triggers WHERE probe_id = 'p1'"); err != nil {
		t.Fatalf("delete pending: %v", err)
	}
	if err := s.CompleteTriggers(ctx, []PendingTrigger{{ProbeID: "p1", RunID: "run_memory_only"}}); err != nil {
		t.Fatalf("CompleteTriggers: %v", err)
	}
	var completed int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM completed_triggers WHERE probe_id = 'p1' AND run_id = 'run_memory_only'").Scan(&completed); err != nil {
		t.Fatalf("completed trigger: %v", err)
	}
	if completed != 1 {
		t.Fatalf("completed trigger row = %d, want 1", completed)
	}
	if inserted, err := s.RecordTrigger(ctx, testTrigger("p1", "run_memory_only")); err != nil || inserted {
		t.Fatalf("completed run should stay deduped: inserted=%v err=%v", inserted, err)
	}
}

func TestTriggerSnapshotDoesNotOverwriteAssignmentProjection(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	current := testProbe("p1")
	current.Target = "current.example:443"
	if err := s.SaveAssignment(ctx, "hash-current", []wire.ProbeSnapshot{current}); err != nil {
		t.Fatalf("SaveAssignment: %v", err)
	}
	event := testTrigger("p1", "run_old")
	event.Probe.Target = "old.example:443"
	if inserted, err := s.RecordTrigger(ctx, event); err != nil || !inserted {
		t.Fatalf("RecordTrigger = %v, %v", inserted, err)
	}
	check := func(stage string) {
		t.Helper()
		var target string
		if err := s.db.QueryRowContext(ctx, "SELECT target FROM probes WHERE id = 'p1'").Scan(&target); err != nil {
			t.Fatalf("%s: read probe: %v", stage, err)
		}
		if target != current.Target {
			t.Fatalf("%s: trigger snapshot overwrote the assignment projection: %q", stage, target)
		}
		triggers, err := s.PendingTriggers(ctx)
		if err != nil {
			t.Fatalf("%s: PendingTriggers: %v", stage, err)
		}
		if len(triggers) != 1 || triggers[0].Probe.Target != event.Probe.Target {
			t.Fatalf("%s: pending trigger lost its own snapshot: %+v", stage, triggers)
		}
	}
	check("record")
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	check("rebuild")
}

func TestPendingTriggerEventSeqUsesJournalRowID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	valid := testResult("run_valid", "p1", 1, now)
	skipped := valid
	skipped.RunID = "" // reserves an origin sequence number without inserting
	if _, err := s.RecordResults(ctx, []wire.Result{valid, skipped}); err != nil {
		t.Fatalf("RecordResults: %v", err)
	}
	if inserted, err := s.RecordTrigger(ctx, testTrigger("p1", "run_trigger")); err != nil || !inserted {
		t.Fatalf("RecordTrigger = %v, %v", inserted, err)
	}

	var (
		rowSeq    int64
		originSeq int64
		eventSeq  int64
	)
	if err := s.db.QueryRowContext(ctx,
		"SELECT seq, origin_seq FROM journal WHERE kind = 'trigger' AND run_id = 'run_trigger'",
	).Scan(&rowSeq, &originSeq); err != nil {
		t.Fatalf("read trigger event: %v", err)
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT event_seq FROM pending_triggers WHERE probe_id = 'p1' AND run_id = 'run_trigger'",
	).Scan(&eventSeq); err != nil {
		t.Fatalf("read pending trigger: %v", err)
	}
	if originSeq == rowSeq {
		t.Fatalf("test setup produced no sequence gap (rowid=%d, origin_seq=%d)", rowSeq, originSeq)
	}
	if eventSeq != rowSeq {
		t.Fatalf("pending trigger event_seq = %d, want journal rowid %d (origin_seq was %d)", eventSeq, rowSeq, originSeq)
	}

	// A pending trigger's journal event is never eligible for pruning,
	// however old it is.
	if _, err := s.db.ExecContext(ctx, "UPDATE journal SET at_ms = ? WHERE seq = ?", now.Add(-48*time.Hour).UnixMilli(), rowSeq); err != nil {
		t.Fatalf("age trigger event: %v", err)
	}
	if _, err := s.Maintain(ctx, MaintainOptions{Retention: time.Hour}); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	var remaining int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM journal WHERE kind = 'trigger' AND run_id = 'run_trigger'").Scan(&remaining); err != nil {
		t.Fatalf("count trigger event: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("pending trigger's journal event was pruned")
	}
}

func TestOpenMigratesPendingTriggerEventSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "radar.db")
	ctx := context.Background()
	s, err := Open(path, Options{Origin: testOrigin})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Now()
	valid := testResult("run_valid", "p1", 1, now)
	skipped := valid
	skipped.RunID = ""
	if _, err := s.RecordResults(ctx, []wire.Result{valid, skipped}); err != nil {
		t.Fatalf("RecordResults: %v", err)
	}
	if inserted, err := s.RecordTrigger(ctx, testTrigger("p1", "run_trigger")); err != nil || !inserted {
		t.Fatalf("RecordTrigger = %v, %v", inserted, err)
	}
	var rowSeq, originSeq int64
	if err := s.db.QueryRowContext(ctx,
		"SELECT seq, origin_seq FROM journal WHERE kind = 'trigger' AND run_id = 'run_trigger'",
	).Scan(&rowSeq, &originSeq); err != nil {
		t.Fatalf("read trigger event: %v", err)
	}
	if originSeq == rowSeq {
		t.Fatalf("test setup produced no sequence gap")
	}
	// Reproduce the v1 representation and version.
	if _, err := s.db.ExecContext(ctx, "UPDATE pending_triggers SET event_seq = ? WHERE probe_id = 'p1'", originSeq); err != nil {
		t.Fatalf("corrupt pending event_seq: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		t.Fatalf("downgrade user_version: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path, Options{Origin: testOrigin})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	var eventSeq int64
	if err := reopened.db.QueryRowContext(ctx,
		"SELECT event_seq FROM pending_triggers WHERE probe_id = 'p1' AND run_id = 'run_trigger'",
	).Scan(&eventSeq); err != nil {
		t.Fatalf("read migrated pending trigger: %v", err)
	}
	if eventSeq != rowSeq {
		t.Fatalf("migrated event_seq = %d, want %d", eventSeq, rowSeq)
	}
	var version int
	if err := reopened.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Fatalf("user_version = %d, want %d", version, schemaVersion)
	}
}

func TestRebuildRestoresState(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.SaveAssignment(ctx, "hash-1", []wire.ProbeSnapshot{testProbe("p1"), testProbe("p2")}); err != nil {
		t.Fatalf("SaveAssignment: %v", err)
	}
	ranAt := time.Now().Add(-2 * time.Minute).UTC()
	if _, err := s.RecordResults(ctx, []wire.Result{testResult("run_1", "p1", 1, ranAt)}); err != nil {
		t.Fatalf("RecordResults: %v", err)
	}
	pending, err := s.PendingResults(ctx, 10)
	if err != nil {
		t.Fatalf("PendingResults: %v", err)
	}
	if err := s.MarkOutcomes(ctx, []Outcome{{Seq: pending[0].Seq, Accepted: true}}); err != nil {
		t.Fatalf("MarkOutcomes: %v", err)
	}
	if _, err := s.RecordTrigger(ctx, testTrigger("p1", "run_pending")); err != nil {
		t.Fatalf("RecordTrigger: %v", err)
	}
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	hash, probes, err := s.LoadAssignment(ctx)
	if err != nil {
		t.Fatalf("LoadAssignment: %v", err)
	}
	if hash != "hash-1" || len(probes) != 2 {
		t.Fatalf("assignment = %q, %d probes", hash, len(probes))
	}
	var lastRun int64
	if err := s.db.QueryRowContext(ctx, "SELECT last_run_at_ms FROM probes WHERE id = 'p1'").Scan(&lastRun); err != nil {
		t.Fatalf("p1 last run: %v", err)
	}
	if lastRun != ranAt.UnixMilli() {
		t.Errorf("p1 last_run = %d, want %d", lastRun, ranAt.UnixMilli())
	}
	triggers, err := s.PendingTriggers(ctx)
	if err != nil {
		t.Fatalf("PendingTriggers: %v", err)
	}
	if len(triggers) != 1 || triggers[0].RunID != "run_pending" {
		t.Fatalf("pending triggers = %+v", triggers)
	}
}

func TestMaintainRetentionAndSize(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old := time.Now().Add(-60 * 24 * time.Hour).UTC()
	recent := time.Now().UTC()

	if _, err := s.RecordResults(ctx, []wire.Result{testResult("run_old", "p1", 1, old)}); err != nil {
		t.Fatalf("old result: %v", err)
	}
	pending, err := s.PendingResults(ctx, 10)
	if err != nil {
		t.Fatalf("PendingResults: %v", err)
	}
	if err := s.MarkOutcomes(ctx, []Outcome{{Seq: pending[0].Seq, Accepted: true}}); err != nil {
		t.Fatalf("MarkOutcomes: %v", err)
	}
	if _, err := s.RecordResults(ctx, []wire.Result{testResult("run_pending_old", "p1", 2, old)}); err != nil {
		t.Fatalf("old pending result: %v", err)
	}
	if _, err := s.RecordResults(ctx, []wire.Result{testResult("run_recent", "p1", 3, recent)}); err != nil {
		t.Fatalf("recent result: %v", err)
	}
	pending, err = s.PendingResults(ctx, 10)
	if err != nil {
		t.Fatalf("PendingResults: %v", err)
	}
	for _, item := range pending {
		if item.Result.RunID == "run_recent" {
			if err := s.MarkOutcomes(ctx, []Outcome{{Seq: item.Seq, Accepted: true}}); err != nil {
				t.Fatalf("mark recent: %v", err)
			}
		}
	}

	stats, err := s.Maintain(ctx, MaintainOptions{Retention: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if stats.DeletedRows < 1 {
		t.Errorf("DeletedRows = %d, want >= 1", stats.DeletedRows)
	}
	pending, err = s.PendingResults(ctx, 10)
	if err != nil {
		t.Fatalf("PendingResults after retention: %v", err)
	}
	if len(pending) != 1 || pending[0].Result.RunID != "run_pending_old" {
		t.Fatalf("pending after retention = %+v", pending)
	}
	var results int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM journal WHERE kind = 'result'").Scan(&results); err != nil {
		t.Fatalf("count results: %v", err)
	}
	if results != 2 {
		t.Errorf("results after retention = %d, want 2 (pending old + recent)", results)
	}

	bulkBatch := make([]wire.Result, 0, 400)
	for i := 0; i < 400; i++ {
		bulkBatch = append(bulkBatch, testResult(fmt.Sprintf("run_bulk_%d", i), "p1", i+10, recent))
	}
	if _, err := s.RecordResults(ctx, bulkBatch); err != nil {
		t.Fatalf("bulk results: %v", err)
	}
	bulk, err := s.PendingResults(ctx, 2000)
	if err != nil {
		t.Fatalf("bulk pending: %v", err)
	}
	for _, item := range bulk {
		if item.Result.RunID == "run_pending_old" {
			continue
		}
		if err := s.MarkOutcomes(ctx, []Outcome{{Seq: item.Seq, Accepted: true}}); err != nil {
			t.Fatalf("mark bulk: %v", err)
		}
	}
	stats, err = s.Maintain(ctx, MaintainOptions{Retention: 30 * 24 * time.Hour, MaxBytes: 128 << 10})
	if err != nil {
		t.Fatalf("size Maintain: %v", err)
	}
	pending, err = s.PendingResults(ctx, 10)
	if err != nil {
		t.Fatalf("pending after size: %v", err)
	}
	if len(pending) != 1 || pending[0].Result.RunID != "run_pending_old" {
		t.Fatalf("pending after size = %+v", pending)
	}
	if stats.OverBudget {
		t.Fatalf("unexpected over-budget result at a reachable cap: %+v", stats)
	}
	if stats.DeletedRows == 0 {
		t.Fatal("size cap did not prune any delivered results")
	}
}

func TestOriginMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "radar.db")
	s, err := Open(path, Options{Origin: testOrigin})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := Open(path, Options{Origin: "node_other"}); err == nil {
		t.Fatal("Open with a different origin should fail")
	}
}

func TestOpenRelativePath(t *testing.T) {
	t.Chdir(t.TempDir())
	local, err := Open("radar.db", Options{Origin: testOrigin})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer local.Close()
	if _, err := local.RecordResults(context.Background(), []wire.Result{testResult("run_1", "p1", 1, time.Now())}); err != nil {
		t.Fatalf("RecordResults: %v", err)
	}
}

func TestOpenPathWithSpaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state dir", "radar node.db")
	local, err := Open(path, Options{Origin: testOrigin})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer local.Close()
	if _, err := local.RecordResults(context.Background(), []wire.Result{testResult("run_1", "p1", 1, time.Now())}); err != nil {
		t.Fatalf("RecordResults: %v", err)
	}
	pending, err := local.PendingResults(context.Background(), 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %d, err = %v", len(pending), err)
	}
}

func TestOpenWithoutOriginAdoptsExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "radar.db")
	s, err := Open(path, Options{Origin: testOrigin})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	adopted, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open without origin: %v", err)
	}
	defer adopted.Close()
	if adopted.Origin() != testOrigin {
		t.Fatalf("origin = %q, want %q", adopted.Origin(), testOrigin)
	}
	if _, err := Open(filepath.Join(t.TempDir(), "new.db"), Options{}); err == nil {
		t.Fatal("creating a new database without an origin should fail")
	}
}
