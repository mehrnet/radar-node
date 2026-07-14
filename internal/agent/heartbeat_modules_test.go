package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/agent"
	"github.com/mehrnet/radar-node/internal/wire"
)

// TestRun_UploadsModulesOnHeartbeatRejectionThenSucceeds proves the
// full "server rejects with missing prober_ids -> agent uploads
// exactly those -> retry heartbeat succeeds" loop, not just each half
// of it in isolation.
func TestRun_UploadsModulesOnHeartbeatRejectionThenSucceeds(t *testing.T) {
	var (
		mu               sync.Mutex
		heartbeatCount   int
		uploadedProberID string
		accepted         bool
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/nodes/events", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(wire.EventsResponse{SpecVersion: 1, ServerTime: time.Now().UTC().Format(time.RFC3339Nano)})
	})
	mux.HandleFunc("/v1/nodes/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var req wire.HeartbeatRequest
		json.NewDecoder(r.Body).Decode(&req)

		mu.Lock()
		heartbeatCount++
		alreadyAccepted := accepted
		mu.Unlock()

		if !alreadyAccepted {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(wire.HeartbeatRejection{
				Error:                 wire.HeartbeatErrorModulesOutOfSync,
				MissingProberIDs:      []string{"tcp"},
				NodeStatus:            wire.NodeStatusActive,
				HeartbeatIntervalSecs: 3600,
			})
			return
		}
		json.NewEncoder(w).Encode(wire.HeartbeatResponse{
			SpecVersion: 1, NodeStatus: wire.NodeStatusActive, HeartbeatIntervalSecs: 3600,
		})
	})
	mux.HandleFunc("/v1/nodes/modules", func(w http.ResponseWriter, r *http.Request) {
		var req wire.ModulesUploadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		if len(req.Modules) == 1 {
			uploadedProberID = req.Modules[0].ProberID
			accepted = true
		}
		mu.Unlock()
		json.NewEncoder(w).Encode(wire.ModulesUploadResponse{SpecVersion: 1, Stored: len(req.Modules)})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- agent.Run(ctx, agent.Config{
			APIURL:         srv.URL,
			APIKey:         "node_test:secret",
			EventsInterval: time.Hour, // irrelevant to this test
			SchedulerTick:  time.Hour,
			Concurrency:    1,
		})
	}()

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		ok := accepted
		mu.Unlock()
		if ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for the heartbeat to be accepted after a module upload")
		case <-time.After(20 * time.Millisecond):
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

	mu.Lock()
	defer mu.Unlock()
	if uploadedProberID != "tcp" {
		t.Fatalf("expected the agent to upload exactly the missing prober_id \"tcp\", got %q", uploadedProberID)
	}
	if heartbeatCount < 2 {
		t.Fatalf("expected at least 2 heartbeat attempts (rejected, then retried), got %d", heartbeatCount)
	}
}
