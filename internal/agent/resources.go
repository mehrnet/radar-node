package agent

import (
	"os"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/mem"
)

// availableMemoryBytes tries the process's own cgroup first (containers
// and memory-limited services), then MemAvailable, then a cross-platform
// fallback. The scheduler uses this only for defaults; explicit flags
// always win.
func availableMemoryBytes() int64 {
	if limit, current, ok := cgroupMemoryV2(); ok {
		if limit > current {
			return limit - current
		}
		return limit
	}
	if available, ok := procMemAvailable(); ok {
		return available
	}
	if vm, err := mem.VirtualMemory(); err == nil && vm.Available > 0 {
		return int64(vm.Available)
	}
	return 0
}

func cgroupMemoryV2() (limit, current int64, ok bool) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || parts[0] != "0" || parts[1] != "" {
			continue
		}
		rel := strings.TrimSpace(parts[2])
		base := "/sys/fs/cgroup" + rel
		maxBytes, maxErr := readCgroupInt(base + "/memory.max")
		currentBytes, currentErr := readCgroupInt(base + "/memory.current")
		if maxErr != nil || currentErr != nil || maxBytes <= 0 {
			return 0, 0, false
		}
		return maxBytes, currentBytes, true
	}
	return 0, 0, false
}

func readCgroupInt(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	value := strings.TrimSpace(string(data))
	if value == "max" {
		return 0, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func procMemAvailable() (int64, bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "MemAvailable:" {
			continue
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err == nil && kb > 0 {
			return kb * 1024, true
		}
	}
	return 0, false
}

func autoConcurrency(available int64) int {
	if available <= 0 {
		return 8
	}
	mb := available / (32 << 20)
	switch {
	case mb < 4:
		return 4
	case mb > 64:
		return 64
	default:
		return int(mb)
	}
}

func autoMemoryLimit(available int64) int64 {
	if available <= 0 {
		return 0
	}
	limit := available * 65 / 100
	if limit < 32<<20 {
		limit = 32 << 20
	}
	if limit > available {
		limit = available
	}
	return limit
}
