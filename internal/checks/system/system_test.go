package system_test

import (
	"context"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/checks/system"
	"github.com/mehrnet/radar-node/internal/probe"
)

func TestCheck_IgnoresTargetAndReturnsStats(t *testing.T) {
	c := system.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  "this-is-never-dialed",
		Timeout: time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if res.Type != "system" {
		t.Fatalf("unexpected Type(): %s", res.Type)
	}
	for _, key := range []string{"load1", "load5", "load15", "mem_total_bytes", "mem_available_bytes", "mem_used_percent", "disk_total_bytes", "disk_free_bytes", "disk_used_percent", "uptime_seconds"} {
		if _, ok := res.Extra[key]; !ok {
			t.Errorf("expected Extra to contain %q, got %+v", key, res.Extra)
		}
	}
	if memTotal, _ := res.Extra["mem_total_bytes"].(int64); memTotal <= 0 {
		t.Errorf("expected a positive mem_total_bytes, got %v", res.Extra["mem_total_bytes"])
	}
}

func TestCheck_NetThroughputAppearsOnlyOnceAPreviousSampleExists(t *testing.T) {
	c := system.New()
	opts := probe.Options{Target: "this-is-never-dialed", Timeout: time.Second, Mode: probe.ModeWarm}

	first := c.Check(context.Background(), func() probe.Options { o := opts; o.Seq = 1; return o }())
	if !first.Ok {
		t.Fatalf("expected ok, got error %q", first.Error)
	}
	if _, ok := first.Extra["net_in_mbps"]; ok {
		t.Errorf("expected no net_in_mbps on the very first sample (nothing to diff against), got %+v", first.Extra)
	}
	if _, ok := first.Extra["net_out_mbps"]; ok {
		t.Errorf("expected no net_out_mbps on the very first sample, got %+v", first.Extra)
	}

	second := c.Check(context.Background(), func() probe.Options { o := opts; o.Seq = 2; return o }())
	if !second.Ok {
		t.Fatalf("expected ok, got error %q", second.Error)
	}
	if _, ok := second.Extra["net_in_mbps"]; !ok {
		t.Errorf("expected net_in_mbps once a previous sample exists, got %+v", second.Extra)
	}
	if _, ok := second.Extra["net_out_mbps"]; !ok {
		t.Errorf("expected net_out_mbps once a previous sample exists, got %+v", second.Extra)
	}
}

func TestCheck_CPUPercentEventuallyAppears(t *testing.T) {
	// gopsutil's cpu.Percent keeps its own package-level "last sample"
	// state (not per-Checker, unlike net throughput above), so whether
	// it's present on any *particular* call depends on whatever else
	// in this test binary called it first -- not asserted here. What's
	// always true regardless of ordering is that it shows up once
	// there's been a real previous sample to diff against.
	c := system.New()
	opts := probe.Options{Target: "this-is-never-dialed", Timeout: time.Second, Mode: probe.ModeWarm}
	c.Check(context.Background(), func() probe.Options { o := opts; o.Seq = 1; return o }())
	time.Sleep(10 * time.Millisecond)
	res := c.Check(context.Background(), func() probe.Options { o := opts; o.Seq = 2; return o }())
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if _, ok := res.Extra["cpu_percent"]; !ok {
		t.Errorf("expected cpu_percent once gopsutil has a previous sample to diff against, got %+v", res.Extra)
	}
}
