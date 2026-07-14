// Package tcp implements a TCP connect check, optionally verifying a
// TLS handshake on top for TCP(s) targets that aren't HTTP (e.g. a
// generic TLS-wrapped port).
package tcp

import (
	"context"
	"crypto/tls"
	"net"
	"strconv"
	"time"

	"github.com/mehrnet/radar-node/internal/probe"
)

type Checker struct{}

func New() Checker { return Checker{} }

func (Checker) Type() string { return "tcp" }

// Check dials target (host:port) and measures connect time. TCP has
// no meaningful warm/hard distinction at the level of a single
// connect -- every dial is a fresh handshake regardless of mode, so
// Mode is recorded on the result but does not change behavior here.
//
// Supported params:
//
//	tls: "true" to also perform a TLS handshake after connecting
//	sni: server name to send (defaults to the target host)
//	insecure: "true" to skip certificate verification
func (c Checker) Check(ctx context.Context, opts probe.Options) probe.Result {
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	dialer := net.Dialer{}
	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", opts.Target)
	connectElapsed := time.Since(start)
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, err)
	}
	defer conn.Close()

	if opts.Param("tls", "") != "true" {
		return probe.Ok(c.Type(), opts.Target, opts.Mode, opts.Seq, connectElapsed, nil)
	}

	sni := opts.Param("sni", hostOnly(opts.Target))
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         sni,
		InsecureSkipVerify: opts.Param("insecure", "") == "true",
	})
	if deadline, ok := ctx.Deadline(); ok {
		_ = tlsConn.SetDeadline(deadline)
	}

	tlsStart := time.Now()
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, err)
	}
	tlsElapsed := time.Since(tlsStart)
	state := tlsConn.ConnectionState()

	extra := map[string]any{
		"connect_ms":  ms(connectElapsed),
		"tls_ms":      ms(tlsElapsed),
		"tls_version": tlsVersionName(state.Version),
	}
	if len(state.PeerCertificates) > 0 {
		extra["cert_subject"] = state.PeerCertificates[0].Subject.CommonName
		extra["cert_not_after"] = state.PeerCertificates[0].NotAfter.UTC().Format(time.RFC3339)
	}

	return probe.Ok(c.Type(), opts.Target, opts.Mode, opts.Seq, connectElapsed+tlsElapsed, extra)
}

func hostOnly(target string) string {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return target
	}
	return host
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "1.3"
	case tls.VersionTLS12:
		return "1.2"
	case tls.VersionTLS11:
		return "1.1"
	case tls.VersionTLS10:
		return "1.0"
	default:
		return "0x" + strconv.FormatUint(uint64(v), 16)
	}
}
