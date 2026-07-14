// Package icmp implements an ICMP echo (ping) check.
//
// Uses an unprivileged "udp4" ICMP socket, which on Linux requires
// the process's group to be within net.ipv4.ping_group_range -- no
// CAP_NET_RAW or root needed once that sysctl is set. IPv6 targets
// are not yet supported.
//
// This opens one socket per check call. The concurrent scheduler
// (not yet built) should share a single long-lived icmp.PacketConn
// across all in-flight pings, dispatching replies to pending checks
// by sequence number via a background reader goroutine -- opening a
// socket per ping does not scale to hundreds of concurrent probes.
package icmp

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"

	"github.com/mehrnet/radar-node/internal/probe"
)

type Checker struct{}

func New() Checker { return Checker{} }

func (Checker) Type() string { return "icmp" }

func (c Checker) Check(ctx context.Context, opts probe.Options) probe.Result {
	dst, err := net.ResolveIPAddr("ip4", opts.Target)
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, err)
	}

	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq,
			fmt.Errorf("open icmp socket (is net.ipv4.ping_group_range configured?): %w", err))
	}
	defer conn.Close()

	seq := opts.Seq
	if seq == 0 {
		seq = 1
	}
	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   os.Getpid() & 0xffff,
			Seq:  seq,
			Data: []byte("radar-node"),
		},
	}
	wb, err := msg.Marshal(nil)
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, err)
	}

	deadline := time.Now().Add(opts.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	start := time.Now()
	if _, err := conn.WriteTo(wb, &net.UDPAddr{IP: dst.IP}); err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, err)
	}

	rb := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(rb)
		if err != nil {
			return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, err)
		}
		elapsed := time.Since(start)

		reply, err := icmp.ParseMessage(1, rb[:n])
		if err != nil {
			return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, err)
		}

		echo, ok := reply.Body.(*icmp.Echo)
		if reply.Type != ipv4.ICMPTypeEchoReply || !ok || echo.Seq != seq {
			// Not the reply we're waiting for; keep reading until
			// the deadline fires and ReadFrom returns a timeout.
			continue
		}

		return probe.Ok(c.Type(), opts.Target, opts.Mode, opts.Seq, elapsed, map[string]any{
			"resolved_ip": dst.IP.String(),
		})
	}
}
