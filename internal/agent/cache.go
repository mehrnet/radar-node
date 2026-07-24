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

// pendingTrigger is one "run this probe right now" request, queued by
// applyEvents (a "triggered" event) and drained by the scheduler tick
// alongside its own dueProbes call. RunID is server-issued (see
// routes/probes.ts's POST .../trigger), not node-generated the way a
// normal scheduled run's is -- every node executing the same trigger
// reports back under that one shared id, which is what lets the
// dashboard correlate one button click across however many nodes the
// probe is assigned to into a single table instead of N unrelated runs.
type pendingTrigger struct {
	ProbeID string
	RunID   string
}

// probeCache is the node's local understanding of which probes it's
// responsible for, built and kept current by applying events synced
// via POST /v1/nodes/heartbeat's since_seq/events (see heartbeatLoop
// in agent.go). Safe for concurrent use by the scheduler and
// heartbeat loops.
type probeCache struct {
	mu              sync.Mutex
	probes          map[string]*cachedProbe
	lastSeq         int
	pendingTriggers []pendingTrigger
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
// for it). "triggered" carries a fresh snapshot too (so a trigger
// fired the instant after an edit still runs the new definition, not
// a stale cached one) but never touches lastRunAt -- it queues an
// immediate one-off run instead, entirely independent of this probe's
// normal schedule/due-ness bookkeeping.
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
		if ev.EventType == "triggered" && ev.RunID != "" {
			c.pendingTriggers = append(c.pendingTriggers, pendingTrigger{ProbeID: ev.Probe.ID, RunID: ev.RunID})
		}
	}
}

// drainPendingTriggers returns and clears every trigger queued since
// the last drain -- called once per scheduler tick, alongside
// dueProbes, so a triggered run is picked up within one tick interval
// of the event arriving rather than waiting for anything schedule-
// related.
func (c *probeCache) drainPendingTriggers() []pendingTrigger {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pendingTriggers) == 0 {
		return nil
	}
	drained := c.pendingTriggers
	c.pendingTriggers = nil
	return drained
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

		// A "manual" probe is never due through this path at all -- it
		// only ever runs via an explicit trigger (see pendingTrigger/
		// drainPendingTriggers), not automatically, not even once.
		if probe.ScheduleType == "manual" {
			continue
		}
		interval := time.Duration(probe.IntervalSeconds) * time.Second
		isDue := probe.lastRunAt.IsZero() || !now.Before(probe.lastRunAt.Add(interval))
		if isDue {
			due = append(due, probe.ProbeSnapshot)
		}
	}
	return due
}

// get returns the current cached definition for probeID, if this node
// still has one -- used to resolve a drained pendingTrigger back to a
// runnable snapshot (a probe could in principle be removed between a
// trigger firing and this node's next tick draining it).
func (c *probeCache) get(probeID string) (wire.ProbeSnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.probes[probeID]
	if !ok {
		return wire.ProbeSnapshot{}, false
	}
	return entry.ProbeSnapshot, true
}

func (c *probeCache) markRun(probeID string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if probe, ok := c.probes[probeID]; ok {
		probe.lastRunAt = at
	}
}
