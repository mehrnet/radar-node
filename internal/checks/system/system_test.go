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
