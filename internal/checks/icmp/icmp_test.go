package icmp_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/checks/icmp"
	"github.com/mehrnet/radar-node/internal/probe"
)

// These tests need an unprivileged ICMP socket to be permitted
// (net.ipv4.ping_group_range covering this process's group, which
// is the common case in containers/CI as well as on Linux desktops
// with no extra config). If the socket can't be opened at all, skip
// rather than fail, since that's an environment property, not a
// bug in the checker.
func TestCheck_Loopback(t *testing.T) {
	c := icmp.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  "127.0.0.1",
		Timeout: 2 * time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
	})
	if !res.Ok {
		if isPermissionErr(res.Error) {
			t.Skipf("unprivileged ICMP not permitted in this environment: %s", res.Error)
		}
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if res.LatencyMs == nil {
		t.Fatal("expected latency_ms to be set")
	}
}

func TestCheck_UnresolvableHost(t *testing.T) {
	c := icmp.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  "this-host-should-not-resolve.invalid",
		Timeout: time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
	})
	if res.Ok {
		t.Fatal("expected failure resolving an invalid host")
	}
}

func isPermissionErr(msg string) bool {
	return strings.Contains(msg, "permission denied") || strings.Contains(msg, "operation not permitted")
}
