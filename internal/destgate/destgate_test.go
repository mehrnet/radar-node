package destgate

import (
	"context"
	"testing"
	"time"
)

// resetForTest clears the shared last-attempt map and configures floor
// between tests -- destgate is a package-level singleton by design
// (see its own doc comment), so tests need to reset that shared state
// themselves rather than constructing a fresh instance each time.
// maxWait stays 0 (no internal bound beyond whatever ctx a test
// itself supplies) unless a test explicitly needs otherwise -- see
// resetForTestWithMaxWait.
func resetForTest(t *testing.T, floorD time.Duration) {
	t.Helper()
	resetForTestWithMaxWait(t, floorD, 0)
}

func resetForTestWithMaxWait(t *testing.T, floorD, maxWaitD time.Duration) {
	t.Helper()
	mu.Lock()
	last = map[string]time.Time{}
	floor = floorD
	maxWait = maxWaitD
	mu.Unlock()
}

func TestWait_FirstCallForADestinationReturnsImmediately(t *testing.T) {
	resetForTest(t, time.Hour) // a floor long enough that any wait would be obviously wrong
	start := time.Now()
	if err := Wait(context.Background(), "example.com:443"); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("first call for a fresh destination should return immediately, took %v", elapsed)
	}
}

func TestWait_SecondCallForSameDestinationBlocksUntilFloorElapses(t *testing.T) {
	floorD := 150 * time.Millisecond
	resetForTest(t, floorD)

	if err := Wait(context.Background(), "shared-host:8080"); err != nil {
		t.Fatalf("first Wait: %v", err)
	}
	start := time.Now()
	if err := Wait(context.Background(), "shared-host:8080"); err != nil {
		t.Fatalf("second Wait: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < floorD-20*time.Millisecond {
		t.Fatalf("second call returned after only %v, expected to block ~%v", elapsed, floorD)
	}
}

func TestWait_DifferentDestinationsDoNotBlockEachOther(t *testing.T) {
	floorD := time.Hour
	resetForTest(t, floorD)

	if err := Wait(context.Background(), "host-a:443"); err != nil {
		t.Fatalf("Wait host-a: %v", err)
	}
	start := time.Now()
	if err := Wait(context.Background(), "host-b:443"); err != nil {
		t.Fatalf("Wait host-b: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("an unrelated destination should never wait on another's floor, took %v", elapsed)
	}
}

func TestWait_ContextCancellationDuringAWaitReturnsPromptly(t *testing.T) {
	resetForTest(t, time.Hour)
	if err := Wait(context.Background(), "busy-host:22"); err != nil {
		t.Fatalf("first Wait: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := Wait(ctx, "busy-host:22")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error once ctx's own deadline passed while still waiting")
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("ctx cancellation should cut the wait short, took %v", elapsed)
	}
}

func TestWait_DisabledOrEmptyDestinationIsANoOp(t *testing.T) {
	resetForTest(t, 0) // floor<=0 disables gating entirely
	if err := Wait(context.Background(), "any-host:443"); err != nil {
		t.Fatalf("Wait with floor=0: %v", err)
	}
	if err := Wait(context.Background(), "any-host:443"); err != nil {
		t.Fatalf("second Wait with floor=0 should also be a no-op: %v", err)
	}

	resetForTest(t, time.Hour)
	if err := Wait(context.Background(), ""); err != nil {
		t.Fatalf("Wait with an empty destination: %v", err)
	}
	if err := Wait(context.Background(), ""); err != nil {
		t.Fatalf("a second call with an empty destination should also be a no-op, not serialize against itself: %v", err)
	}
}

func TestWait_ConcurrentCallersForTheSameDestinationAreFullySerialized(t *testing.T) {
	floorD := 30 * time.Millisecond
	resetForTest(t, floorD)

	const callers = 5
	done := make(chan time.Time, callers)
	for range callers {
		go func() {
			_ = Wait(context.Background(), "hot-host:443")
			done <- time.Now()
		}()
	}

	var times []time.Time
	for range callers {
		times = append(times, <-done)
	}
	// Sort isn't imported; a simple selection sort is enough for 5 elements.
	for i := range times {
		for j := i + 1; j < len(times); j++ {
			if times[j].Before(times[i]) {
				times[i], times[j] = times[j], times[i]
			}
		}
	}
	for i := 1; i < len(times); i++ {
		gap := times[i].Sub(times[i-1])
		if gap < floorD-20*time.Millisecond {
			t.Fatalf("consecutive grants %d and %d were only %v apart, expected at least ~%v", i-1, i, gap, floorD)
		}
	}
}

// TestWait_MaxWaitGivesUpEvenWithAnUnboundedCallerContext is the
// regression this maxWait budget exists for: production fan-out (one
// account's subscription listed 33 separate "proxies" that all turned
// out to be the same physical host on different ports) meant a floor
// bounded only by each check's own few-second timeout left every
// probe but the first permanently failing, every single cycle -- not
// an occasional/transient failure. maxWait is destgate's own second,
// independent budget, enforced even when the caller's own ctx never
// expires on its own (context.Background(), standing in for the
// caller giving Wait a long-lived context and expecting Wait itself
// to bound the wait, not the caller).
func TestWait_MaxWaitGivesUpEvenWithAnUnboundedCallerContext(t *testing.T) {
	resetForTestWithMaxWait(t, time.Hour, 100*time.Millisecond) // floor far longer than maxWait
	if err := Wait(context.Background(), "oversubscribed-host:443"); err != nil {
		t.Fatalf("first Wait: %v", err)
	}

	start := time.Now()
	err := Wait(context.Background(), "oversubscribed-host:443")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error once maxWait passed while still waiting on a floor that hasn't cleared")
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("maxWait should cut the wait short even with an unbounded caller ctx, took %v", elapsed)
	}
}

// TestWait_ZeroMaxWaitLeavesBoundingEntirelyToTheCallersContext is the
// converse -- maxWait<=0 (this package's own zero value, and what
// Configure is never called with at all for the `probe` one-shot
// subcommand) must not silently impose some other bound; the only
// thing that can end an unbounded wait is the caller's own ctx.
func TestWait_ZeroMaxWaitLeavesBoundingEntirelyToTheCallersContext(t *testing.T) {
	resetForTestWithMaxWait(t, time.Hour, 0)
	if err := Wait(context.Background(), "busy-host:443"); err != nil {
		t.Fatalf("first Wait: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := Wait(ctx, "busy-host:443")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error once the caller's own ctx deadline passed")
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("the caller's own ctx should still cut the wait short with maxWait disabled, took %v", elapsed)
	}
}
