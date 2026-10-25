package module

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/probe"
)

// A pooled job must not spend its timeout while waiting for one of the
// pool's test slots. Before the timeout moved after the semaphore, a
// large batch with a small pool-concurrency cap expired most of its
// jobs before they ever made a request.
func TestPoolTestJobTimeoutStartsAfterSlotAcquired(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	m := Module{
		Name:    "queue-timeout-test",
		Run:     &Step{Command: []string{"sh", "-c", `printf '{"latency_ms":1}\n'`}},
		Collect: Collect{Format: "writeout_json"},
	}
	c := &PoolChecker{m: m}

	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	done := make(chan probe.Result, 1)
	go func() {
		done <- c.testJob(context.Background(), sem, poolJob{opts: probe.Options{
			Target:  "queue-timeout",
			Timeout: 500 * time.Millisecond,
			Seq:     1,
		}})
	}()

	// Hold the only slot past the job's own timeout. Once released, the
	// job still has its full budget and completes immediately.
	time.Sleep(700 * time.Millisecond)
	<-sem

	select {
	case result := <-done:
		if !result.Ok || result.Error != "" {
			t.Fatalf("job failed after waiting for a slot: ok=%v error=%q", result.Ok, result.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job did not finish after the slot was released")
	}
}
