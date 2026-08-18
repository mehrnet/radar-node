package agent_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
	// prober/probeCount default to "tcp"/2 (the plain single-check
	// path) when left unset -- TestRun_BatchesPooledProberChecksIntoOneEngineInstance
	// overrides both to exercise a pooled module instead.
	prober     string
	probeCount int
	timeoutMs  int
	// probeIDs defaults to a single "probe_test" probe (probeCount
	// checks of it) when left unset -- TestRun_DestinationIntervalSpacesChecksAgainstAOneSharedTarget
	// overrides it with more than one id, all sharing f.target, to get
	// several *separate* probes (probeCount=1 each) due at the same
	// tick against one destination, rather than one probe's own
	// repeated checks.
	probeIDs []string
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
			prober := f.prober
			if prober == "" {
				prober = "tcp"
			}
			probeCount := f.probeCount
			if probeCount == 0 {
				probeCount = 2
			}
			timeoutMs := f.timeoutMs
			if timeoutMs == 0 {
				timeoutMs = 1000
			}
			ids := f.probeIDs
			if len(ids) == 0 {
				ids = []string{"probe_test"}
			}
			// "created" alone would never run at all now -- a manual
			// probe only executes via an explicit "triggered" event, so
			// this fakes exactly that: a create immediately followed by
			// one trigger, both applied from a single heartbeat
			// response the same way a real create-then-click-"Run now"
			// would arrive across two real ones. seq just needs to be
			// unique within this one response. runID stays the single
			// original literal for the (far more common) one-probe
			// case, matching every existing test's own expectation --
			// only suffixed per-id when f.probeIDs was actually set to
			// more than one, so results from different probes in the
			// same response are still trivially distinguishable.
			seq := 1
			for _, id := range ids {
				snapshot := wire.ProbeSnapshot{
					ID:           id,
					Target:       f.target,
					Prober:       prober,
					ProbeCount:   probeCount,
					TimeoutMs:    timeoutMs,
					ScheduleType: "manual",
					Status:       wire.ProbeStatusActive,
					StartsAt:     time.Now().Add(-time.Hour).UnixMilli(),
				}
				runID := "run_test_trigger"
				if len(f.probeIDs) > 1 {
					runID += "_" + id
				}
				resp.Events = append(resp.Events,
					wire.Event{Seq: seq, EventType: "created", Probe: snapshot},
					wire.Event{Seq: seq + 1, EventType: "triggered", RunID: runID, Probe: snapshot},
				)
				seq += 2
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

// TestRun_BatchesPooledProberChecksIntoOneEngineInstance is the
// scheduler-level counterpart to internal/module's own PoolChecker
// tests: it proves the agent's real due-check dispatch (executeProbes/
// runChecks) actually groups a pooled prober's checks into one
// CheckBatch call -- one build_config/start pair for all of a probe's
// probe_count checks -- rather than firing one goroutine (and one
// subprocess lifecycle) per check the way every other prober still
// does. This is the whole point of pooling: it would be meaningless if
// PoolChecker worked in isolation but the scheduler never actually
// used it as a batch.
func TestRun_BatchesPooledProberChecksIntoOneEngineInstance(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

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

	modulesDir := t.TempDir()
	buildConfigLog := filepath.Join(modulesDir, "build-config.log")

	buildConfigScript := filepath.Join(modulesDir, "build-config.py")
	writeExecutable(t, buildConfigScript, `
import shutil, sys
with open("`+buildConfigLog+`", "a") as log:
    log.write("instance\n")
shutil.copy(sys.argv[1], sys.argv[2])
`)
	startScript := filepath.Join(modulesDir, "start.py")
	writeExecutable(t, startScript, `
import sys, json, socket, threading, time

with open(sys.argv[1]) as f:
    jobs = json.load(f)

def serve(port):
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("127.0.0.1", port))
    s.listen(5)
    while True:
        conn, _ = s.accept()
        conn.close()

for j in jobs:
    threading.Thread(target=serve, args=(j["alloc_port"],), daemon=True).start()

while True:
    time.sleep(1)
`)
	runScript := filepath.Join(modulesDir, "run.py")
	writeExecutable(t, runScript, `
import socket, sys
port = int(sys.argv[1])
s = socket.create_connection(("127.0.0.1", port), timeout=2)
s.close()
print('{"latency_ms": 1}')
`)

	if err := os.WriteFile(filepath.Join(modulesDir, "pool-mod.yaml"), []byte(`
name: pool-mod
run:
  command: ["python3", "`+runScript+`", "{{alloc_port}}"]
collect:
  format: writeout_json
pool:
  max_jobs_per_instance: 100
  test_concurrency: 5
  build_config:
    command: ["python3", "`+buildConfigScript+`", "{{jobs_json}}", "{{config_path}}"]
  start:
    command: ["python3", "`+startScript+`", "{{config_path}}"]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	fake := newFakeAPI(ln.Addr().String())
	fake.prober = "pool-mod"
	fake.probeCount = 5
	fake.timeoutMs = 5000
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
			ModulesDir:    modulesDir,
		})
	}()

	got := 0
	deadline := time.After(5 * time.Second)
	for got < 5 {
		select {
		case <-fake.resultsAdded:
			got++
		case <-deadline:
			t.Fatalf("timed out waiting for results; got %d of 5", got)
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
	results := append([]wire.Result(nil), fake.gotResults...)
	fake.mu.Unlock()
	if len(results) != 5 {
		t.Fatalf("expected 5 results, got %d", len(results))
	}
	for _, r := range results {
		if !r.Ok {
			t.Errorf("expected a successful pooled check, got %+v", r)
		}
	}

	log, err := os.ReadFile(buildConfigLog)
	if err != nil {
		t.Fatalf("expected build_config to have run at least once: %v", err)
	}
	gotInstances := 0
	for _, b := range log {
		if b == '\n' {
			gotInstances++
		}
	}
	if gotInstances != 1 {
		t.Fatalf("expected the scheduler to batch all 5 due checks into a single pool instance (1 build_config invocation), got %d", gotInstances)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
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

// TestRun_DestinationIntervalSpacesChecksAgainstASharedTarget is the
// scheduler-level counterpart to internal/destgate's own unit tests:
// it proves the agent's real due-check dispatch (executeProbes/
// runChecks/runCheck) actually calls destgate.Wait before running a
// check, not just that destgate works in isolation. Two entirely
// separate probes (different ids, both probeCount=1, both "manual"
// triggered on the same heartbeat -- so both are due on the exact
// same scheduler tick) share one Target; with DestinationInterval set
// well above the tcp check's own near-instant dial time, their
// results' own ObservedAt timestamps should land at least
// DestinationInterval apart, proving the second one waited for the
// first's destination slot to clear rather than running concurrently
// with it (which is what a shared destination hammered by unrelated
// probes looks like in production -- see this feature's own plan).
func TestRun_DestinationIntervalSpacesChecksAgainstASharedTarget(t *testing.T) {
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
	fake.probeCount = 1
	fake.probeIDs = []string{"probe_a", "probe_b"}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const destinationInterval = 300 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		done <- agent.Run(ctx, agent.Config{
			APIURL:              srv.URL,
			APIKey:              "node_test:secret",
			SchedulerTick:       20 * time.Millisecond,
			Concurrency:         4,
			DestinationInterval: destinationInterval,
		})
	}()

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
	results := append([]wire.Result(nil), fake.gotResults...)
	fake.mu.Unlock()
	if len(results) != 2 {
		t.Fatalf("expected 2 results (one per probe), got %d: %+v", len(results), results)
	}

	var observedAt [2]time.Time
	for i, r := range results {
		if !r.Ok {
			t.Errorf("expected a successful tcp probe against a live listener, got %+v", r)
		}
		ts, err := time.Parse(time.RFC3339Nano, r.ObservedAt)
		if err != nil {
			t.Fatalf("observed_at %q did not parse: %v", r.ObservedAt, err)
		}
		observedAt[i] = ts
	}

	first, second := observedAt[0], observedAt[1]
	if second.Before(first) {
		first, second = second, first
	}
	gap := second.Sub(first)
	if gap < destinationInterval-50*time.Millisecond {
		t.Fatalf("two checks against the same target were only %v apart, expected at least ~%v (destgate should have spaced them)", gap, destinationInterval)
	}
}
