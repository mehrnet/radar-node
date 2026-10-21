package module

import (
	"testing"
	"time"
)

func TestWithMinTimeoutRaisesFloor(t *testing.T) {
	m := Module{MinTimeoutMs: 12000}
	if got := m.withMinTimeout(5 * time.Second); got != 12*time.Second {
		t.Fatalf("5s with a 12s floor = %v, want 12s", got)
	}
	if got := m.withMinTimeout(20 * time.Second); got != 20*time.Second {
		t.Fatalf("a probe timeout above the floor must win, got %v", got)
	}
	if got := (Module{}).withMinTimeout(5 * time.Second); got != 5*time.Second {
		t.Fatalf("no floor must leave the timeout alone, got %v", got)
	}
}

func TestParseMinTimeout(t *testing.T) {
	m, err := ParseBytes([]byte(`
name: floored
run:
  command: ["true"]
collect:
  format: writeout_json
min_timeout_ms: 12000
`))
	if err != nil {
		t.Fatal(err)
	}
	if m.MinTimeoutMs != 12000 {
		t.Fatalf("min_timeout_ms = %d, want 12000", m.MinTimeoutMs)
	}
}

func TestParseRejectsNegativeMinTimeout(t *testing.T) {
	_, err := ParseBytes([]byte(`
name: bad-floor
run:
  command: ["true"]
collect:
  format: writeout_json
min_timeout_ms: -1
`))
	if err == nil {
		t.Fatal("expected a negative min_timeout_ms to be rejected")
	}
}
