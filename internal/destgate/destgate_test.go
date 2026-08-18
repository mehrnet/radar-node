package destgate

import (
	"context"
	"testing"
	"time"
)

// resetForTest clears the shared last-attempt map and floor between
// tests -- destgate is a package-level singleton by design (see its
// own doc comment), so tests need to reset that shared state
// themselves rather than constructing a fresh instance each time.
func resetForTest(t *testing.T, floorD time.Duration) {
	t.Helper()
	mu.Lock()
	last = map[string]time.Time{}
	floor = floorD
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
