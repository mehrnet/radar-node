package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/agent"
	"github.com/mehrnet/radar-node/internal/wire"
)

func TestRunSendsNodeHealthTelemetry(t *testing.T) {
	healthCh := make(chan wire.NodeHealth, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/nodes/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var req wire.HeartbeatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Health != nil {
			select {
			case healthCh <- *req.Health:
			default:
			}
		}
		_ = json.NewEncoder(w).Encode(wire.HeartbeatResponse{
			SpecVersion:           2,
			NodeStatus:            wire.NodeStatusActive,
			HeartbeatIntervalSecs: 3600,
			ServerTime:            time.Now().UTC().Format(time.RFC3339Nano),
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- agent.Run(ctx, agent.Config{
			APIURL: srv.URL, APIKey: "node_test:secret",
			SchedulerTick: 20 * time.Millisecond, Concurrency: 2,
		})
	}()

	select {
	case health := <-healthCh:
		if health.CPUCount < 1 || health.Goroutines < 1 {
			t.Fatalf("missing resource sample: %+v", health)
		}
		if health.Pressure == "" || health.Path == "" {
			t.Fatalf("health pressure/path not populated: %+v", health)
		}
		if health.MemUsedPct < 0 || health.MemUsedPct > 100 {
			t.Fatalf("invalid memory percentage: %v", health.MemUsedPct)
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("timed out waiting for a health-bearing heartbeat")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("agent.Run returned: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent.Run did not exit after cancellation")
	}
}
