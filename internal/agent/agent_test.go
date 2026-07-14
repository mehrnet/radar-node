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
	mu           sync.Mutex
	target       string
	served       bool // hand out the one job-created event exactly once
	gotResults   []wire.Result
	resultsAdded chan struct{}
}

func newFakeAPI(target string) *fakeAPI {
	return &fakeAPI{target: target, resultsAdded: make(chan struct{}, 8)}
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
		f.mu.Lock()
		defer f.mu.Unlock()
		resp := wire.HeartbeatResponse{
			SpecVersion:           1,
			NodeStatus:            wire.NodeStatusActive,
			HeartbeatIntervalSecs: 3600, // long enough to not interfere with the test
			ServerTime:            time.Now().UTC().Format(time.RFC3339Nano),
		}
		if !f.served {
			f.served = true
			resp.Events = []wire.Event{{
				Seq:       1,
				EventType: "created",
				Job: wire.JobSnapshot{
					ID:           "job_test",
					Target:       f.target,
					Prober:       "tcp",
					Mode:         "warm",
					ProbeCount:   2,
					TimeoutMs:    1000,
					ScheduleType: "once",
					Status:       wire.JobStatusActive,
					StartsAt:     time.Now().Add(-time.Hour).UnixMilli(),
				},
			}}
		}
		json.NewEncoder(w).Encode(resp)
	})
	return mux
}

func TestRun_SyncsExecutesAndReportsJob(t *testing.T) {
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

	// Wait for both probes of the one job (a "once" job, already
	// past its starts_at, so it's due the moment it's synced) to be
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
		if r.RunID == "" || r.JobID != "job_test" {
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
	// A "once" job must only ever run once even though the scheduler
	// ticks many times over a 5s wait -- if markRun-before-execute
	// wasn't working, we'd see far more than 2 results.
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
