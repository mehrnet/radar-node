package agent_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/agent"
	"github.com/mehrnet/radar-node/internal/wire"
)

// fakeAPI implements just enough of radar-api's node-facing surface
// to drive a real agent.Run loop end-to-end.
type fakeAPI struct {
	mu               sync.Mutex
	target           string
	served           bool // hand out the one probe-created event exactly once
	gotResults       []wire.Result
	resultsAdded     chan struct{}
	lastAgentVersion string
	versionSeen      chan struct{}
}

func newFakeAPI(target string) *fakeAPI {
	return &fakeAPI{target: target, resultsAdded: make(chan struct{}, 8), versionSeen: make(chan struct{}, 8)}
}

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/nodes/results", func(w http.ResponseWriter, r *http.Request) {
		var req wire.ResultsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.gotResults = append(f.gotResults, req.Results...)
		n := len(req.Results)
		f.mu.Unlock()
		for i := 0; i < n; i++ {
			f.resultsAdded <- struct{}{}
		}
		json.NewEncoder(w).Encode(wire.ResultsResponse{
			SpecVersion: 1,
			Accepted:    len(req.Results),
			NodeStatus:  wire.NodeStatusActive,
		})
	})
	mux.HandleFunc("/v1/nodes/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var req wire.HeartbeatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastAgentVersion = req.AgentVersion
		select {
		case f.versionSeen <- struct{}{}:
		default:
		}
		resp := wire.HeartbeatResponse{
			SpecVersion:           1,
			NodeStatus:            wire.NodeStatusActive,
			HeartbeatIntervalSecs: 3600, // long enough to not interfere with the test
			ServerTime:            time.Now().UTC().Format(time.RFC3339Nano),
		}
		if !f.served {
			f.served = true
			snapshot := wire.ProbeSnapshot{
				ID:           "probe_test",
				Target:       f.target,
				Prober:       "tcp",
				Mode:         "warm",
				ProbeCount:   2,
				TimeoutMs:    1000,
				ScheduleType: "manual",
				Status:       wire.ProbeStatusActive,
				StartsAt:     time.Now().Add(-time.Hour).UnixMilli(),
			}
			// "created" alone would never run at all now -- a manual
			// probe only executes via an explicit "triggered" event, so
			// this fakes exactly that: a create immediately followed by
			// one trigger, both applied from a single heartbeat
			// response the same way a real create-then-click-"Run now"
			// would arrive across two real ones.
			resp.Events = []wire.Event{
				{Seq: 1, EventType: "created", Probe: snapshot},
				{Seq: 2, EventType: "triggered", RunID: "run_test_trigger", Probe: snapshot},
			}
		}
		json.NewEncoder(w).Encode(resp)
	})
	return mux
}

func TestRun_SyncsExecutesAndReportsProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	fake := newFakeAPI(ln.Addr().String())
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- agent.Run(ctx, agent.Config{
			APIURL:        srv.URL,
			APIKey:        "node_test:secret",
			SchedulerTick: 20 * time.Millisecond,
			Concurrency:   4,
		})
	}()

	// Wait for both checks of the one probe (a "manual" probe, never due
	// on its own -- these only run because the fake heartbeat handler
	// also fired a "triggered" event for it, see newFakeAPI) to be
	// reported, or time out if the loop never syncs/schedules/
	// executes/reports correctly.
	got := 0
	deadline := time.After(5 * time.Second)
	for got < 2 {
		select {
		case <-fake.resultsAdded:
			got++
		case <-deadline:
			t.Fatalf("timed out waiting for results; got %d of 2", got)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("agent.Run returned an error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent.Run did not exit after context cancellation")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.gotResults) != 2 {
		t.Fatalf("expected 2 results, got %d", len(fake.gotResults))
	}
	seenSeqs := map[int]bool{}
	for _, r := range fake.gotResults {
		if r.RunID == "" || r.ProbeID != "probe_test" {
			t.Errorf("unexpected correlation fields: %+v", r)
		}
		if !r.Ok {
			t.Errorf("expected a successful tcp probe against a live listener, got %+v", r)
		}
		if r.ObservedAt == "" {
			t.Errorf("expected observed_at to be set: %+v", r)
		}
		seenSeqs[r.Seq] = true
	}
	if !seenSeqs[1] || !seenSeqs[2] {
		t.Fatalf("expected seq 1 and 2 (probe_count=2), got %+v", fake.gotResults)
	}
	// A single "triggered" event must only ever run once even though the
	// scheduler ticks many times over a 5s wait -- if
	// drainPendingTriggers wasn't actually draining the queue, we'd see
	// far more than 2 results.
	for _, r := range fake.gotResults {
		if r.RunID != "run_test_trigger" {
			t.Errorf("expected the server-issued trigger run_id to be reported verbatim, got %+v", r)
		}
	}
}

func TestRun_ReportsConfiguredVersionInHeartbeat(t *testing.T) {
	fake := newFakeAPI("127.0.0.1:0")
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- agent.Run(ctx, agent.Config{
			APIURL:        srv.URL,
			APIKey:        "node_test:secret",
			Version:       "0.5",
			SchedulerTick: 20 * time.Millisecond,
			Concurrency:   4,
		})
	}()

	select {
	case <-fake.versionSeen:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timed out waiting for a heartbeat")
	}
	cancel()
	<-done

	fake.mu.Lock()
	got := fake.lastAgentVersion
	fake.mu.Unlock()
	if got != "0.5" {
		t.Fatalf("expected the configured version %q to be reported verbatim, got %q", "0.5", got)
	}
}

func TestRun_DefaultsVersionToDevWhenUnset(t *testing.T) {
	fake := newFakeAPI("127.0.0.1:0")
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- agent.Run(ctx, agent.Config{
			APIURL:        srv.URL,
			APIKey:        "node_test:secret",
			SchedulerTick: 20 * time.Millisecond,
			Concurrency:   4,
		})
	}()

	select {
	case <-fake.versionSeen:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timed out waiting for a heartbeat")
	}
	cancel()
	<-done

	fake.mu.Lock()
	got := fake.lastAgentVersion
	fake.mu.Unlock()
	if got != "dev" {
		t.Fatalf("expected an unset Version to default to %q, got %q", "dev", got)
	}
}

func TestRun_RejectsMalformedAPIKey(t *testing.T) {
	err := agent.Run(context.Background(), agent.Config{
		APIURL:        "http://127.0.0.1:0",
		APIKey:        "not-a-valid-key",
		SchedulerTick: time.Second,
		Concurrency:   1,
	})
	if err == nil {
		t.Fatal("expected an error for an api-key without a colon")
	}
}
