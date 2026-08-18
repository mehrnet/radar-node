// Package destgate enforces a node-wide minimum spacing between
// connection attempts aimed at the same real-world destination,
// regardless of which probe, group, subscription, or account asked
// for the check. A subscription's own discovered proxies routinely
// fan out to the same handful of physical third-party hosts under
// many different probe names/ports -- confirmed in production: one
// shared VPS received on the order of 100+ connection attempts/minute
// from this node's own fleet alone, indistinguishable from a port
// scan to that host's own abuse defenses, and very likely degrading
// the accuracy of the very checks meant to measure it. This package
// is the one place that throttles regardless of prober, overriding a
// probe's own configured interval when its destination is shared and
// busy.
//
// A package-level singleton, not an injected instance -- there is and
// only ever will be one of these per process (both cmd/radar-node's
// `probe` and `agent` subcommands construct exactly one registry
// each), and internal/module (which needs to call this from both the
// single-check and pooled-check dispatch paths) cannot import
// internal/agent to receive one via constructor injection. See
// internal/portalloc's own doc comment for the same reasoning applied
// to port allocation.
package destgate

import (
	"context"
	"sync"
	"time"
)

var (
	mu      sync.Mutex
	last    = map[string]time.Time{}
	floor   time.Duration
	maxWait time.Duration
)

// Configure sets the minimum spacing between two granted Wait calls
// for the same destination (floor), and the longest Wait will ever
// block one caller before giving up (maxWait). Call once, before
// either the scheduler or pool loops start; floor<=0 disables gating
// entirely, so callers that never configure this (the `probe`
// one-shot subcommand, which has no fleet-spamming risk to guard
// against) pay nothing.
//
// maxWait is deliberately its own, separate budget from a check's own
// configured timeout_ms -- confirmed against real production fan-out
// (one account's subscription listed 33 separate "proxies" that all
// turned out to be the same physical host on different ports, all due
// on the same schedule): with a 10s floor and typical few-second check
// timeouts, only the very first of many probes sharing one destination
// could ever get a real attempt in per floor window, and every other
// one would fail waiting, every single cycle, forever -- not a
// transient/occasional failure but a permanent one, indistinguishable
// from the destination actually being down. Once Wait's own budget
// clears, the caller gives the check itself a full, fresh
// timeout_ms-scoped budget to actually run in -- see agent.runCheck's
// own comment.
func Configure(floorD, maxWaitD time.Duration) {
	mu.Lock()
	defer mu.Unlock()
	floor = floorD
	maxWait = maxWaitD
}

// Wait blocks until at least the configured floor has elapsed since
// the last granted call for dest, then records this call's own time
// and returns. Returns immediately (no bookkeeping) if dest is empty
// or gating is disabled -- an empty destination means the caller
// couldn't determine one, and silently degrading to "no throttling"
// is safer than accidentally serializing every such call against
// itself under one bogus shared key.
//
// Bounded by the lesser of ctx's own deadline and the configured
// maxWait -- a destination still busy past that point returns an
// error rather than blocking indefinitely, so one oversubscribed
// destination can never hang a caller (or, transitively, whatever
// concurrency-limited dispatch loop is waiting on that caller's own
// goroutine) forever. Callers must acquire any concurrency-limiting
// slot *after* Wait returns, never before calling it -- otherwise
// every check queued behind one busy destination sits there holding a
// slot, starving unrelated checks against completely different
// destinations, reintroducing the exact class of problem this package
// exists to prevent.
func Wait(ctx context.Context, dest string) error {
	mu.Lock()
	if maxWait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, maxWait)
		defer cancel()
	}
	mu.Unlock()

	for {
		mu.Lock()
		if dest == "" || floor <= 0 {
			mu.Unlock()
			return nil
		}
		now := time.Now()
		prev, ok := last[dest]
		if !ok || now.Sub(prev) >= floor {
			last[dest] = now
			mu.Unlock()
			return nil
		}
		remaining := floor - now.Sub(prev)
		mu.Unlock()

		t := time.NewTimer(remaining)
		select {
		case <-t.C:
			// Recheck rather than assume our turn -- another goroutine
			// may have taken the slot while this one was waiting.
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
	}
}
