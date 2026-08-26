package agent

import (
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/wire"
)

func TestProbeCache_LastKnownHash_EmptyOnFreshCache(t *testing.T) {
	c := newProbeCache()
	// The bootstrap case: an empty hash naturally compares unequal to
	// whatever the server has compiled, so a fresh cache always
	// triggers a full snapshot on its very first heartbeat -- no
	// separate bootstrap path needed.
	if got := c.lastKnownHash(); got != "" {
		t.Fatalf("expected an empty hash on a fresh cache, got %q", got)
	}
}

func TestProbeCache_ReplaceAssignment_ThenDue(t *testing.T) {
	c := newProbeCache()
	now := time.Now()
	c.replaceAssignment("hash1", []wire.ProbeSnapshot{{
		ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "interval", IntervalSeconds: 30,
		StartsAt: now.Add(-time.Minute).UnixMilli(),
	}})

	if c.lastKnownHash() != "hash1" {
		t.Fatalf("expected lastKnownHash 'hash1', got %q", c.lastKnownHash())
	}
	due := c.dueProbes(now)
	if len(due) != 1 || due[0].ID != "probe_1" {
		t.Fatalf("expected probe_1 due, got %+v", due)
	}
}

func TestProbeCache_ManualProbe_NeverDueViaScheduler(t *testing.T) {
	c := newProbeCache()
	now := time.Now()
	c.replaceAssignment("hash1", []wire.ProbeSnapshot{{
		ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "manual", StartsAt: now.Add(-time.Minute).UnixMilli(),
	}})

	// Never due on its own -- not even once, not even before any run has
	// happened. Only an explicit "triggered" event (see
	// TestProbeCache_TriggeredEvent_QueuesPendingTrigger) makes it run.
	if due := c.dueProbes(now); len(due) != 0 {
		t.Fatalf("expected a manual probe to never be due via the scheduler, got %+v", due)
	}
	if due := c.dueProbes(now.Add(time.Hour)); len(due) != 0 {
		t.Fatalf("expected a manual probe to still never be due later, got %+v", due)
	}
}

func TestProbeCache_TriggeredEvent_QueuesPendingTrigger(t *testing.T) {
	c := newProbeCache()
	now := time.Now()
	c.replaceAssignment("hash1", []wire.ProbeSnapshot{{
		ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "manual", StartsAt: now.Add(-time.Minute).UnixMilli(),
	}})
	c.applyTriggeredEvents([]wire.Event{{Seq: 1, EventType: "triggered", RunID: "run_abc", Probe: wire.ProbeSnapshot{
		ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "manual", StartsAt: now.Add(-time.Minute).UnixMilli(),
	}}})

	triggers := c.drainPendingTriggers()
	if len(triggers) != 1 || triggers[0].ProbeID != "probe_1" || triggers[0].RunID != "run_abc" {
		t.Fatalf("expected one pending trigger for probe_1/run_abc, got %+v", triggers)
	}
	// A drain clears the queue -- draining again with nothing new
	// applied since must come back empty, not repeat the same trigger.
	if triggers := c.drainPendingTriggers(); len(triggers) != 0 {
		t.Fatalf("expected the queue to be empty after a drain, got %+v", triggers)
	}
	// A trigger is never a substitute for real due-ness -- the probe
	// itself must still never show up in dueProbes.
	if due := c.dueProbes(now); len(due) != 0 {
		t.Fatalf("expected the probe to still not be 'due' via the scheduler, got %+v", due)
	}
}

func TestProbeCache_TriggeredEvent_DoesNotChangeLastKnownHash(t *testing.T) {
	c := newProbeCache()
	now := time.Now()
	c.replaceAssignment("hash1", []wire.ProbeSnapshot{{
		ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "manual", StartsAt: now.Add(-time.Minute).UnixMilli(),
	}})
	c.applyTriggeredEvents([]wire.Event{{Seq: 1, EventType: "triggered", RunID: "run_abc", Probe: wire.ProbeSnapshot{
		ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "manual", StartsAt: now.Add(-time.Minute).UnixMilli(),
	}}})

	// Triggers are entirely independent of the hash-compare protocol --
	// this agent's own reported ProbeHash on the next heartbeat must
	// still be whatever replaceAssignment last set, not perturbed by a
	// trigger's own snapshot payload.
	if c.lastKnownHash() != "hash1" {
		t.Fatalf("expected a triggered event to leave lastKnownHash unchanged at 'hash1', got %q", c.lastKnownHash())
	}
}

