// Package udp implements a best-effort UDP reachability check.
//
// UDP is connectionless, so "reachable" is inherently ambiguous: most
// services silently drop unsolicited probe packets rather than reply,
// which is indistinguishable on the wire from a filtered or dead
// host. We report the one signal that *is* unambiguous -- the kernel
// surfacing an ICMP port-unreachable on a connected UDP socket as a
// read error -- as a definite failure, and otherwise report whether
// any response was received without claiming certainty either way.
package udp

import (
	"context"
	"errors"
	"net"
	"syscall"
	"time"

	"github.com/mehrnet/radar-node/internal/probe"
)

type Checker struct{}

func New() Checker { return Checker{} }

func (Checker) Type() string { return "udp" }

func (c Checker) Check(ctx context.Context, opts probe.Options) probe.Result {
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	dialer := net.Dialer{}
	start := time.Now()
	conn, err := dialer.DialContext(ctx, "udp", opts.Target)
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte{}); err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, err)
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(opts.Timeout)
	}
	_ = conn.SetReadDeadline(deadline)

	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	elapsed := time.Since(start)

	switch {
	case err == nil:
		return probe.Ok(c.Type(), opts.Target, opts.Mode, opts.Seq, elapsed, map[string]any{
			"responded":  true,
			"bytes_read": n,
		})
	case errors.Is(err, syscall.ECONNREFUSED):
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, errors.New("connection refused (icmp port unreachable)"))
	case isTimeout(err):
		// No response and no ICMP unreachable: inconclusive, not a
		// failure. The send succeeded and nothing told us otherwise.
		return probe.Result{
			Ok:     true,
			Type:   c.Type(),
			Target: opts.Target,
			Mode:   opts.Mode,
			Seq:    opts.Seq,
			Extra: map[string]any{
				"responded": false,
				"note":      "no response within timeout; many UDP services do not reply to unsolicited probes",
			},
		}
	default:
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, err)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
