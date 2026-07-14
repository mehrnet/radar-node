// Package system implements a self-monitoring check: the node
// reports its own resource usage (CPU load, memory, disk, uptime)
// rather than probing anything external. It ignores probe.Options's
// Target entirely -- there is nothing to dial, the box running the
// check *is* the subject.
package system

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mehrnet/radar-node/internal/probe"
)

type Checker struct{}

func New() Checker { return Checker{} }

func (Checker) Type() string { return "system" }

// Check reads /proc and statfs("/") -- Linux only, matching this
// project's only supported deployment targets (see Makefile's
// `cross` target). Target/params are unused; every field reported
// comes from the local machine regardless of what a job specifies.
func (c Checker) Check(_ context.Context, opts probe.Options) probe.Result {
	start := time.Now()

	load1, load5, load15, err := readLoadAvg()
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, fmt.Errorf("read loadavg: %w", err))
	}
	memTotal, memAvailable, err := readMemInfo()
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, fmt.Errorf("read meminfo: %w", err))
	}
	uptime, err := readUptime()
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, fmt.Errorf("read uptime: %w", err))
	}
	diskTotal, diskFree, err := readDiskUsage("/")
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, fmt.Errorf("read disk usage: %w", err))
	}

	memUsedPercent := 0.0
	if memTotal > 0 {
		memUsedPercent = 100 * float64(memTotal-memAvailable) / float64(memTotal)
	}
	diskUsedPercent := 0.0
	if diskTotal > 0 {
		diskUsedPercent = 100 * float64(diskTotal-diskFree) / float64(diskTotal)
	}

	return probe.Ok(c.Type(), opts.Target, opts.Mode, opts.Seq, time.Since(start), map[string]any{
		"load1":               load1,
		"load5":               load5,
		"load15":              load15,
		"mem_total_bytes":     memTotal,
		"mem_available_bytes": memAvailable,
		"mem_used_percent":    memUsedPercent,
		"disk_total_bytes":    diskTotal,
		"disk_free_bytes":     diskFree,
		"disk_used_percent":   diskUsedPercent,
		"uptime_seconds":      uptime,
	})
}

func readLoadAvg() (load1, load5, load15 float64, err error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return 0, 0, 0, fmt.Errorf("unexpected /proc/loadavg format: %q", data)
	}
	load1, err = strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, 0, 0, err
	}
	load5, err = strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0, 0, 0, err
	}
	load15, err = strconv.ParseFloat(fields[2], 64)
	if err != nil {
		return 0, 0, 0, err
	}
	return load1, load5, load15, nil
}

func readMemInfo() (totalBytes, availableBytes int64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			totalBytes = parseMemInfoKB(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			availableBytes = parseMemInfoKB(line)
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, err
	}
	return totalBytes, availableBytes, nil
}

// parseMemInfoKB parses a "Key:   12345 kB" /proc/meminfo line into
// bytes, returning 0 for anything malformed rather than erroring --
// a single unexpected line shouldn't fail the whole check.
func parseMemInfoKB(line string) int64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	kb, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return kb * 1024
}

func readUptime() (float64, error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, fmt.Errorf("unexpected /proc/uptime format: %q", data)
	}
	return strconv.ParseFloat(fields[0], 64)
}

func readDiskUsage(path string) (totalBytes, freeBytes int64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	totalBytes = int64(stat.Blocks) * int64(stat.Bsize)
	freeBytes = int64(stat.Bavail) * int64(stat.Bsize)
	return totalBytes, freeBytes, nil
}
