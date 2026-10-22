package agent

import (
	"sync"
	"time"

	"github.com/mehrnet/radar-node/internal/wire"
)

// cachedProbe is a locally-held probe definition plus this node's own
// memory of when it last ran it -- due-ness is computed entirely
// from this, no server round trip needed per decision. lastTriggerRunID
// is the dedup memory applyTriggeredEvents relies on -- see its own
// doc comment for why a single trigger's RunID can legitimately arrive
// more than once.
type cachedProbe struct {
	wire.ProbeSnapshot
	lastRunAt        time.Time
	lastTriggerRunID string
}

// pendingTrigger is one "run this probe right now" request, queued by
// applyTriggeredEvents (a "triggered" event) and drained by the
// scheduler tick alongside its own dueProbes call. RunID is server-
// issued (see routes/probes.ts's POST .../trigger), not node-generated
// the way a normal scheduled run's is -- every node executing the same
// trigger reports back under that one shared id, which is what lets
// the dashboard correlate one button click across however many nodes
// the probe is assigned to into a single table instead of N unrelated
// runs.
type pendingTrigger struct {
	ProbeID string
	RunID   string
	// Probe is the snapshot carried by the trigger itself. The probe may
	// already have been removed from the standing assignment, so the
	// trigger has to stay runnable on its own.
	Probe wire.ProbeSnapshot
}

// probeCache is the node's local understanding of which probes it's
// responsible for. As of SpecVersion 2, assignment sync is content-
// hash-compared, not delta-applied: contentHash is this node's own
// last-known hash (sent as HeartbeatRequest.ProbeHash on every
// heartbeat), and replaceAssignment is called whenever the server
// reports a mismatch, replacing the whole set at once (see agent.go's
// beat()). "triggered" events are unrelated to this and still arrive
// incrementally via HeartbeatResponse.Events on every heartbeat
// regardless of hash match. Safe for concurrent use by the scheduler
// and heartbeat loops.
type probeCache struct {
	mu              sync.Mutex
	probes          map[string]*cachedProbe
	contentHash     string
	pendingTriggers []pendingTrigger
}

func newProbeCache() *probeCache {
	return &probeCache{probes: map[string]*cachedProbe{}}
}

// lastKnownHash is empty on a fresh cache (this node's very first-ever
// heartbeat) -- an empty ProbeHash on the wire naturally compares
// unequal to whatever the server has compiled, triggering a full
// snapshot in response, the same "no separate bootstrap path needed"
// shape SinceSeq=0 used to give.
func (c *probeCache) lastKnownHash() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.contentHash
}

// replaceAssignment applies a full-snapshot response (the hash-compare
// path's mismatch case, see agent.go's beat()) -- not a merge of
// deltas the way the old applyEvents used to be. lastRunAt is still
// carried forward per probe ID for anything present in both the old
// and new sets: a probe's own due-ness timer must survive a sync
// that's unrelated to its own schedule, or every probe on this node
// would look simultaneously overdue the instant a hash mismatch
// resolves, causing a check-burst across however many probes this
// node runs. A probe absent from the new set (removed, paused,
// deactivated, ...) is simply dropped -- the same outcome an explicit
// "removed" event used to produce under the old delta protocol.
func (c *probeCache) replaceAssignment(hash string, probes []wire.ProbeSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := make(map[string]*cachedProbe, len(probes))
	for _, snapshot := range probes {
		entry := &cachedProbe{ProbeSnapshot: snapshot}
		if existing, ok := c.probes[snapshot.ID]; ok {
			entry.lastRunAt = existing.lastRunAt
			// Carried forward for the same reason lastRunAt is: a full-
			// snapshot resync is unrelated to any trigger this probe may
			// already have queued/run, and losing this would reopen the
			// exact redelivery-after-resync gap applyTriggeredEvents'
			// own dedup exists to close (see its doc comment) -- a probe
			// whose standing assignment happens to resync in the same
			// window as a still-undeleted "triggered" event redelivery
			// would otherwise re-run it.
			entry.lastTriggerRunID = existing.lastTriggerRunID
		}
		next[snapshot.ID] = entry
	}
	c.probes = next
	c.contentHash = hash
}

// cachedProbeState is one restored probe row, including the schedule
// and trigger bookkeeping that must survive a restart.
type cachedProbeState struct {
	Snapshot         wire.ProbeSnapshot
	LastRunAt        time.Time
	LastTriggerRunID string
}

