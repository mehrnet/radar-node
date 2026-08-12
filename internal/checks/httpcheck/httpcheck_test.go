package httpcheck_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/checks/httpcheck"
	"github.com/mehrnet/radar-node/internal/probe"
)

func TestCheck_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := httpcheck.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  srv.URL,
		Timeout: 2 * time.Second,
		Seq:     1,
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if code, _ := res.Extra["http_code"].(int); code != http.StatusNoContent {
		t.Fatalf("expected http_code 204, got %v", res.Extra["http_code"])
	}
}

func TestCheck_ServerErrorIsNotOk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := httpcheck.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  srv.URL,
		Timeout: 2 * time.Second,
		Seq:     1,
	})
	if res.Ok {
		t.Fatal("expected a 502 response to be reported as not ok")
	}
}

// Regression guard for the dual cold/warm redesign: a single Check()
// call always reports both figures -- there is no longer a mode-
// dependent branch that could silently report only one.
func TestCheck_ReportsBothColdAndWarmFigures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := httpcheck.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  srv.URL,
		Timeout: 2 * time.Second,
		Seq:     1,
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	for _, key := range []string{"cold_ttfb_ms", "warm_ttfb_ms", "dns_ms", "connect_ms", "tls_ms", "bytes"} {
		if _, ok := res.Extra[key]; !ok {
			t.Errorf("expected Extra to contain %q, got %+v", key, res.Extra)
		}
	}
	if res.LatencyMs == nil {
		t.Fatal("expected latency_ms to be set")
	}
}

// Each Check() call makes two requests -- one on a throwaway,
// non-pooled transport (the cold half) and one on the Checker's own
// shared transport (the warm half, see New()) -- and the warm half is
// what actually benefits from connection reuse *across* calls.
func TestCheck_WarmRequestReusesConnectionAcrossCalls(t *testing.T) {
	var remoteAddrs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteAddrs = append(remoteAddrs, r.RemoteAddr)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := httpcheck.New()
	for i := 0; i < 2; i++ {
		res := c.Check(context.Background(), probe.Options{
			Target:  srv.URL,
			Timeout: 2 * time.Second,
			Seq:     i + 1,
		})
		if !res.Ok {
			t.Fatalf("probe %d failed: %s", i, res.Error)
		}
	}

	// Request order within and across calls is deterministic (cold,
	// then warm, sequentially -- see Check's own comment): [0]=call1
	// cold, [1]=call1 warm, [2]=call2 cold, [3]=call2 warm.
	if len(remoteAddrs) != 4 {
		t.Fatalf("expected 4 requests total (cold+warm per call), got %d: %v", len(remoteAddrs), remoteAddrs)
	}
	if remoteAddrs[1] != remoteAddrs[3] {
		t.Fatalf("expected the warm requests to reuse one connection across calls, got %q vs %q", remoteAddrs[1], remoteAddrs[3])
	}
	if remoteAddrs[0] == remoteAddrs[2] {
		t.Fatalf("expected each call's own cold request to dial fresh, got the same source port twice: %q", remoteAddrs[0])
	}
}
