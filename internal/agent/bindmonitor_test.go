package agent

import (
	"context"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/netbind"
	"github.com/mehrnet/radar-node/internal/wire"
)

func TestBindMonitorCachesFailure(t *testing.T) {
	binding, err := netbind.Parse("", "radar-definitely-missing0")
	if err != nil {
		t.Fatal(err)
	}
	monitor := newBindMonitor(binding)
	now := time.Now()
	first, changed := monitor.check(context.Background(), now)
	if first == "" || !changed {
		t.Fatalf("expected an initial bind failure, got %q changed=%v", first, changed)
	}
	second, changed := monitor.check(context.Background(), now.Add(5*time.Second))
	if second != first || changed {
		t.Fatalf("expected the cached failure within 30s, got %q changed=%v", second, changed)
	}
}

func TestNoteBindingPauseCountsDeferredJobs(t *testing.T) {
	a := &agent{cache: newProbeCache(), maxJobsPerProbe: 32}
	now := time.Now()
	a.cache.replaceAssignment("hash", []wire.ProbeSnapshot{
		dueProbe("a", 2, now),
		dueProbe("b", 1, now),
	})
	a.noteBindingPause(now)
	if got := a.deferredJobs.Load(); got != 3 {
		t.Fatalf("deferred jobs = %d, want 3", got)
	}
}

func TestOptionsForCarriesBinding(t *testing.T) {
	binding, err := netbind.Parse("127.0.0.2", "")
	if err != nil {
		t.Fatal(err)
	}
	a := &agent{bind: binding}
	opts := a.optionsFor(dueProbe("p", 1, time.Now()), 1)
	if opts.Bind != binding {
		t.Fatalf("options bind = %+v, want %+v", opts.Bind, binding)
	}
}
