package agent

import (
	"sync"
	"time"

	"github.com/mehrnet/radar-node/internal/wire"
)

// cachedJob is a locally-held job definition plus this node's own
// memory of when it last ran it -- due-ness is computed entirely
// from this, no server round trip needed per decision.
type cachedJob struct {
	wire.JobSnapshot
	lastRunAt time.Time
}

// jobCache is the node's local understanding of which jobs it's
// responsible for, built and kept current by applying events from
// GET /v1/nodes/events (see eventsSyncLoop in agent.go). Safe for
// concurrent use by the scheduler and events-sync loops.
type jobCache struct {
	mu      sync.Mutex
	jobs    map[string]*cachedJob
	lastSeq int
}

func newJobCache() *jobCache {
	return &jobCache{jobs: map[string]*cachedJob{}}
}

func (c *jobCache) lastKnownSeq() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSeq
}

// applyEvents folds a batch of events into the cache in order --
// "created" and "updated" both carry the job's full current
// definition, so applying them in seq order always converges to the
// latest state regardless of how many changes happened between
// syncs. An existing job's lastRunAt is preserved across an update
// (the job changing doesn't reset this node's own due-ness memory
// for it).
func (c *jobCache) applyEvents(events []wire.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ev := range events {
		if ev.Seq > c.lastSeq {
			c.lastSeq = ev.Seq
		}
		if ev.EventType == "removed" {
			delete(c.jobs, ev.Job.ID)
			continue
		}
		entry := &cachedJob{JobSnapshot: ev.Job}
		if existing, ok := c.jobs[ev.Job.ID]; ok {
			entry.lastRunAt = existing.lastRunAt
		}
		c.jobs[ev.Job.ID] = entry
	}
}

// dueJobs returns a snapshot of every currently-active job due to
// run at `now` (already clock-corrected by the caller -- see
// clock.go), without mutating lastRunAt; that only happens once a
// run actually completes, via markRun.
func (c *jobCache) dueJobs(now time.Time) []wire.JobSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	var due []wire.JobSnapshot
	for _, job := range c.jobs {
		if job.Status != wire.JobStatusActive {
			continue
		}
		if now.Before(time.UnixMilli(job.StartsAt)) {
			continue
		}
		if job.EndsAt > 0 && !now.Before(time.UnixMilli(job.EndsAt)) {
			continue
		}

		var isDue bool
		if job.ScheduleType == "once" {
			isDue = job.lastRunAt.IsZero()
		} else {
			interval := time.Duration(job.IntervalSeconds) * time.Second
			isDue = job.lastRunAt.IsZero() || !now.Before(job.lastRunAt.Add(interval))
		}
		if isDue {
			due = append(due, job.JobSnapshot)
		}
	}
	return due
}

func (c *jobCache) markRun(jobID string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if job, ok := c.jobs[jobID]; ok {
		job.lastRunAt = at
	}
}
