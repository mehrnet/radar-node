package agent

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/mehrnet/radar-node/internal/journal"
	"github.com/mehrnet/radar-node/internal/store"
	"github.com/mehrnet/radar-node/internal/wire"
)

const (
	dbMaintenanceInitialDelay = 5 * time.Second
	dbMaintenanceInterval     = time.Hour
	dbMaintenanceRetry        = 5 * time.Minute
	dbMaintenanceBudgetRetry  = 15 * time.Minute
)

// restorePendingTriggers loads the durable queue. It is retried from
// the heartbeat loop until it succeeds, so one transient local read
// failure cannot leave persisted triggers permanently stranded.
func (a *agent) restorePendingTriggers(ctx context.Context) bool {
	if a.store == nil {
		return true
	}
	triggers, err := a.store.PendingTriggers(ctx)
	if err != nil {
		if ctx.Err() == nil {
			journal.Warn("agent", "restore pending triggers failed; will retry", "err", err)
		}
		return false
	}
	if queued := a.cache.queueTriggers(agentTriggers(triggers)); queued > 0 {
		journal.Info("agent", "restored pending triggers", "triggers", queued)
	}
	a.triggersRestored.Store(true)
	return true
}

// applyTriggeredEvents persists heartbeat triggers before letting them
// reach the scheduler. If persistence fails the trigger is still run
// and reported; durability is simply degraded for that delivery.
func (a *agent) applyTriggeredEvents(ctx context.Context, events []wire.Event) {
	if a.store == nil {
		a.cache.applyTriggeredEvents(events)
		journal.Info("agent", "synced triggered events", "events", len(events))
		return
	}
	queued := 0
	for _, event := range events {
		inserted, err := a.store.RecordTrigger(ctx, event)
		if err != nil {
			journal.Error("agent", "persist trigger failed; applying in memory", "err", err)
			if a.cache.applyTriggeredEvent(event) {
				queued++
			}
			continue
		}
		if inserted && a.cache.applyTriggeredEvent(event) {
			queued++
		}
	}
	if queued > 0 {
		journal.Info("agent", "synced triggered events", "events", queued)
	}
}

func (a *agent) finishTriggers(ctx context.Context, triggers []pendingTrigger, durable bool) {
	if len(triggers) == 0 {
		return
	}
	if !durable {
		a.cache.requeueTriggers(triggers)
		journal.Warn("agent", "trigger results were not durable; requeued", "triggers", len(triggers))
		return
	}
	if a.store != nil {
		if err := a.store.CompleteTriggers(ctx, storeTriggers(triggers)); err != nil {
			journal.Error("agent", "complete triggers failed (they will replay after a restart)", "err", err)
		}
	}
	for _, trigger := range triggers {
		a.cache.markTriggerComplete(trigger.ProbeID, trigger.RunID)
	}
}

func (a *agent) drainDelivery(ctx context.Context) (delivered, accepted, rejected int, err error) {
	if a.store != nil {
		return a.drainStore(ctx)
	}
	return a.drainSpool(ctx)
}

// drainStore posts pending rows oldest-first until the queue is empty,
// the drain budget expires, or a POST fails. Delivery state is settled
// per result from the server's own acks.
func (a *agent) drainStore(ctx context.Context) (delivered, accepted, rejected int, err error) {
	if a.store == nil {
		return 0, 0, 0, nil
	}
	if !a.deliveryNextAttempt.IsZero() && time.Now().Before(a.deliveryNextAttempt) {
		return 0, 0, 0, nil
	}
	drainCtx, cancel := context.WithTimeout(ctx, deliveryDrainBudget)
	defer cancel()
	for drainCtx.Err() == nil {
		batch, err := a.store.PendingResults(drainCtx, 500)
		if err != nil {
			a.backoffDelivery()
			return delivered, accepted, rejected, err
		}
		if len(batch) == 0 {
			break
		}
		seqs := make([]int64, len(batch))
		results := make([]wire.Result, len(batch))
		for i, item := range batch {
			seqs[i] = item.Seq
			results[i] = item.Result
		}
		if err := a.store.MarkAttempts(drainCtx, seqs); err != nil {
			journal.Warn("agent", "could not record delivery attempts", "err", err)
		}
		resp, postErr := a.postResults(drainCtx, results, "")
		if postErr != nil {
			a.backoffDelivery()
			return delivered, accepted, rejected, postErr
		}
		if resp == nil {
			a.backoffDelivery()
			return delivered, accepted, rejected, errors.New("radar-api returned an empty results response")
		}
		if err := a.store.MarkOutcomes(drainCtx, outcomesFor(batch, resp)); err != nil {
			// The POST succeeded but the local DB could not settle it;
			// back off so a broken DB doesn't trigger a duplicate POST
			// every scheduler tick.
			a.backoffDelivery()
			return delivered, accepted, rejected, err
		}
		delivered += len(batch)
		accepted += resp.Accepted
		rejected += resp.Rejected
		a.clearDeliveryBackoff()
		if len(batch) < 500 {
			break
		}
	}
	return delivered, accepted, rejected, nil
}

