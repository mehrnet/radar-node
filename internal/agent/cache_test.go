package agent

import (
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/wire"
)

func TestProbeCache_ApplyEvents_CreatedThenDue(t *testing.T) {
	c := newProbeCache()
	now := time.Now()
	c.applyEvents([]wire.Event{{
		Seq:       1,
		EventType: "created",
		Probe: wire.ProbeSnapshot{
			ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "interval", IntervalSeconds: 30,
			StartsAt: now.Add(-time.Minute).UnixMilli(),
		},
	}})

	if c.lastKnownSeq() != 1 {
		t.Fatalf("expected lastKnownSeq 1, got %d", c.lastKnownSeq())
	}
	due := c.dueProbes(now)
	if len(due) != 1 || due[0].ID != "probe_1" {
		t.Fatalf("expected probe_1 due, got %+v", due)
	}
}

func TestProbeCache_ManualProbe_NeverDueViaScheduler(t *testing.T) {
	c := newProbeCache()
	now := time.Now()
	c.applyEvents([]wire.Event{{Seq: 1, EventType: "created", Probe: wire.ProbeSnapshot{
		ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "manual", StartsAt: now.Add(-time.Minute).UnixMilli(),
	}}})

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
	c.applyEvents([]wire.Event{{Seq: 1, EventType: "created", Probe: wire.ProbeSnapshot{
		ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "manual", StartsAt: now.Add(-time.Minute).UnixMilli(),
	}}})
	c.applyEvents([]wire.Event{{Seq: 2, EventType: "triggered", RunID: "run_abc", Probe: wire.ProbeSnapshot{
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

func TestProbeCache_IntervalProbe_DueAgainAfterInterval(t *testing.T) {
	c := newProbeCache()
	now := time.Now()
	c.applyEvents([]wire.Event{{Seq: 1, EventType: "created", Probe: wire.ProbeSnapshot{
		ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "interval", IntervalSeconds: 30,
		StartsAt: now.Add(-time.Hour).UnixMilli(),
	}}})

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
	c.applyEvents([]wire.Event{{Seq: 1, EventType: "created", Probe: wire.ProbeSnapshot{
		ID: "probe_1", Status: wire.ProbeStatusInactiveBilling, ScheduleType: "interval", IntervalSeconds: 30,
		StartsAt: now.Add(-time.Minute).UnixMilli(),
	}}})
	if due := c.dueProbes(now); len(due) != 0 {
		t.Fatalf("expected an inactive_billing probe to never be due, got %+v", due)
	}
}

func TestProbeCache_UpdatedEvent_PreservesLastRunAt(t *testing.T) {
	c := newProbeCache()
	now := time.Now()
	c.applyEvents([]wire.Event{{Seq: 1, EventType: "created", Probe: wire.ProbeSnapshot{
		ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "interval", IntervalSeconds: 30,
		StartsAt: now.Add(-time.Hour).UnixMilli(),
	}}})
	c.markRun("probe_1", now)

	// An "updated" event (e.g. the billing cascade flipping status
	// and back) must not reset this node's own memory of when it
	// last ran the probe -- otherwise a routine status update would
	// cause an immediate re-run regardless of the real interval.
	c.applyEvents([]wire.Event{{Seq: 2, EventType: "updated", Probe: wire.ProbeSnapshot{
		ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "interval", IntervalSeconds: 30,
		StartsAt: now.Add(-time.Hour).UnixMilli(),
	}}})
	if due := c.dueProbes(now.Add(5 * time.Second)); len(due) != 0 {
		t.Fatalf("expected lastRunAt to survive an update event, got due=%+v", due)
	}
}

func TestProbeCache_RemovedEvent_DeletesProbe(t *testing.T) {
	c := newProbeCache()
	now := time.Now()
	c.applyEvents([]wire.Event{{Seq: 1, EventType: "created", Probe: wire.ProbeSnapshot{
		ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "interval", IntervalSeconds: 30,
		StartsAt: now.Add(-time.Minute).UnixMilli(),
	}}})
	c.applyEvents([]wire.Event{{Seq: 2, EventType: "removed", Probe: wire.ProbeSnapshot{ID: "probe_1"}}})

	if due := c.dueProbes(now); len(due) != 0 {
		t.Fatalf("expected a removed probe to no longer be cached, got %+v", due)
	}
}

func TestProbeCache_EndsAt_NotDueAfterEnd(t *testing.T) {
	c := newProbeCache()
	now := time.Now()
	c.applyEvents([]wire.Event{{Seq: 1, EventType: "created", Probe: wire.ProbeSnapshot{
		ID: "probe_1", Status: wire.ProbeStatusActive, ScheduleType: "interval", IntervalSeconds: 10,
		StartsAt: now.Add(-time.Hour).UnixMilli(), EndsAt: now.Add(-time.Minute).UnixMilli(),
	}}})
	if due := c.dueProbes(now); len(due) != 0 {
		t.Fatalf("expected a probe past its ends_at to never be due, got %+v", due)
	}
}
