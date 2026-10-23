package agent

import (
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	gprocess "github.com/shirou/gopsutil/v4/process"

	"github.com/mehrnet/radar-node/internal/wire"
)

const healthSampleInterval = 30 * time.Second

// healthSampler keeps resource sampling cheap and infrequent (at most
// once per 30s) while still refreshing lightweight heartbeat RTT/failure
// state on every heartbeat. Pressure is entered after two consecutive
// high samples and cleared after two low ones, so a single transient
// spike does not flap the server's availability state.
type healthSampler struct {
	mu           sync.Mutex
	started      time.Time
	lastSample   time.Time
	resource     wire.NodeHealth
	prevCPUTotal float64
	prevCPUSteal float64
	lastRTT      time.Duration
	failures     int
	memHot       bool
	loadHot      bool
	stealHot     bool
	memStreak    int
	loadStreak   int
	stealStreak  int
}

func newHealthSampler() *healthSampler {
	return &healthSampler{started: time.Now()}
}

func (s *healthSampler) noteHeartbeat(d time.Duration, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.lastRTT = d
		s.failures = 0
	} else {
		s.failures++
	}
}

func (s *healthSampler) snapshot(now time.Time, pathState string, deferredJobs int) *wire.NodeHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastSample.IsZero() || now.Sub(s.lastSample) >= healthSampleInterval {
		s.sampleLocked(now)
	}
	health := s.resource
	health.HeartbeatRTTMs = float64(s.lastRTT) / float64(time.Millisecond)
	if health.HeartbeatRTTMs > 600_000 {
		health.HeartbeatRTTMs = 600_000
	}
	health.ConsecutiveFailures = s.failures
	if health.ConsecutiveFailures > 1_000_000 {
		health.ConsecutiveFailures = 1_000_000
	}
	health.DeferredJobs = deferredJobs
	if health.DeferredJobs > 1_000_000 {
		health.DeferredJobs = 1_000_000
	}
	health.Path = pathState
	health.UptimeSeconds = now.Sub(s.started).Seconds()
	if health.UptimeSeconds < 0 {
		health.UptimeSeconds = 0
	}
	return &health
}

func (s *healthSampler) sampleLocked(now time.Time) {
	s.lastSample = now
	s.resource.CPUCount = runtime.NumCPU()
	s.resource.Goroutines = runtime.NumGoroutine()

	if vm, err := mem.VirtualMemory(); err == nil {
		s.resource.MemUsedPct = vm.UsedPercent
	}
	if avg, err := load.Avg(); err == nil {
		s.resource.Load1 = avg.Load1
	}
	if p, err := gprocess.NewProcess(int32(os.Getpid())); err == nil {
		if info, err := p.MemoryInfo(); err == nil {
			s.resource.RSSBytes = int64(info.RSS)
		}
	}
	if times, err := cpu.Times(false); err == nil && len(times) > 0 {
		t := times[0]
		total := t.User + t.System + t.Idle + t.Nice + t.Iowait + t.Irq + t.Softirq + t.Steal
		if s.prevCPUTotal > 0 {
			totalDelta := total - s.prevCPUTotal
			stealDelta := t.Steal - s.prevCPUSteal
			if totalDelta > 0 {
				pct := stealDelta / totalDelta * 100
				if pct < 0 {
					pct = 0
				}
				if pct > 100 {
					pct = 100
				}
				s.resource.CPUStealPct = pct
			}
		}
		s.prevCPUTotal = total
		s.prevCPUSteal = t.Steal
	}

	cpuCount := float64(s.resource.CPUCount)
	if cpuCount < 1 {
		cpuCount = 1
	}
	s.memHot, s.memStreak = hysteresis(s.memHot, s.memStreak, s.resource.MemUsedPct >= 92, s.resource.MemUsedPct <= 85)
	s.loadHot, s.loadStreak = hysteresis(s.loadHot, s.loadStreak, s.resource.Load1 > cpuCount*2, s.resource.Load1 <= cpuCount*1.5)
	s.stealHot, s.stealStreak = hysteresis(s.stealHot, s.stealStreak, s.resource.CPUStealPct >= 30, s.resource.CPUStealPct <= 15)

	switch {
	case s.memHot && (s.loadHot || s.stealHot), s.loadHot && s.stealHot:
		s.resource.Pressure = "mixed"
	case s.memHot:
		s.resource.Pressure = "memory"
	case s.loadHot:
		s.resource.Pressure = "load"
	case s.stealHot:
		s.resource.Pressure = "steal"
	default:
		s.resource.Pressure = "ok"
	}
}

// hysteresis returns the next state/streak. Positive streaks count high
// samples, negative streaks count low samples; two in a row are needed
// to enter or leave a pressure state.
func hysteresis(active bool, streak int, high, low bool) (bool, int) {
	switch {
	case high:
		if streak < 0 {
			streak = 1
		} else {
			streak++
		}
		return active || streak >= 2, streak
	case low:
		if streak > 0 {
			streak = -1
		} else {
			streak--
		}
		if streak <= -2 {
			return false, 0
		}
		return active, streak
	default:
		return active, streak
	}
}
