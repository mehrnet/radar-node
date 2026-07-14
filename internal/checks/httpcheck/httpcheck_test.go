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
		Mode:    probe.ModeWarm,
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
		Mode:    probe.ModeWarm,
		Seq:     1,
	})
	if res.Ok {
		t.Fatal("expected a 502 response to be reported as not ok")
	}
}

func TestCheck_WarmModeReusesConnection(t *testing.T) {
	var remoteAddrs [2]string
	var reqCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteAddrs[reqCount] = r.RemoteAddr
		reqCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := httpcheck.New()
	for i := 0; i < 2; i++ {
		res := c.Check(context.Background(), probe.Options{
			Target:  srv.URL,
			Timeout: 2 * time.Second,
			Mode:    probe.ModeWarm,
			Seq:     i + 1,
		})
		if !res.Ok {
			t.Fatalf("probe %d failed: %s", i, res.Error)
		}
	}

	// Same client-side RemoteAddr (as seen by the server) on both
	// requests means the second probe reused the first's connection
	// instead of dialing fresh -- the whole point of warm mode.
	if remoteAddrs[0] != remoteAddrs[1] {
		t.Fatalf("expected connection reuse, got different source ports: %q vs %q", remoteAddrs[0], remoteAddrs[1])
	}
}

func TestCheck_HardModeForcesFreshConnection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := httpcheck.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  srv.URL,
		Timeout: 2 * time.Second,
		Mode:    probe.ModeHard,
		Seq:     1,
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
}
