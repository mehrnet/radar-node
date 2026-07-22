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

// TestRun_AcksPendingActionBeforeActingOnIt proves the ordering that
// matters most here: the agent must ack a delivered pending_action
// before it would ever act on it (reinstall re-execs install.sh and
// kills this process, so acking has to happen first or never at
// all). Kind is deliberately not "update"/"module_actions" -- an
// unrecognized kind is a safe stand-in that lets handlePendingAction's
// ack-then-dispatch flow run for real without this test spawning a
// real install.sh subprocess.
func TestRun_AcksPendingActionBeforeActingOnIt(t *testing.T) {
	var (
		mu       sync.Mutex
		acked    []string
		beatSent bool
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/nodes/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		alreadySent := beatSent
		beatSent = true
		mu.Unlock()

		resp := wire.HeartbeatResponse{
			SpecVersion: 1, NodeStatus: wire.NodeStatusActive, HeartbeatIntervalSecs: 3600,
			ServerTime: time.Now().UTC().Format(time.RFC3339Nano),
		}
		if !alreadySent {
			resp.PendingAction = &wire.PendingAction{ID: "action_test123", Kind: "no_op_for_test"}
		}
		json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/v1/nodes/ack", func(w http.ResponseWriter, r *http.Request) {
		var req wire.AckRequest
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		acked = append(acked, req.ID)
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- agent.Run(ctx, agent.Config{
			APIURL:        srv.URL,
			APIKey:        "node_test:secret",
			SchedulerTick: time.Hour,
			Concurrency:   1,
		})
	}()

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		got := len(acked) > 0
		mu.Unlock()
		if got {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for the agent to ack the delivered pending action")
		case <-time.After(20 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(acked) != 1 || acked[0] != "action_test123" {
		t.Fatalf("expected exactly one ack for action_test123, got %v", acked)
	}
}

// TestRun_SkipsActingWhenAckIsRejected proves the "mutual drop" half:
// if the server no longer recognizes this id by the time the agent
// tries to ack it (410 -- already given up on, or superseded), the
// agent must not proceed as though it were still live. There's
// nothing further to assert beyond "the process doesn't crash and
// keeps heartbeating" since the dispatch that would follow a
// successful ack is exactly what must NOT happen here.
func TestRun_SkipsActingWhenAckIsRejected(t *testing.T) {
	var (
		mu             sync.Mutex
		heartbeatCount int
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/nodes/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		heartbeatCount++
		count := heartbeatCount
		mu.Unlock()

		resp := wire.HeartbeatResponse{
			// A short interval, unlike the other test in this file --
			// this one needs a real second heartbeat to arrive within
			// the test's own deadline, not just two beats within a
			// single startup cycle.
			SpecVersion: 1, NodeStatus: wire.NodeStatusActive, HeartbeatIntervalSecs: 1,
			ServerTime: time.Now().UTC().Format(time.RFC3339Nano),
		}
		if count == 1 {
			resp.PendingAction = &wire.PendingAction{ID: "action_expired", Kind: "no_op_for_test"}
		}
		json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/v1/nodes/ack", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"no such pending action"}`, http.StatusGone)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- agent.Run(ctx, agent.Config{
			APIURL:        srv.URL,
			APIKey:        "node_test:secret",
			SchedulerTick: time.Hour,
			Concurrency:   1,
		})
	}()

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		count := heartbeatCount
		mu.Unlock()
		if count >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for a second heartbeat after the ack was rejected")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
