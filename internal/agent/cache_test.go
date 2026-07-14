package agent

import (
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/wire"
)

func TestJobCache_ApplyEvents_CreatedThenDue(t *testing.T) {
	c := newJobCache()
	now := time.Now()
	c.applyEvents([]wire.Event{{
		Seq:       1,
		EventType: "created",
		Job: wire.JobSnapshot{
			ID: "job_1", Status: wire.JobStatusActive, ScheduleType: "once",
			StartsAt: now.Add(-time.Minute).UnixMilli(),
		},
	}})

	if c.lastKnownSeq() != 1 {
		t.Fatalf("expected lastKnownSeq 1, got %d", c.lastKnownSeq())
	}
	due := c.dueJobs(now)
	if len(due) != 1 || due[0].ID != "job_1" {
		t.Fatalf("expected job_1 due, got %+v", due)
	}
}

func TestJobCache_OnceJob_NotDueAfterRun(t *testing.T) {
	c := newJobCache()
	now := time.Now()
	c.applyEvents([]wire.Event{{Seq: 1, EventType: "created", Job: wire.JobSnapshot{
		ID: "job_1", Status: wire.JobStatusActive, ScheduleType: "once", StartsAt: now.Add(-time.Minute).UnixMilli(),
	}}})

	c.markRun("job_1", now)
	if due := c.dueJobs(now.Add(time.Hour)); len(due) != 0 {
		t.Fatalf("expected a 'once' job to never be due again after running, got %+v", due)
	}
}

func TestJobCache_IntervalJob_DueAgainAfterInterval(t *testing.T) {
	c := newJobCache()
	now := time.Now()
	c.applyEvents([]wire.Event{{Seq: 1, EventType: "created", Job: wire.JobSnapshot{
		ID: "job_1", Status: wire.JobStatusActive, ScheduleType: "interval", IntervalSeconds: 30,
		StartsAt: now.Add(-time.Hour).UnixMilli(),
	}}})

	c.markRun("job_1", now)
	if due := c.dueJobs(now.Add(10 * time.Second)); len(due) != 0 {
		t.Fatalf("expected not due before the interval elapses, got %+v", due)
	}
	if due := c.dueJobs(now.Add(31 * time.Second)); len(due) != 1 {
		t.Fatalf("expected due once the interval elapses, got %+v", due)
	}
}

func TestJobCache_InactiveStatus_NeverDue(t *testing.T) {
	c := newJobCache()
	now := time.Now()
	c.applyEvents([]wire.Event{{Seq: 1, EventType: "created", Job: wire.JobSnapshot{
		ID: "job_1", Status: wire.JobStatusInactiveBilling, ScheduleType: "once", StartsAt: now.Add(-time.Minute).UnixMilli(),
	}}})
	if due := c.dueJobs(now); len(due) != 0 {
		t.Fatalf("expected an inactive_billing job to never be due, got %+v", due)
	}
}

func TestJobCache_UpdatedEvent_PreservesLastRunAt(t *testing.T) {
	c := newJobCache()
	now := time.Now()
	c.applyEvents([]wire.Event{{Seq: 1, EventType: "created", Job: wire.JobSnapshot{
		ID: "job_1", Status: wire.JobStatusActive, ScheduleType: "interval", IntervalSeconds: 30,
		StartsAt: now.Add(-time.Hour).UnixMilli(),
	}}})
	c.markRun("job_1", now)

	// An "updated" event (e.g. the billing cascade flipping status
	// and back) must not reset this node's own memory of when it
	// last ran the job -- otherwise a routine status update would
	// cause an immediate re-run regardless of the real interval.
	c.applyEvents([]wire.Event{{Seq: 2, EventType: "updated", Job: wire.JobSnapshot{
		ID: "job_1", Status: wire.JobStatusActive, ScheduleType: "interval", IntervalSeconds: 30,
		StartsAt: now.Add(-time.Hour).UnixMilli(),
	}}})
	if due := c.dueJobs(now.Add(5 * time.Second)); len(due) != 0 {
		t.Fatalf("expected lastRunAt to survive an update event, got due=%+v", due)
	}
}

func TestJobCache_RemovedEvent_DeletesJob(t *testing.T) {
	c := newJobCache()
	now := time.Now()
	c.applyEvents([]wire.Event{{Seq: 1, EventType: "created", Job: wire.JobSnapshot{
		ID: "job_1", Status: wire.JobStatusActive, ScheduleType: "once", StartsAt: now.Add(-time.Minute).UnixMilli(),
	}}})
	c.applyEvents([]wire.Event{{Seq: 2, EventType: "removed", Job: wire.JobSnapshot{ID: "job_1"}}})

	if due := c.dueJobs(now); len(due) != 0 {
		t.Fatalf("expected a removed job to no longer be cached, got %+v", due)
	}
}

func TestJobCache_EndsAt_NotDueAfterEnd(t *testing.T) {
	c := newJobCache()
	now := time.Now()
	c.applyEvents([]wire.Event{{Seq: 1, EventType: "created", Job: wire.JobSnapshot{
		ID: "job_1", Status: wire.JobStatusActive, ScheduleType: "interval", IntervalSeconds: 10,
		StartsAt: now.Add(-time.Hour).UnixMilli(), EndsAt: now.Add(-time.Minute).UnixMilli(),
	}}})
	if due := c.dueJobs(now); len(due) != 0 {
		t.Fatalf("expected a job past its ends_at to never be due, got %+v", due)
	}
}
