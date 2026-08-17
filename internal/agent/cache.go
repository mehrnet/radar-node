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
		}
		next[snapshot.ID] = entry
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
func (c *probeCache) applyTriggeredEvents(events []wire.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ev := range events {
		if ev.EventType != "triggered" || ev.RunID == "" {
			continue
		}
		entry := &cachedProbe{ProbeSnapshot: ev.Probe}
		if existing, ok := c.probes[ev.Probe.ID]; ok {
			entry.lastRunAt = existing.lastRunAt
		}
		c.probes[ev.Probe.ID] = entry
		c.pendingTriggers = append(c.pendingTriggers, pendingTrigger{ProbeID: ev.Probe.ID, RunID: ev.RunID})
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
