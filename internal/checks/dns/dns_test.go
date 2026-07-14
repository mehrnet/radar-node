package dns_test

import (
	"context"
	"net"
	"testing"
	"time"

	xdns "golang.org/x/net/dns/dnsmessage"

	"github.com/mehrnet/radar-node/internal/checks/dns"
	"github.com/mehrnet/radar-node/internal/probe"
)

// startFakeDNS answers every A query for name with ip, using a
// minimal in-process UDP server -- avoids depending on real DNS
// infrastructure being reachable from the test sandbox.
func startFakeDNS(t *testing.T, name string, ip [4]byte) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			var req xdns.Message
			if err := req.Unpack(buf[:n]); err != nil {
				continue
			}
			resp := xdns.Message{
				Header:    xdns.Header{ID: req.Header.ID, Response: true, Authoritative: true},
				Questions: req.Questions,
			}
			if len(req.Questions) == 1 && req.Questions[0].Type == xdns.TypeA {
				resp.Answers = []xdns.Resource{{
					Header: xdns.ResourceHeader{
						Name:  req.Questions[0].Name,
						Type:  xdns.TypeA,
						Class: xdns.ClassINET,
						TTL:   60,
					},
					Body: &xdns.AResource{A: ip},
				}}
			}
			packed, err := resp.Pack()
			if err != nil {
				continue
			}
			_, _ = conn.WriteTo(packed, addr)
		}
	}()

	return conn.LocalAddr().String()
}

func TestCheck_ResolvesAgainstCustomServer(t *testing.T) {
	server := startFakeDNS(t, "example.radar.test.", [4]byte{203, 0, 113, 42})

	c := dns.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  "example.radar.test.",
		Timeout: 2 * time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
		Params:  map[string]any{"server": server},
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	answers, _ := res.Extra["answers"].([]string)
	if len(answers) != 1 || answers[0] != "203.0.113.42" {
		t.Fatalf("expected [203.0.113.42], got %v", answers)
	}
}

func TestCheck_NoAnswerIsFailure(t *testing.T) {
	// Fake server that always returns an empty answer section.
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			var req xdns.Message
			if err := req.Unpack(buf[:n]); err != nil {
				continue
			}
			resp := xdns.Message{
				Header:    xdns.Header{ID: req.Header.ID, Response: true},
				Questions: req.Questions,
			}
			packed, _ := resp.Pack()
			_, _ = conn.WriteTo(packed, addr)
		}
	}()

	c := dns.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  "nowhere.radar.test.",
		Timeout: 2 * time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
		Params:  map[string]any{"server": conn.LocalAddr().String()},
	})
	if res.Ok {
		t.Fatal("expected failure when no A records are returned")
	}
}