// Regression for a real production incident: radar-api's own heartbeat
// handler delivers a "triggered" event on *every* heartbeat regardless
// of hash match, deleting the underlying row only once this node
// reports results under that RunID -- so the exact same RunID can
// legitimately arrive more than once before that first report lands
// (a slow probe, a delayed results flush, several heartbeats in that
// window). Without a dedup, each redelivery queued another run, all
// sharing one RunID -- which then failed outright on report (a
// (node_id, run_id, seq) uniqueness violation), taking down every
// other, unrelated result batched alongside it.
func TestProbeCache_TriggeredEvent_DuplicateRunIDNotRequeued(t *testing.T) {
	c := newProbeCache()
	now := time.Now()
	snapshot := wire.ProbeSnapshot{ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "manual", StartsAt: now.Add(-time.Minute).UnixMilli()}
	c.replaceAssignment("hash1", []wire.ProbeSnapshot{snapshot})

	// Same event, delivered on three separate (simulated) heartbeats --
	// must only ever queue once.
	for i := 0; i < 3; i++ {
		c.applyTriggeredEvents([]wire.Event{{Seq: 1, EventType: "triggered", RunID: "run_abc", Probe: snapshot}})
	}
	if triggers := c.drainPendingTriggers(); len(triggers) != 1 {
		t.Fatalf("expected exactly one queued trigger despite 3 redeliveries of the same RunID, got %+v", triggers)
	}

	// A later, genuinely different trigger on the same probe must still
	// queue normally -- the dedup is per-RunID, not "this probe already
	// ran once."
	c.applyTriggeredEvents([]wire.Event{{Seq: 2, EventType: "triggered", RunID: "run_def", Probe: snapshot}})
	if triggers := c.drainPendingTriggers(); len(triggers) != 1 || triggers[0].RunID != "run_def" {
		t.Fatalf("expected a new RunID to queue its own trigger, got %+v", triggers)
	}
}

// The same dedup must survive a standing-assignment resync landing in
// between two redeliveries of the same trigger -- exactly the real
// incident's own timeline (a hash-mismatch resync and a still-
// undelivered trigger redelivery both riding the same heartbeat
// stream). replaceAssignment must carry lastTriggerRunID forward the
// same way it already does lastRunAt.
func TestProbeCache_TriggeredEvent_DuplicateRunIDNotRequeuedAcrossResync(t *testing.T) {
	c := newProbeCache()
	now := time.Now()
	snapshot := wire.ProbeSnapshot{ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "manual", StartsAt: now.Add(-time.Minute).UnixMilli()}
	c.replaceAssignment("hash1", []wire.ProbeSnapshot{snapshot})
	c.applyTriggeredEvents([]wire.Event{{Seq: 1, EventType: "triggered", RunID: "run_abc", Probe: snapshot}})
	if triggers := c.drainPendingTriggers(); len(triggers) != 1 {
		t.Fatalf("expected the first delivery to queue, got %+v", triggers)
	}

	// An unrelated full-snapshot resync (e.g. a routine hash-mismatch
	// refresh) lands in between.
	c.replaceAssignment("hash2", []wire.ProbeSnapshot{snapshot})

	// The same RunID is redelivered afterward -- must still be
	// recognized as already-seen, not requeued.
	c.applyTriggeredEvents([]wire.Event{{Seq: 2, EventType: "triggered", RunID: "run_abc", Probe: snapshot}})
	if triggers := c.drainPendingTriggers(); len(triggers) != 0 {
		t.Fatalf("expected no requeue for an already-seen RunID surviving a resync, got %+v", triggers)
	}
}

