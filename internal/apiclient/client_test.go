package apiclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/apiclient"
	"github.com/mehrnet/radar-node/internal/wire"
)

func TestHeartbeat_SendsSinceSeqAndParsesEvents(t *testing.T) {
	var gotAuth string
	var gotReq wire.HeartbeatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatal(err)
		}
		json.NewEncoder(w).Encode(wire.HeartbeatResponse{
			SpecVersion: 1,
			NodeStatus:  "active",
			ServerTime:  "2026-07-13T00:00:00Z",
			Events:      []wire.Event{{Seq: 11, EventType: "created", Job: wire.JobSnapshot{ID: "job_1", Target: "1.2.3.4:443", Prober: "tcp"}}},
		})
	}))
	defer srv.Close()

	c, err := apiclient.New(srv.URL, "node_1:secret", "")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Heartbeat(context.Background(), wire.HeartbeatRequest{NodeID: "node_1", SinceSeq: 10})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer node_1:secret" {
		t.Fatalf("expected bearer auth, got %q", gotAuth)
	}
	if gotReq.SinceSeq != 10 {
		t.Fatalf("expected since_seq=10 in the request body, got %d", gotReq.SinceSeq)
	}
	if len(resp.Events) != 1 || resp.Events[0].Job.ID != "job_1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestPostResults_SendsBody(t *testing.T) {
	var got wire.ResultsRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		json.NewEncoder(w).Encode(wire.ResultsResponse{Accepted: 1, NodeStatus: "active"})
	}))
	defer srv.Close()

	c, err := apiclient.New(srv.URL, "node_1:secret", "")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.PostResults(context.Background(), wire.ResultsRequest{
		NodeID:  "node_1",
		BatchID: "batch_1",
		Results: []wire.Result{{RunID: "run_1", JobID: "job_1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.NodeID != "node_1" || len(got.Results) != 1 {
		t.Fatalf("unexpected request body: %+v", got)
	}
	if resp.Accepted != 1 || resp.NodeStatus != "active" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestHeartbeat_ParsesModulesOutOfSyncRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(wire.HeartbeatRejection{
			Error:                 wire.HeartbeatErrorModulesOutOfSync,
			MissingProberIDs:      []string{"tcp", "xray-vless"},
			NodeStatus:            "active",
			HeartbeatIntervalSecs: 30,
		})
	}))
	defer srv.Close()

	c, err := apiclient.New(srv.URL, "node_1:secret", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Heartbeat(context.Background(), wire.HeartbeatRequest{NodeID: "node_1", Probers: []string{"tcp:oldhash"}})
	var rejected *apiclient.HeartbeatRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("expected a *HeartbeatRejectedError, got %v", err)
	}
	if len(rejected.Rejection.MissingProberIDs) != 2 || rejected.Rejection.MissingProberIDs[0] != "tcp" {
		t.Fatalf("unexpected missing_prober_ids: %v", rejected.Rejection.MissingProberIDs)
	}
}

func TestUploadModules_SendsBody(t *testing.T) {
	var got wire.ModulesUploadRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		json.NewEncoder(w).Encode(wire.ModulesUploadResponse{Stored: len(got.Modules)})
	}))
	defer srv.Close()

	c, err := apiclient.New(srv.URL, "node_1:secret", "")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.UploadModules(context.Background(), wire.ModulesUploadRequest{
		NodeID:  "node_1",
		Modules: []wire.ModuleUpload{{ProberID: "tcp", FileHash: "abc", YAML: "name: tcp\naction: tcp_connect\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.NodeID != "node_1" || len(got.Modules) != 1 || got.Modules[0].ProberID != "tcp" {
		t.Fatalf("unexpected request body: %+v", got)
	}
	if resp.Stored != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestDo_NonOKStatusReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c, err := apiclient.New(srv.URL, "node_1:secret", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Heartbeat(context.Background(), wire.HeartbeatRequest{NodeID: "node_1"})
	if err == nil {
		t.Fatal("expected an error for a 401 response")
	}
	var statusErr *apiclient.StatusError
	if !isStatusError(err, &statusErr) || statusErr.Code != http.StatusUnauthorized {
		t.Fatalf("expected *StatusError with code 401, got %v", err)
	}
}

func isStatusError(err error, target **apiclient.StatusError) bool {
	se, ok := err.(*apiclient.StatusError)
	if ok {
		*target = se
	}
	return ok
}

func TestNew_UnsupportedProxyScheme(t *testing.T) {
	if _, err := apiclient.New("http://example.com", "node_1:secret", "ftp://proxy.example.com"); err == nil {
		t.Fatal("expected an error for an unsupported proxy scheme")
	}
}

func TestNew_HTTPProxyIsActuallyUsed(t *testing.T) {
	var proxyHit bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHit = true
		json.NewEncoder(w).Encode(wire.HeartbeatResponse{SpecVersion: 1})
	}))
	defer proxy.Close()

	// The target URL doesn't need to resolve to anything real -- the
	// whole point is that the request should never reach it directly,
	// only the proxy should see traffic.
	c, err := apiclient.New("http://radar-api.invalid", "node_1:secret", proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Heartbeat(ctx, wire.HeartbeatRequest{NodeID: "node_1"}); err != nil {
		t.Fatal(err)
	}
	if !proxyHit {
		t.Fatal("expected the request to go through --api-proxy, but the proxy was never hit")
	}
}

func TestNew_SOCKS5ProxyBuildsWithoutError(t *testing.T) {
	// A live SOCKS5 round trip needs a real SOCKS5 server to test
	// against; here we only verify the transport builds correctly for
	// the scheme, which is the part unique to this option (http(s)
	// proxy wiring is exercised live above).
	if _, err := apiclient.New("http://radar-api.invalid", "node_1:secret", "socks5h://127.0.0.1:1080"); err != nil {
		t.Fatalf("expected socks5h scheme to build without error, got %v", err)
	}
}
