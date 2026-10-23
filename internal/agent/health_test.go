package agent

import (
	"fmt"
	"testing"
	"time"
)

func TestPathHealthRequiresBroadNetworkFailure(t *testing.T) {
	now := time.Now()
	p := newPathHealth()
	for i := 0; i < 5; i++ {
		p.record(fmt.Sprintf("target-%d:443", i), "dial tcp: i/o timeout", "", now)
	}
	if got := p.state(now); got != "degraded" {
		t.Fatalf("path state = %q, want degraded", got)
	}

	// A single target failing repeatedly is a target problem, not a
	// node path problem.
	one := newPathHealth()
	for i := 0; i < 50; i++ {
		one.record("one-target:443", "connection refused", "", now)
	}
	if got := one.state(now); got != "ok" {
		t.Fatalf("single-target path state = %q, want ok", got)
	}

	// HTTP status failures are target evidence, not network-path
	// evidence.
	http := newPathHealth()
	for i := 0; i < 5; i++ {
		http.record(fmt.Sprintf("target-%d:443", i), "unexpected status 500", "", now)
	}
	if got := http.state(now); got != "ok" {
		t.Fatalf("HTTP-failure path state = %q, want ok", got)
	}
}

func TestPathHealthExpiresAndIgnoresInvalidParams(t *testing.T) {
	now := time.Now()
	p := newPathHealth()
	for i := 0; i < 5; i++ {
		p.record(fmt.Sprintf("target-%d", i), "context deadline exceeded", "invalid_params", now)
	}
	if got := p.state(now); got != "ok" {
		t.Fatalf("invalid-params path state = %q, want ok", got)
	}
	for i := 0; i < 4; i++ {
		p.record(fmt.Sprintf("target-%d", i), "context deadline exceeded", "", now)
	}
	if got := p.state(now); got != "ok" {
		t.Fatalf("four-target path state = %q, want ok", got)
	}
	p.record("target-4", "context deadline exceeded", "", now)
	if got := p.state(now); got != "degraded" {
		t.Fatalf("five-target path state = %q, want degraded", got)
	}
	if got := p.state(now.Add(2 * pathHealthWindow)); got != "ok" {
		t.Fatalf("expired path state = %q, want ok", got)
	}
}

func TestHysteresisNeedsTwoSamples(t *testing.T) {
	active, streak := hysteresis(false, 0, true, false)
	if active || streak != 1 {
		t.Fatalf("first high sample = (%v,%d), want (false,1)", active, streak)
	}
	active, streak = hysteresis(active, streak, true, false)
	if !active || streak != 2 {
		t.Fatalf("second high sample = (%v,%d), want (true,2)", active, streak)
	}
	active, streak = hysteresis(active, streak, false, true)
	if !active || streak != -1 {
		t.Fatalf("first low sample = (%v,%d), want (true,-1)", active, streak)
	}
	active, streak = hysteresis(active, streak, false, true)
	if active || streak != 0 {
		t.Fatalf("second low sample = (%v,%d), want (false,0)", active, streak)
	}
}

func TestHealthSnapshotFields(t *testing.T) {
	now := time.Now()
	s := newHealthSampler()
	s.noteHeartbeat(12*time.Millisecond, true)
	health := s.snapshot(now, "ok", 7)
	if health == nil {
		t.Fatal("snapshot returned nil")
	}
	if health.CPUCount < 1 || health.Goroutines < 1 {
		t.Fatalf("unexpected CPU/goroutine sample: %+v", health)
	}
	if health.MemUsedPct < 0 || health.MemUsedPct > 100 {
		t.Fatalf("invalid memory percentage: %v", health.MemUsedPct)
	}
	if health.HeartbeatRTTMs < 11 || health.HeartbeatRTTMs > 13 {
		t.Fatalf("heartbeat RTT = %v, want ~12ms", health.HeartbeatRTTMs)
	}
	if health.DeferredJobs != 7 || health.Path != "ok" || health.UptimeSeconds < 0 {
		t.Fatalf("unexpected snapshot: %+v", health)
	}
	if health.Pressure == "" {
		t.Fatal("pressure should always be populated")
	}

	s.noteHeartbeat(0, false)
	failed := s.snapshot(now.Add(time.Second), "degraded", 0)
	if failed.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive failures = %d, want 1", failed.ConsecutiveFailures)
	}
	if failed.Path != "degraded" {
		t.Fatalf("path = %q, want degraded", failed.Path)
	}
}

func TestIsNetworkError(t *testing.T) {
	for _, text := range []string{
		"i/o timeout",
		"context deadline exceeded",
		"dial tcp 1.2.3.4:443: connection refused",
		"no such host",
		"read: connection reset by peer",
	} {
		if !isNetworkError(text, "") {
			t.Fatalf("expected %q to classify as a network error", text)
		}
	}
	for _, text := range []string{"", "unexpected status 500", "x509: certificate expired"} {
		if isNetworkError(text, "") {
			t.Fatalf("expected %q NOT to classify as a network error", text)
		}
	}
	if isNetworkError("i/o timeout", "invalid_params") {
		t.Fatal("invalid_params must never classify as a network error")
	}
}