func TestProbeCache_IntervalProbe_DueAgainAfterInterval(t *testing.T) {
	c := newProbeCache()
	now := time.Now()
	c.replaceAssignment("hash1", []wire.ProbeSnapshot{{
		ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "interval", IntervalSeconds: 30,
		StartsAt: now.Add(-time.Hour).UnixMilli(),
	}})

	c.markRun("probe_1", now)
	if due := c.dueProbes(now.Add(10 * time.Second)); len(due) != 0 {
		t.Fatalf("expected not due before the interval elapses, got %+v", due)
	}
	if due := c.dueProbes(now.Add(31 * time.Second)); len(due) != 1 {
		t.Fatalf("expected due once the interval elapses, got %+v", due)
	}
}

func TestProbeCache_InactiveStatus_NeverDue(t *testing.T) {
	c := newProbeCache()
	now := time.Now()
	c.replaceAssignment("hash1", []wire.ProbeSnapshot{{
		ID: "probe_1", Status: wire.ProbeStatusInactiveBilling, ScheduleType: "interval", IntervalSeconds: 30,
		StartsAt: now.Add(-time.Minute).UnixMilli(),
	}})
	if due := c.dueProbes(now); len(due) != 0 {
		t.Fatalf("expected an inactive_billing probe to never be due, got %+v", due)
	}
}

// The core invariant the whole hash-compare redesign depends on: a
// resync unrelated to a probe's own schedule must never reset this
// node's memory of when it last ran it, or every probe on this node
// would look simultaneously overdue the instant a hash mismatch
// resolves, causing a check-burst.
func TestProbeCache_ReplaceAssignment_PreservesLastRunAt(t *testing.T) {
	c := newProbeCache()
	now := time.Now()
	c.replaceAssignment("hash1", []wire.ProbeSnapshot{{
		ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "interval", IntervalSeconds: 30,
		StartsAt: now.Add(-time.Hour).UnixMilli(),
	}})
	c.markRun("probe_1", now)

	// A second full-snapshot replace (e.g. an unrelated probe on this
	// same node changed, triggering a hash mismatch) for the exact
	// same probe_1 content, under a new hash -- as a real recompile
	// would produce.
	c.replaceAssignment("hash2", []wire.ProbeSnapshot{{
		ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "interval", IntervalSeconds: 30,
		StartsAt: now.Add(-time.Hour).UnixMilli(),
	}})
	if due := c.dueProbes(now.Add(5 * time.Second)); len(due) != 0 {
		t.Fatalf("expected lastRunAt to survive a replaceAssignment, got due=%+v", due)
	}
	if c.lastKnownHash() != "hash2" {
		t.Fatalf("expected lastKnownHash to update to 'hash2', got %q", c.lastKnownHash())
	}
}

func TestProbeCache_ReplaceAssignment_DropsAbsentProbe(t *testing.T) {
	c := newProbeCache()
	now := time.Now()
	c.replaceAssignment("hash1", []wire.ProbeSnapshot{{
		ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "interval", IntervalSeconds: 30,
		StartsAt: now.Add(-time.Minute).UnixMilli(),
	}})
	// A snapshot that no longer includes probe_1 at all (removed,
	// paused, deactivated, ...) -- the same outcome an explicit
	// "removed" event used to produce under the old delta protocol.
	c.replaceAssignment("hash2", []wire.ProbeSnapshot{})

	if due := c.dueProbes(now); len(due) != 0 {
		t.Fatalf("expected a probe absent from the new snapshot to no longer be cached, got %+v", due)
	}
	if _, ok := c.get("probe_1"); ok {
		t.Fatalf("expected probe_1 to be gone from the cache entirely")
	}
}

func TestProbeCache_EndsAt_NotDueAfterEnd(t *testing.T) {
	c := newProbeCache()
	now := time.Now()
	c.replaceAssignment("hash1", []wire.ProbeSnapshot{{
		ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "interval", IntervalSeconds: 10,
		StartsAt: now.Add(-time.Hour).UnixMilli(), EndsAt: now.Add(-time.Minute).UnixMilli(),
	}})
	if due := c.dueProbes(now); len(due) != 0 {
		t.Fatalf("expected a probe past its ends_at to never be due, got %+v", due)
	}
}
