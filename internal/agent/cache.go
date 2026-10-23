package agent

import (
	"hash/fnv"
	"sort"
	"sync"
	"time"

	"github.com/mehrnet/radar-node/internal/journal"
	"github.com/mehrnet/radar-node/internal/wire"
)

const maxPendingTriggers = 256

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
	c.replaceAssignmentLocked(probes)
	c.contentHash = hash
}

// replaceAssignmentKeepingHash applies a new assignment to the live
// cache without committing its hash. Used when persisting the hash to
// the local store failed: the next heartbeat still advertises the old
// hash, so the server sends the full assignment again and persistence
// gets another chance instead of the database silently going stale.
func (c *probeCache) replaceAssignmentKeepingHash(probes []wire.ProbeSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.replaceAssignmentLocked(probes)
}

func (c *probeCache) replaceAssignmentLocked(probes []wire.ProbeSnapshot) {
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
// regardless of hash match). The queued run carries the event's own
// embedded snapshot, so a trigger fired the instant after an edit runs
// the new definition. It deliberately does not overwrite the probe's
// cached scheduled definition: a redelivered or restored trigger can
// be older than the current assignment, and a matching assignment hash
// means the server will not resend the correct snapshot to repair it.
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
		existing.lastTriggerRunID = ev.RunID
	}
	if len(c.pendingTriggers) >= maxPendingTriggers {
		journal.Warn("cache", "pending trigger queue full; deferring to durable replay", "probe_id", ev.Probe.ID, "run_id", ev.RunID)
		return false
	}
	c.pendingTriggers = append(c.pendingTriggers, pendingTrigger{
		ProbeID: ev.Probe.ID,
		RunID:   ev.RunID,
		Probe:   ev.Probe,
	})
	return true
}

// queueTriggers restores durable triggers after a restart. Each keeps
// its own snapshot and never overwrites the live scheduled definition;
// probes the server no longer assigned are not added back.
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
			existing.lastTriggerRunID = trigger.RunID
		}
		if len(c.pendingTriggers) >= maxPendingTriggers {
			journal.Warn("cache", "pending trigger queue full; skipping restored trigger", "probe_id", trigger.ProbeID, "run_id", trigger.RunID)
			break
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
		if len(c.pendingTriggers) >= maxPendingTriggers {
			journal.Warn("cache", "pending trigger queue full; dropping requeued trigger", "probe_id", trigger.ProbeID, "run_id", trigger.RunID)
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
// the last drain. Kept for tests and callers that explicitly want the
// whole queue; the scheduler uses the limited variant below so a
// trigger flood cannot bypass its job budget.
func (c *probeCache) drainPendingTriggers() []pendingTrigger {
	return c.drainPendingTriggersLimited(0)
}

// drainPendingTriggersLimited returns at most limit triggers, leaving
// the rest queued for the next scheduler tick. limit <= 0 drains all.
func (c *probeCache) drainPendingTriggersLimited(limit int) []pendingTrigger {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pendingTriggers) == 0 {
		return nil
	}
	if limit <= 0 || limit >= len(c.pendingTriggers) {
		drained := c.pendingTriggers
		c.pendingTriggers = nil
		return drained
	}
	drained := append([]pendingTrigger(nil), c.pendingTriggers[:limit]...)
	c.pendingTriggers = c.pendingTriggers[limit:]
	return drained
}

func (c *probeCache) pendingTriggerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pendingTriggers)
}

func (c *probeCache) probeDueLocked(probe *cachedProbe, now time.Time) bool {
	if probe.Status != wire.ProbeStatusActive {
		return false
	}
	if now.Before(time.UnixMilli(probe.StartsAt)) {
		return false
	}
	if probe.EndsAt > 0 && !now.Before(time.UnixMilli(probe.EndsAt)) {
		return false
	}
	// A "manual" probe is never due through this path at all -- it only
	// ever runs via an explicit trigger (see pendingTrigger/
	// drainPendingTriggers), not automatically, not even once.
	if probe.ScheduleType == "manual" {
		return false
	}
	interval := time.Duration(probe.IntervalSeconds) * time.Second
	return probe.lastRunAt.IsZero() || !now.Before(probe.lastRunAt.Add(interval))
}

// dueProbes returns every currently-due probe without mutating state.
func (c *probeCache) dueProbes(now time.Time) []wire.ProbeSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	var due []wire.ProbeSnapshot
	for _, probe := range c.probes {
		if c.probeDueLocked(probe, now) {
			due = append(due, probe.ProbeSnapshot)
		}
	}
	return due
}

// takeDueProbes selects a fairness-ordered, job-bounded subset of due
// probes and marks only those run. Probes left out stay due, so a huge
// assignment is spread over future ticks instead of being marked run
// and silently skipped. maxJobs<=0 means no budget (select nothing);
// maxPerProbe<=0 means no per-probe cap.
func (c *probeCache) takeDueProbes(now time.Time, maxJobs, maxPerProbe int) ([]wire.ProbeSnapshot, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	type dueEntry struct {
		probe *cachedProbe
		count int
	}
	entries := make([]dueEntry, 0, len(c.probes))
	for _, probe := range c.probes {
		if !c.probeDueLocked(probe, now) {
			continue
		}
		count := probe.ProbeCount
		if count < 1 {
			count = 1
		}
		if maxPerProbe > 0 && count > maxPerProbe {
			count = maxPerProbe
		}
		if maxJobs > 0 && count > maxJobs {
			count = maxJobs
		}
		entries = append(entries, dueEntry{probe: probe, count: count})
	}
	if maxJobs <= 0 {
		deferred := 0
		for _, entry := range entries {
			deferred += entry.count
		}
		return nil, deferred
	}
	// Oldest due first, then a stable ID tie-break -- probes repeatedly
	// deferred because they were newest cannot starve forever.
	sort.Slice(entries, func(i, j int) bool {
		left, right := entries[i].probe.lastRunAt, entries[j].probe.lastRunAt
		if left.Equal(right) {
			return entries[i].probe.ID < entries[j].probe.ID
		}
		return left.Before(right)
	})
	selected := make([]wire.ProbeSnapshot, 0, len(entries))
	deferred := 0
	used := 0
	for _, entry := range entries {
		if maxJobs > 0 && used+entry.count > maxJobs {
			deferred += entry.count
			continue
		}
		used += entry.count
		entry.probe.lastRunAt = now
		snapshot := entry.probe.ProbeSnapshot
		snapshot.ProbeCount = entry.count
		selected = append(selected, snapshot)
	}
	return selected, deferred
}

// staggerUnstarted spreads brand-new probes over a short window instead
// of firing the entire first assignment on one tick. Existing probes
// (including restored ones with a real last-run time) are untouched.
func (c *probeCache) staggerUnstarted(now time.Time, jitter time.Duration) {
	if jitter <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, probe := range c.probes {
		if !probe.lastRunAt.IsZero() || probe.ScheduleType == "manual" {
			continue
		}
		interval := time.Duration(probe.IntervalSeconds) * time.Second
		if interval <= 0 {
			continue
		}
		maxOffset := jitter
		if interval < maxOffset {
			maxOffset = interval
		}
		if maxOffset <= 0 {
			continue
		}
		h := fnv.New64a()
		_, _ = h.Write([]byte(id))
		offset := time.Duration(h.Sum64() % uint64(maxOffset))
		probe.lastRunAt = now.Add(-interval + offset)
	}
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
