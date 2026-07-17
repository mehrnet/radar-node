package agent

import (
	"sync"
	"time"

	"github.com/mehrnet/radar-node/internal/wire"
)

// cachedProbe is a locally-held probe definition plus this node's own
// memory of when it last ran it -- due-ness is computed entirely
// from this, no server round trip needed per decision.
type cachedProbe struct {
	wire.ProbeSnapshot
	lastRunAt time.Time
}

// probeCache is the node's local understanding of which probes it's
// responsible for, built and kept current by applying events synced
// via POST /v1/nodes/heartbeat's since_seq/events (see heartbeatLoop
// in agent.go). Safe for concurrent use by the scheduler and
// heartbeat loops.
type probeCache struct {
	mu      sync.Mutex
	probes  map[string]*cachedProbe
	lastSeq int
}

func newProbeCache() *probeCache {
	return &probeCache{probes: map[string]*cachedProbe{}}
}

func (c *probeCache) lastKnownSeq() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSeq
}

// applyEvents folds a batch of events into the cache in order --
// "created" and "updated" both carry the probe's full current
// definition, so applying them in seq order always converges to the
// latest state regardless of how many changes happened between
// syncs. An existing probe's lastRunAt is preserved across an update
// (the probe changing doesn't reset this node's own due-ness memory
// for it).
func (c *probeCache) applyEvents(events []wire.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ev := range events {
		if ev.Seq > c.lastSeq {
			c.lastSeq = ev.Seq
		}
		if ev.EventType == "removed" {
			delete(c.probes, ev.Probe.ID)
			continue
		}
		entry := &cachedProbe{ProbeSnapshot: ev.Probe}
		if existing, ok := c.probes[ev.Probe.ID]; ok {
			entry.lastRunAt = existing.lastRunAt
		}
		c.probes[ev.Probe.ID] = entry
	}
}

// dueProbes returns a snapshot of every currently-active probe due to
// run at `now` (already clock-corrected by the caller -- see
// clock.go), without mutating lastRunAt; that only happens once a
// run actually completes, via markRun.
func (c *probeCache) dueProbes(now time.Time) []wire.ProbeSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	var due []wire.ProbeSnapshot
	for _, probe := range c.probes {
		if probe.Status != wire.ProbeStatusActive {
			continue
		}
		if now.Before(time.UnixMilli(probe.StartsAt)) {
			continue
		}
		if probe.EndsAt > 0 && !now.Before(time.UnixMilli(probe.EndsAt)) {
			continue
		}

		var isDue bool
		if probe.ScheduleType == "once" {
			isDue = probe.lastRunAt.IsZero()
		} else {
			interval := time.Duration(probe.IntervalSeconds) * time.Second
			isDue = probe.lastRunAt.IsZero() || !now.Before(probe.lastRunAt.Add(interval))
		}
		if isDue {
			due = append(due, probe.ProbeSnapshot)
		}
	}
	return due
}

func (c *probeCache) markRun(probeID string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if probe, ok := c.probes[probeID]; ok {
		probe.lastRunAt = at
	}
}
