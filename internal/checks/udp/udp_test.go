package udp_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/checks/udp"
	"github.com/mehrnet/radar-node/internal/probe"
)

func TestCheck_Responds(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	go func() {
		buf := make([]byte, 512)
		_, addr, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		_, _ = conn.WriteTo([]byte("pong"), addr)
	}()

	c := udp.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  conn.LocalAddr().String(),
		Timeout: 2 * time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if responded, _ := res.Extra["responded"].(bool); !responded {
		t.Fatalf("expected responded=true, got %v", res.Extra)
	}
}

func TestCheck_NoResponseIsInconclusiveNotFailure(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Listener exists but never replies -- silent drop, the common
	// case for UDP services that don't echo unsolicited probes.

	c := udp.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  conn.LocalAddr().String(),
		Timeout: 300 * time.Millisecond,
		Mode:    probe.ModeWarm,
		Seq:     1,
	})
	if !res.Ok {
		t.Fatalf("expected ok=true (inconclusive, not failure), got error %q", res.Error)
	}
	if responded, _ := res.Extra["responded"].(bool); responded {
		t.Fatal("expected responded=false")
	}
}

func TestCheck_ConnectionRefused(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := conn.LocalAddr().String()
	conn.Close() // nothing listening now -> kernel should ICMP-unreachable us

	c := udp.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  addr,
		Timeout: time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
	})
	if res.Ok {
		t.Fatal("expected connection refused to be reported as a failure")
	}
}
