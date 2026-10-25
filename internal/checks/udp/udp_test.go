package udp_test

import (
	"context"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/checks/udp"
	"github.com/mehrnet/radar-node/internal/netbind"
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
		Seq:     1,
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if responded, _ := res.Extra["responded"].(bool); !responded {
		t.Fatalf("expected responded=true, got %v", res.Extra)
	}
}

func TestCheck_BindsSourceIP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("127.0.0.2 is not assigned by default on the Windows CI runner")
	}
	binding, err := netbind.Parse("127.0.0.2", "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	source := make(chan string, 1)
	go func() {
		buf := make([]byte, 512)
		_, addr, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		source <- addr.(*net.UDPAddr).IP.String()
		_, _ = conn.WriteTo([]byte("pong"), addr)
	}()

	c := udp.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  conn.LocalAddr().String(),
		Timeout: 2 * time.Second,
		Seq:     1,
		Bind:    binding,
	})
	if !res.Ok {
		if strings.Contains(res.Error, "cannot assign requested address") {
			t.Skipf("loopback source binding unavailable: %s", res.Error)
		}
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	select {
	case got := <-source:
		if got != "127.0.0.2" {
			t.Fatalf("server saw source %s, want 127.0.0.2", got)
		}
	case <-time.After(time.Second):
		t.Fatal("UDP listener did not observe a packet")
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
		Seq:     1,
	})
	if res.Ok {
		t.Fatal("expected connection refused to be reported as a failure")
	}
}