func outcomesFor(batch []store.PendingResult, resp *wire.ResultsResponse) []store.Outcome {
	outcomes := make([]store.Outcome, 0, len(batch))
	if resp == nil {
		for _, item := range batch {
			outcomes = append(outcomes, store.Outcome{Seq: item.Seq, Accepted: true})
		}
		return outcomes
	}
	acks := make(map[string]wire.ResultAck, len(resp.Results))
	for _, ack := range resp.Results {
		acks[ackKey(ack.RunID, ack.Seq)] = ack
	}
	for _, item := range batch {
		ack, ok := acks[ackKey(item.Result.RunID, item.Result.Seq)]
		if !ok {
			// A 2xx response without a matching ack should not leave a
			// result retrying forever; the server deduplicates anyway.
			outcomes = append(outcomes, store.Outcome{Seq: item.Seq, Accepted: true})
			continue
		}
		outcomes = append(outcomes, store.Outcome{Seq: item.Seq, Accepted: ack.Accepted, Reason: ack.Reason})
	}
	return outcomes
}

func ackKey(runID string, seq int) string {
	return runID + "\x00" + strconv.Itoa(seq)
}

// maintenanceLoop keeps retention and rollups off the scheduler
// goroutine: a large vacuum must never delay probe execution. The
// store's single connection still serializes the actual writes.
func (a *agent) maintenanceLoop(ctx context.Context) {
	if a.store == nil {
		return
	}
	timer := time.NewTimer(dbMaintenanceInitialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		stats, err := a.store.Maintain(ctx, store.MaintainOptions{Retention: a.dbRetention, MaxBytes: a.dbMaxBytes})
		next := dbMaintenanceInterval
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			journal.Error("agent", "local store maintenance failed", "err", err)
			next = dbMaintenanceRetry
		} else {
			if stats.DeletedRows > 0 || stats.DeletedRollups > 0 || stats.RolledUpResults > 0 || stats.VacuumedPages > 0 || stats.OverBudget {
				journal.Info("agent", "local store maintenance",
					"deleted_rows", stats.DeletedRows, "deleted_rollups", stats.DeletedRollups,
					"rolled_up_results", stats.RolledUpResults, "vacuumed_pages", stats.VacuumedPages,
					"live_bytes", stats.LiveBytes, "db_bytes", stats.DBBytes, "wal_bytes", stats.WALBytes,
					"over_budget", stats.OverBudget)
			}
			if stats.OverBudget {
				journal.Warn("agent", "local store is over its byte budget; pending results are never pruned", "live_bytes", stats.LiveBytes, "max_bytes", a.dbMaxBytes)
				next = dbMaintenanceBudgetRetry
			}
		}
		timer.Reset(next)
	}
}

func (a *agent) backoffDelivery() {
	a.deliveryBackoff = nextDeliveryBackoff(a.deliveryBackoff)
	a.deliveryNextAttempt = time.Now().Add(a.deliveryBackoff)
}

func (a *agent) clearDeliveryBackoff() {
	a.deliveryBackoff = 0
	a.deliveryNextAttempt = time.Time{}
}

func cachedStates(states []store.ProbeState) []cachedProbeState {
	out := make([]cachedProbeState, len(states))
	for i, state := range states {
		out[i] = cachedProbeState{
			Snapshot:         state.Snapshot,
			LastRunAt:        state.LastRunAt,
			LastTriggerRunID: state.LastTriggerRunID,
		}
	}
	return out
}

func agentTriggers(triggers []store.PendingTrigger) []pendingTrigger {
	out := make([]pendingTrigger, len(triggers))
	for i, trigger := range triggers {
		out[i] = pendingTrigger{ProbeID: trigger.ProbeID, RunID: trigger.RunID, Probe: trigger.Probe}
	}
	return out
}

func storeTriggers(triggers []pendingTrigger) []store.PendingTrigger {
	out := make([]store.PendingTrigger, len(triggers))
	for i, trigger := range triggers {
		out[i] = store.PendingTrigger{ProbeID: trigger.ProbeID, RunID: trigger.RunID, Probe: trigger.Probe}
	}
	return out
}
