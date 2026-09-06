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

// The gap expect_status exists to close: without it, only 5xx fails,
// so a Cloudflare Worker answering 404 for a bad route or 403 behind a
// WAF reported as perfectly healthy.
func TestCheck_NotFoundIsOkWithoutExpectStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	res := httpcheck.New().Check(context.Background(), probe.Options{Target: srv.URL, Timeout: 2 * time.Second, Seq: 1})
	if !res.Ok {
		t.Fatal("default rule should still treat 404 as up -- changing that silently would flip existing probes")
	}
}

func TestCheck_ExpectStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   int
		expect string
		wantOk bool
	}{
		{"exact match", 200, "200", true},
		{"exact mismatch", 404, "200", false},
		{"forbidden fails", 403, "200", false},
		{"range covers", 204, "200-299", true},
		{"range excludes", 301, "200-299", false},
		{"list covers", 204, "200,204", true},
		{"list excludes", 202, "200,204", false},
		{"whitespace tolerated", 204, " 200 , 204 ", true},
		// An expectation replaces the default rule rather than adding
		// to it, so a target that is supposed to answer 500 can be
		// monitored as healthy when it does.
		{"5xx can be the expectation", 500, "500", true},
		// ...and the same expectation still fails when it is not met.
		{"5xx expectation unmet", 502, "500", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
			}))
			defer srv.Close()

			res := httpcheck.New().Check(context.Background(), probe.Options{
				Target: srv.URL, Timeout: 2 * time.Second, Seq: 1,
				Params: map[string]any{"expect_status": tc.expect},
			})
			if res.Ok != tc.wantOk {
				t.Fatalf("code %d against %q: ok=%v, want %v (error %q)", tc.code, tc.expect, res.Ok, tc.wantOk, res.Error)
			}
			if !tc.wantOk && res.Error == "" {
				t.Fatal("a failed expectation must say what it expected")
			}
		})
	}
}

// A typo'd expectation fails the check outright instead of quietly
// falling back to the default rule, which would leave someone
// believing they had asserted something they had not.
func TestCheck_MalformedExpectStatusFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	for _, spec := range []string{"abc", "99", "600", "299-200", "200-"} {
		res := httpcheck.New().Check(context.Background(), probe.Options{
			Target: srv.URL, Timeout: 2 * time.Second, Seq: 1,
			Params: map[string]any{"expect_status": spec},
		})
		if res.Ok {
			t.Fatalf("expect_status=%q should have failed the check outright", spec)
		}
	}
}
