package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/apiclient"
	"github.com/mehrnet/radar-node/internal/probe"
	"github.com/mehrnet/radar-node/internal/wire"
)

func testResults() []wire.Result {
	return []wire.Result{{
		RunID:      "run_1",
		ProbeID:    "probe_1",
		ObservedAt: "2026-10-21T00:00:00Z",
		Result:     probe.Result{Ok: true, Type: "xray", Target: "https://example.com", Seq: 1},
	}}
}

func TestResultSpoolRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	s, err := newResultSpool(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("spool dir mode = %v, want 0700", dirInfo.Mode().Perm())
	}

	path, err := s.enqueue(testResults(), "2026-10-21T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("spool file mode = %v, want 0600", fileInfo.Mode().Perm())
	}
	pending, err := s.pending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %v, %v; want 1 entry", pending, err)
	}
	batch, err := s.read(pending[0].path)
	if err != nil {
		t.Fatal(err)
	}
	if batch.SentAt != "2026-10-21T00:00:00Z" || len(batch.Results) != 1 || batch.Results[0].RunID != "run_1" {
		t.Fatalf("read batch = %+v", batch)
	}
	s.remove(pending[0].path)
	if pending, _ := s.pending(); len(pending) != 0 {
		t.Fatalf("pending after remove = %d, want 0", len(pending))
	}
}

func TestResultSpoolPruneKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	s, err := newResultSpool(dir, 1) // every file exceeds this; the newest must survive
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.enqueue(testResults(), "")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	second, err := s.enqueue(testResults(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("oldest spool file still exists: %v", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("newest spool file missing: %v", err)
	}
	if pending, _ := s.pending(); len(pending) != 1 {
		t.Fatalf("pending = %d, want the single newest file", len(pending))
	}
}

func TestNextSpoolBackoff(t *testing.T) {
	for _, tc := range []struct{ in, want time.Duration }{
		{0, 5 * time.Second},
		{5 * time.Second, 10 * time.Second},
		{30 * time.Second, time.Minute},
		{time.Minute, time.Minute},
	} {
		if got := nextDeliveryBackoff(tc.in); got != tc.want {
			t.Fatalf("nextDeliveryBackoff(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestDrainSpoolRetriesThenDeletes(t *testing.T) {
	var calls int32
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if fail.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"spec_version":1,"accepted":1,"rejected":0,"results":[],"node_status":"active"}`)
	}))
	defer srv.Close()

	client, err := apiclient.New(srv.URL, "node_test:secret", "")
	if err != nil {
		t.Fatal(err)
	}
	spool, err := newResultSpool(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	a := &agent{client: client, nodeID: "node_test", spool: spool}
	if _, err := spool.enqueue(testResults(), ""); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := a.drainSpool(context.Background()); err == nil {
		t.Fatal("expected the first drain to fail")
	}
	if pending, _ := spool.pending(); len(pending) != 1 {
		t.Fatalf("pending after failed drain = %d, want 1", len(pending))
	}
	if a.deliveryNextAttempt.IsZero() {
		t.Fatal("failed drain did not arm a retry backoff")
	}

	fail.Store(false)
	a.deliveryNextAttempt = time.Time{}
	a.deliveryBackoff = 0
	delivered, accepted, rejected, err := a.drainSpool(context.Background())
	if err != nil {
		t.Fatalf("drain after recovery: %v", err)
	}
	if delivered != 1 || accepted != 1 || rejected != 0 {
		t.Fatalf("delivered=%d accepted=%d rejected=%d", delivered, accepted, rejected)
	}
	if pending, _ := spool.pending(); len(pending) != 0 {
		t.Fatalf("pending after success = %d, want 0", len(pending))
	}
	if calls != 2 {
		t.Fatalf("server calls = %d, want 2", calls)
	}
}

func TestDrainSpoolDiscardsCorruptFile(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"spec_version":1,"accepted":1,"rejected":0,"results":[],"node_status":"active"}`)
	}))
	defer srv.Close()

	client, err := apiclient.New(srv.URL, "node_test:secret", "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	spool, err := newResultSpool(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "batch-00000000000000000001-bad.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := spool.enqueue(testResults(), ""); err != nil {
		t.Fatal(err)
	}
	a := &agent{client: client, nodeID: "node_test", spool: spool}

	delivered, _, _, err := a.drainSpool(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1 (corrupt file skipped)", delivered)
	}
	if calls != 1 {
		t.Fatalf("server calls = %d, want 1", calls)
	}
	if pending, _ := spool.pending(); len(pending) != 0 {
		t.Fatalf("pending = %d, want 0", len(pending))
	}
}
