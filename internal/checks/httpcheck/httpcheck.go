// Package httpcheck implements an HTTP(S) check with a curl-style
// timing breakdown (DNS, connect, TLS, TTFB).
package httpcheck

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"github.com/mehrnet/radar-node/internal/probe"
)

// Checker holds one shared, keep-alive Transport reused across every
// Check() call so the warm half of each check (see below) benefits
// from a connection a previous tick already established, instead of
// paying a fresh TLS handshake every time.
type Checker struct {
	warm *http.Transport
}

func New() *Checker {
	return &Checker{warm: baseTransport(false)}
}

func (*Checker) Type() string { return "http" }

func baseTransport(disableKeepAlives bool) *http.Transport {
	return &http.Transport{
		DisableKeepAlives:   disableKeepAlives,
		MaxIdleConnsPerHost: 4,
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

// roundTripResult carries one request's own timing breakdown --
// separate from probe.Result since a Check() call now makes two of
// these (see Check's own comment) and only combines them afterward.
type roundTripResult struct {
	httpCode                        int
	dnsMs, connectMs, tlsMs, ttfbMs float64
	bytes                           int64
	elapsed                         time.Duration
}

// Check performs an HTTP(S) request against opts.Target twice -- once
// on a fresh, non-pooled transport (a true cold path: no reused
// connection, no cached TLS session) and once on the Checker's own
// shared keep-alive transport (a realistic steady-state path, warmed
// further by every previous tick's own request) -- and reports both
// sets of numbers in a single result, rather than a single number
// whose meaning silently depended on the probe's own Mode. Mode is
// still accepted (opts.Mode) and stamped onto the result like every
// other prober does, but no longer changes what this check actually
// does.
//
// Supported params:
//
//	method: HTTP method, default GET
func (c *Checker) Check(ctx context.Context, opts probe.Options) probe.Result {
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	url := opts.Target
	if !strings.Contains(url, "://") {
		url = "https://" + url
	}
	method := opts.Param("method", "GET")

	cold, err := c.roundTrip(ctx, baseTransport(true), method, url)
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, err)
	}
	warm, err := c.roundTrip(ctx, c.warm, method, url)
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, err)
	}

	extra := map[string]any{
		"http_code":    warm.httpCode,
		"cold_ttfb_ms": cold.ttfbMs,
		"warm_ttfb_ms": warm.ttfbMs,
		// The full breakdown is only meaningful on the cold request --
		// a reused warm connection skips DNS/connect/TLS entirely
		// (near-zero, not a real measurement) once a previous tick has
		// already warmed the pool.
		"dns_ms":     cold.dnsMs,
		"connect_ms": cold.connectMs,
		"tls_ms":     cold.tlsMs,
		"bytes":      warm.bytes,
	}

	// latency_ms (the universal default headline every prober reports)
	// stays the warm figure -- the "realistic steady-state" number
	// warm mode always favored by default (see probe.Mode's own doc
	// comment), so existing history/alerting built on it keeps meaning
	// what it always meant rather than silently doubling in scope.
	result := probe.Ok(c.Type(), opts.Target, opts.Mode, opts.Seq, warm.elapsed, extra)
	if bad := badStatusCode(cold.httpCode, warm.httpCode); bad != 0 {
		result.Ok = false
		result.Error = fmt.Sprintf("http %d", bad)
	}
	return result
}

func badStatusCode(codes ...int) int {
	for _, code := range codes {
		if code >= 500 {
			return code
		}
	}
	return 0
}

func (c *Checker) roundTrip(ctx context.Context, transport *http.Transport, method, url string) (roundTripResult, error) {
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return roundTripResult{}, err
	}

	var (
		start, dnsStart, connectStart, tlsStart time.Time
		dnsMs, connectMs, tlsMs, ttfbMs         float64
	)
	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone: func(httptrace.DNSDoneInfo) {
			if !dnsStart.IsZero() {
				dnsMs = ms(time.Since(dnsStart))
			}
		},
		ConnectStart: func(string, string) { connectStart = time.Now() },
		ConnectDone: func(string, string, error) {
			if !connectStart.IsZero() {
				connectMs = ms(time.Since(connectStart))
			}
		},
		TLSHandshakeStart: func() { tlsStart = time.Now() },
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			if !tlsStart.IsZero() {
				tlsMs = ms(time.Since(tlsStart))
			}
		},
		GotFirstResponseByte: func() { ttfbMs = ms(time.Since(start)) },
	}
	req = req.WithContext(httptrace.WithClientTrace(ctx, trace))

	start = time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return roundTripResult{}, err
	}
	defer resp.Body.Close()
	// The body must be fully drained (not just closed) or the
	// underlying connection cannot be returned to the pool -- losing
	// this silently defeats warm-mode reuse entirely.
	drained, _ := io.Copy(io.Discard, resp.Body)
	elapsed := time.Since(start)

	return roundTripResult{
		httpCode:  resp.StatusCode,
		dnsMs:     dnsMs,
		connectMs: connectMs,
		tlsMs:     tlsMs,
		ttfbMs:    ttfbMs,
		bytes:     drained,
		elapsed:   elapsed,
	}, nil
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
