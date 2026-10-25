package agent

import (
	"context"
	"sync"
	"time"

	"github.com/mehrnet/radar-node/internal/journal"
	"github.com/mehrnet/radar-node/internal/netbind"
)

const (
	bindCheckInterval  = 30 * time.Second
	maxBindErrorLength = 512
)

// bindMonitor caches the result of a cheap socket self-test so the
// scheduler and heartbeat paths agree on whether the configured
// binding is currently usable.
type bindMonitor struct {
	binding netbind.Binding

	mu          sync.Mutex
	checkedAt   time.Time
	initialized bool
	errText     string
}

func newBindMonitor(binding netbind.Binding) *bindMonitor {
	return &bindMonitor{binding: binding}
}

func (m *bindMonitor) check(ctx context.Context, now time.Time) (string, bool) {
	if m == nil || m.binding.IsZero() {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	age := now.Sub(m.checkedAt)
	if m.initialized && age >= 0 && age < bindCheckInterval {
		return m.errText, false
	}

	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	err := m.binding.SelfTest(checkCtx)
	cancel()
	text := ""
	if err != nil {
		text = trimBindError(err.Error())
	}
	changed := !m.initialized || text != m.errText
	m.initialized = true
	m.checkedAt = now
	m.errText = text
	return text, changed
}

func (a *agent) bindError(ctx context.Context, now time.Time) string {
	if a.bindMonitor == nil {
		return ""
	}
	text, changed := a.bindMonitor.check(ctx, now)
	if changed {
		if text != "" {
			journal.Warn("agent", "outbound binding unavailable; pausing probes",
				"binding", a.bind.String(), "err", text)
		} else {
			journal.Info("agent", "outbound binding available", "binding", a.bind.String())
		}
	}
	return text
}

// noteBindingPause keeps the deferred-job health signal meaningful
// while probes cannot run: due probes are left due, so report what
// would otherwise have been selected.
func (a *agent) noteBindingPause(now time.Time) {
	deferred := 0
	for _, pr := range a.cache.dueProbes(now) {
		deferred += a.probeJobCount(pr.ProbeCount)
	}
	a.deferredJobs.Store(int64(deferred + a.cache.pendingTriggerCount()))
}

func trimBindError(text string) string {
	if len(text) > maxBindErrorLength {
		return text[:maxBindErrorLength]
	}
	return text
}
