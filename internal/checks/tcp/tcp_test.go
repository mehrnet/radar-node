package tcp_test

import (
	"context"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/checks/tcp"
	"github.com/mehrnet/radar-node/internal/probe"
)

func TestCheck_Success(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	c := tcp.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  ln.Addr().String(),
		Timeout: time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if res.LatencyMs == nil {
		t.Fatal("expected latency_ms to be set")
	}
}

func TestCheck_ConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // free the port but keep a valid, unreachable address

	c := tcp.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  addr,
		Timeout: time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
	})
	if res.Ok {
		t.Fatal("expected failure for a closed port")
	}
}

func TestCheck_TLS(t *testing.T) {
	srv := httptest.NewTLSServer(nil)
	defer srv.Close()

	target := srv.Listener.Addr().String()
	c := tcp.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  target,
		Timeout: 2 * time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
		Params: map[string]any{
			"tls":      "true",
			"insecure": "true",
		},
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if res.Extra["tls_version"] != "1.3" {
		t.Fatalf("expected tls 1.3, got %v", res.Extra["tls_version"])
	}
}

func TestCheck_TLS_CertVerifyFailsWithoutInsecure(t *testing.T) {
	srv := httptest.NewTLSServer(nil)
	defer srv.Close()

	c := tcp.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  srv.Listener.Addr().String(),
		Timeout: 2 * time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
		Params:  map[string]any{"tls": "true"},
	})
	if res.Ok {
		t.Fatal("expected self-signed cert to fail verification without insecure=true")
	}
}
