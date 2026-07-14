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

// Checker holds one shared, keep-alive Transport used for warm-mode
// probes so repeated checks in the same run reuse connections
// instead of paying a fresh TLS handshake every time.
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

// Check performs an HTTP(S) request against opts.Target. Supported
// params:
//
//	method: HTTP method, default GET
func (c *Checker) Check(ctx context.Context, opts probe.Options) probe.Result {
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	url := opts.Target
	if !strings.Contains(url, "://") {
		url = "https://" + url
	}

	transport := c.warm
	if opts.Mode == probe.ModeHard {
		// A fresh, non-pooled transport each call: no reused
		// connections, no cached TLS session -- a true cold path.
		transport = baseTransport(true)
	}
	client := &http.Client{Transport: transport}

	method := opts.Param("method", "GET")
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, err)
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
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, err)
	}
	defer resp.Body.Close()
	// The body must be fully drained (not just closed) or the
	// underlying connection cannot be returned to the pool -- losing
	// this silently defeats warm-mode reuse entirely.
	drained, _ := io.Copy(io.Discard, resp.Body)
	elapsed := time.Since(start)

	extra := map[string]any{
		"http_code":  resp.StatusCode,
		"dns_ms":     dnsMs,
		"connect_ms": connectMs,
		"tls_ms":     tlsMs,
		"ttfb_ms":    ttfbMs,
		"bytes":      drained,
	}

	result := probe.Ok(c.Type(), opts.Target, opts.Mode, opts.Seq, elapsed, extra)
	if resp.StatusCode >= 500 {
		result.Ok = false
		result.Error = fmt.Sprintf("http %d", resp.StatusCode)
	}
	return result
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