func (c *probeCache) restore(hash string, states []cachedProbeState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := make(map[string]*cachedProbe, len(states))
	for _, state := range states {
		next[state.Snapshot.ID] = &cachedProbe{
			ProbeSnapshot:    state.Snapshot,
			lastRunAt:        state.LastRunAt,
			lastTriggerRunID: state.LastTriggerRunID,
		}
	}
	c.probes = next
	c.contentHash = hash
}

// applyTriggeredEvents queues a run for each "triggered" entry in
// events -- entirely independent of replaceAssignment above (see
// wire.Event's own doc comment: as of SpecVersion 2 "triggered" is the
// only event type that still arrives this way, on every heartbeat
// regardless of hash match). Each trigger also refreshes this probe's
// own cached snapshot from the event's own embedded payload, so a
// trigger fired the instant after an edit still runs the new
// definition, not a stale cached one -- but never touches lastRunAt,
// since a trigger is an independent one-off run, not part of this
// probe's normal schedule/due-ness bookkeeping.
//
// Deduped by RunID against lastTriggerRunID and the pending queue --
// radar-api delivers a "triggered" event on every heartbeat until this
// node reports results under that RunID, so the same event legitimately
// arrives more than once. Without the dedup, every redelivery queued
// another run under the same RunID, which then failed on report as a
// (node_id, run_id, seq) uniqueness violation.
func (c *probeCache) applyTriggeredEvents(events []wire.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ev := range events {
		c.applyTriggeredEventLocked(ev)
	}
}

func (c *probeCache) applyTriggeredEvent(ev wire.Event) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.applyTriggeredEventLocked(ev)
}

func (c *probeCache) applyTriggeredEventLocked(ev wire.Event) bool {
	if ev.EventType != "triggered" || ev.RunID == "" || ev.Probe.ID == "" {
		return false
	}
	if c.hasPendingTriggerLocked(ev.Probe.ID, ev.RunID) {
		return false
	}
	if existing, ok := c.probes[ev.Probe.ID]; ok {
		if existing.lastTriggerRunID == ev.RunID {
			return false
		}
		existing.ProbeSnapshot = ev.Probe
		existing.lastTriggerRunID = ev.RunID
	}
	c.pendingTriggers = append(c.pendingTriggers, pendingTrigger{
		ProbeID: ev.Probe.ID,
		RunID:   ev.RunID,
		Probe:   ev.Probe,
	})
	return true
}

// queueTriggers restores durable triggers after a restart. It updates
// snapshots for probes still in the assignment but does not add probes
// the server no longer assigned.
func (c *probeCache) queueTriggers(triggers []pendingTrigger) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	queued := 0
	for _, trigger := range triggers {
		if trigger.ProbeID == "" || trigger.RunID == "" {
			continue
		}
		if c.hasPendingTriggerLocked(trigger.ProbeID, trigger.RunID) {
			continue
		}
		if trigger.Probe.ID == "" {
			if existing, ok := c.probes[trigger.ProbeID]; ok {
				trigger.Probe = existing.ProbeSnapshot
			}
		}
		if existing, ok := c.probes[trigger.ProbeID]; ok {
			if existing.lastTriggerRunID == trigger.RunID {
				continue
			}
			if trigger.Probe.ID != "" {
				existing.ProbeSnapshot = trigger.Probe
			}
			existing.lastTriggerRunID = trigger.RunID
		}
		c.pendingTriggers = append(c.pendingTriggers, trigger)
		queued++
	}
	return queued
}

// requeueTriggers puts back a drained trigger whose results could not
// be made durable. It deliberately ignores lastTriggerRunID: that field
// is set when the trigger was queued, not when it was safely recorded.
func (c *probeCache) requeueTriggers(triggers []pendingTrigger) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, trigger := range triggers {
		if trigger.ProbeID == "" || trigger.RunID == "" {
			continue
		}
		if c.hasPendingTriggerLocked(trigger.ProbeID, trigger.RunID) {
			continue
		}
		c.pendingTriggers = append(c.pendingTriggers, trigger)
	}
}

func (c *probeCache) markTriggerComplete(probeID, runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.probes[probeID]; ok {
		existing.lastTriggerRunID = runID
	}
}

func (c *probeCache) hasPendingTriggerLocked(probeID, runID string) bool {
	for _, trigger := range c.pendingTriggers {
		if trigger.ProbeID == probeID && trigger.RunID == runID {
			return true
		}
	}
	return false
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
