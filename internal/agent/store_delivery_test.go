package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/apiclient"
	"github.com/mehrnet/radar-node/internal/probe"
	"github.com/mehrnet/radar-node/internal/store"
	"github.com/mehrnet/radar-node/internal/wire"
)

func storeTestResults() []wire.Result {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	latency := 8.5
	return []wire.Result{
		{RunID: "run_1", ProbeID: "probe_1", ObservedAt: now, Result: probe.Result{Ok: true, Type: "tcp", Target: "example.com:443", Seq: 1, LatencyMs: &latency}},
		{RunID: "run_1", ProbeID: "probe_1", ObservedAt: now, Result: probe.Result{Ok: false, Type: "tcp", Target: "example.com:443", Seq: 2, Error: "timeout"}},
	}
}

func TestDrainStoreSettlesAcksAndBacksOff(t *testing.T) {
	var calls int32
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if fail.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		var req wire.ResultsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		acks := make([]wire.ResultAck, 0, len(req.Results))
		accepted, rejected := 0, 0
		for _, result := range req.Results {
			ack := wire.ResultAck{RunID: result.RunID, Seq: result.Seq, Accepted: result.Seq == 1}
			if ack.Accepted {
				accepted++
			} else {
				ack.Reason = "duplicate"
				rejected++
			}
			acks = append(acks, ack)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(wire.ResultsResponse{
			SpecVersion: 1, Accepted: accepted, Rejected: rejected, Results: acks, NodeStatus: wire.NodeStatusActive,
		})
	}))
	defer srv.Close()

	local, err := store.Open(filepath.Join(t.TempDir(), "radar.db"), store.Options{Origin: "node_test"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer local.Close()
	if _, err := local.RecordResults(context.Background(), storeTestResults()); err != nil {
		t.Fatalf("RecordResults: %v", err)
	}
	client, err := apiclient.New(srv.URL, "node_test:secret", "")
	if err != nil {
		t.Fatal(err)
	}
	a := &agent{client: client, nodeID: "node_test", store: local}

	if _, _, _, err := a.drainStore(context.Background()); err == nil {
		t.Fatal("expected the first drain to fail")
	}
	if a.spoolNextAttempt.IsZero() {
		t.Fatal("failed drain did not arm a retry backoff")
	}
	if pending, _ := local.PendingResults(context.Background(), 10); len(pending) != 2 {
		t.Fatalf("pending after failed drain = %d, want 2", len(pending))
	}

	fail.Store(false)
	a.spoolNextAttempt = time.Time{}
	a.spoolBackoff = 0
	delivered, accepted, rejected, err := a.drainStore(context.Background())
	if err != nil {
		t.Fatalf("drain after recovery: %v", err)
	}
	if delivered != 2 || accepted != 1 || rejected != 1 {
		t.Fatalf("delivered=%d accepted=%d rejected=%d", delivered, accepted, rejected)
	}
	if pending, _ := local.PendingResults(context.Background(), 10); len(pending) != 0 {
		t.Fatalf("pending after success = %d, want 0", len(pending))
	}
	stats, err := local.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.DeliveredResults != 1 || stats.RejectedResults != 1 {
		t.Fatalf("delivered=%d rejected=%d, want 1/1", stats.DeliveredResults, stats.RejectedResults)
	}
	if calls != 2 {
		t.Fatalf("server calls = %d, want 2", calls)
	}
}

func TestOutcomesForMissingAckSettles(t *testing.T) {
	batch := []store.PendingResult{
		{Seq: 1, Result: wire.Result{RunID: "run_1", Result: probe.Result{Seq: 1}}},
		{Seq: 2, Result: wire.Result{RunID: "run_1", Result: probe.Result{Seq: 2}}},
	}
	outcomes := outcomesFor(batch, &wire.ResultsResponse{
		Results: []wire.ResultAck{{RunID: "run_1", Seq: 1, Accepted: true}},
	})
	if len(outcomes) != 2 || !outcomes[0].Accepted || !outcomes[1].Accepted {
		t.Fatalf("outcomes = %+v, want both settled", outcomes)
	}
	if outcomesFor(batch, nil)[0].Seq != 1 {
		t.Fatalf("nil response should settle every row")
	}
	if ackKey("run_1", 1) != "run_1\x001" {
		t.Fatalf("ackKey = %q", ackKey("run_1", 1))
	}
}
