// Package system implements a self-monitoring check: the node
// reports its own resource usage (CPU load, memory, disk, uptime, and
// network throughput) rather than probing anything external. It
// ignores probe.Options's Target entirely -- there is nothing to
// dial, the box running the check *is* the subject.
package system

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/mehrnet/radar-node/internal/probe"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
)

// Checker is stateful, unlike every other probe.Checker in this
// package -- network throughput (and, via gopsutil's own global
// sample, CPU percent too) is a rate, not a point-in-time reading, so
// it needs the previous sample to diff against. The zero value isn't
// meant to be used directly; always construct via New(). A single
// instance is shared across every probe that uses the "system" prober
// (see action.Registry) -- there is only one machine to report on, so
// sharing counters across concurrent callers is correct, not just
// convenient. The mutex serializes the read-diff-write for both --
// net throughput's own explicit sampledAt/bytesSent/bytesRecv state
// below, and gopsutil's own package-level cpu.Percent sample, which
// isn't safe against concurrent callers on its own (see Check's own
// comment on that call).
type Checker struct {
	mu        sync.Mutex
	sampledAt time.Time
	bytesSent uint64
	bytesRecv uint64
}

func New() *Checker { return &Checker{} }

func (c *Checker) Type() string { return "system" }

// Check reads load/memory/disk/uptime/network through gopsutil, which
// abstracts each OS's native stats source (Linux /proc, macOS
// sysctl/host_statistics, Windows perf counters/WMI) behind one
// syscall-only, cgo-free API -- so this runs the same on every
// platform radar-node ships for. Target/params are unused; every
// field reported comes from the local machine regardless of what a
// probe specifies.
func (c *Checker) Check(ctx context.Context, opts probe.Options) probe.Result {
	start := time.Now()

	loadAvg, err := load.AvgWithContext(ctx)
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, fmt.Errorf("read load average: %w", err))
	}
	vmem, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, fmt.Errorf("read memory: %w", err))
	}
	uptime, err := host.UptimeWithContext(ctx)
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, fmt.Errorf("read uptime: %w", err))
	}
	usage, err := disk.UsageWithContext(ctx, diskRoot())
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, fmt.Errorf("read disk usage: %w", err))
	}

	result := map[string]any{
		"load1":               loadAvg.Load1,
		"load5":               loadAvg.Load5,
		"load15":              loadAvg.Load15,
		"mem_total_bytes":     int64(vmem.Total),
		"mem_available_bytes": int64(vmem.Available),
		"mem_used_percent":    vmem.UsedPercent,
		"disk_total_bytes":    int64(usage.Total),
		"disk_free_bytes":     int64(usage.Free),
		"disk_used_percent":   usage.UsedPercent,
		"uptime_seconds":      float64(uptime),
	}

	// Only reported once a real delta exists -- the very first check
	// after process start (or after an interface counter resets, e.g.
	// a NIC restart) has nothing to diff against, so it's omitted
	// entirely rather than reported as a misleading 0.
	if mbpsIn, mbpsOut, ok := c.netThroughputMbps(ctx); ok {
		result["net_in_mbps"] = mbpsIn
		result["net_out_mbps"] = mbpsOut
	}

	// gopsutil's own non-blocking mode (interval<=0) keeps its own
	// "last sample" state and diffs against it for us -- unlike
	// net.IOCounters, there's no cumulative-counter API to do this
	// ourselves from, and no reason to duplicate gopsutil's bookkeeping
	// when it already does this exact check. Same shape as net
	// throughput either way: nothing to diff against on the very first
	// call, so it's omitted rather than reported as 0.
	//
	// That "last sample" state is gopsutil's own package-level global,
	// not scoped to this Checker -- fine as long as calls are
	// serialized, but two Check() calls racing on it concurrently (a
	// real production case: a node with two separate "system" probes
	// on the same ~60s interval occasionally has both land in the same
	// scheduler tick) each see a near-zero elapsed window since the
	// *other* call's just-written sample. Any real CPU busy-time
	// accumulated in that near-zero window divides out to a spurious,
	// momentary 100% that isn't real load at all -- confirmed in
	// production by load1 staying flat through every observed spike,
	// and by no process ever actually showing elevated CPU at the time.
	// c.mu serializes this the same way netThroughputMbps's own state
	// is already protected, so two concurrent Check() calls from this
	// process can never race on gopsutil's shared sample here either.
	c.mu.Lock()
	pct, err := cpu.PercentWithContext(ctx, 0, false)
	c.mu.Unlock()
	if err == nil && len(pct) > 0 {
		result["cpu_percent"] = pct[0]
	}

	return probe.Ok(c.Type(), opts.Target, opts.Mode, opts.Seq, time.Since(start), result)
}

// netThroughputMbps aggregates bytes sent/received across every non-
// loopback interface and diffs against the last sample to produce a
// rate. gopsutil only ever exposes cumulative interface counters (the
// same numbers `ip -s link` or /proc/net/dev show), never a rate --
// mbps has to be derived here, not read directly.
func (c *Checker) netThroughputMbps(ctx context.Context) (mbpsIn, mbpsOut float64, ok bool) {
	counters, err := net.IOCountersWithContext(ctx, true)
	if err != nil {
		return 0, 0, false
	}
	var sent, recv uint64
	for _, ctr := range counters {
		if isLoopback(ctr.Name) {
			continue
		}
		sent += ctr.BytesSent
		recv += ctr.BytesRecv
	}
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()
	prevAt, prevSent, prevRecv := c.sampledAt, c.bytesSent, c.bytesRecv
	c.sampledAt, c.bytesSent, c.bytesRecv = now, sent, recv

	if prevAt.IsZero() || sent < prevSent || recv < prevRecv {
		return 0, 0, false
	}
	elapsed := now.Sub(prevAt).Seconds()
	if elapsed <= 0 {
		return 0, 0, false
	}
	return float64(recv-prevRecv) * 8 / elapsed / 1_000_000, float64(sent-prevSent) * 8 / elapsed / 1_000_000, true
}

func isLoopback(name string) bool {
	return strings.HasPrefix(name, "lo")
}

// diskRoot picks the filesystem to report on -- "/" everywhere except
// Windows, which has no single root filesystem and needs a drive
// letter instead.
func diskRoot() string {
	if runtime.GOOS == "windows" {
		return `C:\`
	}
	return "/"
}
