package agent

import (
	"strings"
	"sync"
	"time"
)

const (
	pathHealthWindow    = 60 * time.Second
	pathMinTargets      = 5
	pathNetworkRatio    = 0.70
	pathMaxTrackedHosts = 4096
)

// pathHealth is a tiny rolling window over recent checks. It exists to
// distinguish "one target is down" from "this vantage's network path is
// broadly failing": only a wide set of distinct targets with a high
// share of network/timeout errors marks the path unhealthy. HTTP errors
// and single-target probes deliberately do not qualify.
type pathHealth struct {
	mu            sync.Mutex
	windowStart   time.Time
	total         int
	networkErrors int
	targets       map[string]struct{}
}

func newPathHealth() *pathHealth {
	return &pathHealth{targets: make(map[string]struct{})}
}

func (p *pathHealth) resetLocked(now time.Time) {
	if p.windowStart.IsZero() || now.Sub(p.windowStart) >= pathHealthWindow {
		p.windowStart = now
		p.total = 0
		p.networkErrors = 0
		clear(p.targets)
	}
}

func (p *pathHealth) record(target, errText, errCode string, at time.Time) {
	if errCode == "invalid_params" {
		// Misconfiguration carries no information about the node's path.
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resetLocked(at)
	p.total++
	if target != "" && len(p.targets) < pathMaxTrackedHosts {
		p.targets[target] = struct{}{}
	}
	if isNetworkError(errText, errCode) {
		p.networkErrors++
	}
}

func (p *pathHealth) state(now time.Time) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resetLocked(now)
	if len(p.targets) >= pathMinTargets && p.total > 0 && float64(p.networkErrors)/float64(p.total) >= pathNetworkRatio {
		return "degraded"
	}
	return "ok"
}

// isNetworkError deliberately ignores invalid_params and HTTP/status
// failures: those point at the target or the probe definition, not the
// node's own network path.
func isNetworkError(errText, errCode string) bool {
	if errCode == "invalid_params" || errText == "" {
		return false
	}
	lower := strings.ToLower(errText)
	for _, marker := range []string{
		"timeout",
		"deadline exceeded",
		"connection refused",
		"connection reset",
		"no such host",
		"network is unreachable",
		"network is down",
		"i/o timeout",
		"dial tcp",
		"dial udp",
		"connect: ",
		"read: ",
		"write: ",
		"eof",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
