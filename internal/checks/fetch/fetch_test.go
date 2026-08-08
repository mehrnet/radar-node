package fetch_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/checks/fetch"
	"github.com/mehrnet/radar-node/internal/probe"
)

func TestDo_ReturnsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	body, status, err := fetch.Do(context.Background(), probe.Options{Target: srv.URL, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if string(body) != "hello world" {
		t.Fatalf("unexpected body: %q", body)
	}
}

func TestDo_CapsBodySize(t *testing.T) {
	// 11MB of content, well over fetch's own 10MB cap.
	big := strings.Repeat("x", 11*1024*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(big))
	}))
	defer srv.Close()

	body, _, err := fetch.Do(context.Background(), probe.Options{Target: srv.URL, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(body) != 10*1024*1024 {
		t.Fatalf("expected body capped at 10MB, got %d bytes", len(body))
	}
}

func TestCheck_Fails_OnUnreachable(t *testing.T) {
	c := fetch.New()
	res := c.Check(context.Background(), probe.Options{Target: "http://127.0.0.1:1", Timeout: 500 * time.Millisecond})
	if res.Ok {
		t.Fatal("expected failure for an unreachable target")
	}
}
