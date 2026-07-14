// Package system implements a self-monitoring check: the node
// reports its own resource usage (CPU load, memory, disk, uptime)
// rather than probing anything external. It ignores probe.Options's
// Target entirely -- there is nothing to dial, the box running the
// check *is* the subject.
package system

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/mehrnet/radar-node/internal/probe"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
)

type Checker struct{}

func New() Checker { return Checker{} }

func (Checker) Type() string { return "system" }

// Check reads load/memory/disk/uptime through gopsutil, which
// abstracts each OS's native stats source (Linux /proc, macOS
// sysctl/host_statistics, Windows perf counters/WMI) behind one
// syscall-only, cgo-free API -- so this runs the same on every
// platform radar-node ships for. Target/params are unused; every
// field reported comes from the local machine regardless of what a
// job specifies.
func (c Checker) Check(ctx context.Context, opts probe.Options) probe.Result {
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

	return probe.Ok(c.Type(), opts.Target, opts.Mode, opts.Seq, time.Since(start), map[string]any{
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
	})
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
