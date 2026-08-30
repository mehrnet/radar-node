// Package proxycheck implements a real SOCKS5/HTTP-CONNECT proxy
// check -- dials opts.Target as an actual proxy and fetches test_url
// *through* it, rather than a bare TCP connect. See tcp.go's own
// checker for the "is something listening" version of this; this one
// answers "can a real client actually use this as a proxy."
//
// Deliberately a native Go action, not a run:-based custom module the
// way xray/wireguard/openvpn are (see internal/action's own package
// comment on that split): those need a whole tunnel engine process,
// while a SOCKS5/HTTP-CONNECT handshake is one TCP connection and a
// short protocol exchange -- the same complexity class as tcp/http
// themselves, and golang.org/x/net/proxy plus net/http's own built-in
// HTTP-proxy support cover both without hand-rolling wire bytes.
package proxycheck

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"

	"github.com/mehrnet/radar-node/internal/probe"
)

// defaultTestURL is what gets fetched *through* the proxy to prove it
// actually relays traffic, not just accepts a connection -- a proxy
// discovered from a plain host:port(:user:pass) list has no natural
// "next hop" of its own the way an http/tcp check's own target is
// already the thing being measured. generate_204's own tiny, fast,
// globally-available response is the same one Android/ChromeOS use
// for their own captive-portal connectivity checks.
const defaultTestURL = "https://www.gstatic.com/generate_204"

type Checker struct{}

func New() Checker { return Checker{} }

func (Checker) Type() string { return "proxy" }

// Check attempts opts.Target as a proxy and fetches test_url through
// it.
//
// Supported params:
//
//	protocol: "socks5" or "http" -- when set, only that protocol is
//	  attempted. When unset (the common case for a plain host:port
//	  list entry with no scheme to say which it is), BOTH are
//	  attempted and both results are reported, rather than guessing.
//	username, password: proxy auth, if the proxy requires it.
//	tls: "true" -- for protocol=http only, wraps the connection to the
//	  proxy itself in TLS ("HTTPS proxy", distinct from what's being
//	  fetched through it -- some paid proxy providers use this).
//	test_url: overrides defaultTestURL.
func (c Checker) Check(ctx context.Context, opts probe.Options) probe.Result {
	outerCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	protocol := opts.Param("protocol", "")
	if protocol != "" && protocol != "socks5" && protocol != "http" {
		return probe.Invalid(c.Type(), opts.Target, opts.Seq, fmt.Sprintf(`protocol: expected "socks5" or "http", got %q`, protocol))
	}
	testURL := opts.Param("test_url", defaultTestURL)
	username := opts.Param("username", "")
	password := opts.Param("password", "")
	tlsToProxy := opts.Param("tls", "") == "true"

	dual := protocol == ""
	// Splitting the shared budget in half when both protocols run
	// keeps a slow/timed-out first attempt from starving the second
	// one of its own fair share -- same "sub-step gets a fraction of
	// the overall timeout" reasoning install/modules/xray-run.sh's own
	// comment already established for this codebase.
	attemptTimeout := opts.Timeout
	if dual {
		attemptTimeout = opts.Timeout / 2
	}

	extra := map[string]any{}
	var anyOk bool
	var best *float64
	var firstErr error

	note := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	if dual || protocol == "socks5" {
		attemptCtx, cancel := context.WithTimeout(outerCtx, attemptTimeout)
		latency, err := attemptSOCKS5(attemptCtx, opts.Target, username, password, testURL)
		cancel()
		if err == nil {
			anyOk = true
			extra["socks5_latency_ms"] = latency
			best = pickBest(best, latency)
		} else {
			extra["socks5_error"] = err.Error()
			note(err)
		}
	}

	if dual || protocol == "http" {
		attemptCtx, cancel := context.WithTimeout(outerCtx, attemptTimeout)
		latency, err := attemptHTTPProxy(attemptCtx, opts.Target, username, password, testURL, tlsToProxy)
		cancel()
		if err == nil {
			anyOk = true
			extra["http_latency_ms"] = latency
			best = pickBest(best, latency)
		} else {
			extra["http_error"] = err.Error()
			note(err)
		}
	}

	if !anyOk {
		msg := "proxy check failed"
		if firstErr != nil {
			msg = firstErr.Error()
		}
		return probe.Result{Ok: false, Type: c.Type(), Target: opts.Target, Seq: opts.Seq, Error: msg, Extra: extra}
	}
	return probe.Result{Ok: true, Type: c.Type(), Target: opts.Target, Seq: opts.Seq, LatencyMs: best, Extra: extra}
}

func pickBest(cur *float64, v float64) *float64 {
	if cur == nil || v < *cur {
		return &v
	}
	return cur
}

func attemptSOCKS5(ctx context.Context, target, username, password, testURL string) (float64, error) {
	var auth *proxy.Auth
	if username != "" || password != "" {
		auth = &proxy.Auth{User: username, Password: password}
	}
	dialer, err := proxy.SOCKS5("tcp", target, auth, &net.Dialer{})
	if err != nil {
		return 0, err
	}
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		// Not reachable with the stdlib SOCKS5 dialer as of
		// golang.org/x/net v0.57.0, but golang.org/x/net/proxy's own
		// interface doesn't guarantee it -- fail loudly instead of
		// silently degrading to a dial with no deadline.
		return 0, fmt.Errorf("socks5 dialer does not support context cancellation")
	}
	client := &http.Client{Transport: &http.Transport{DialContext: contextDialer.DialContext}}
	return timedGet(ctx, client, testURL)
}

func attemptHTTPProxy(ctx context.Context, target, username, password, testURL string, tlsToProxy bool) (float64, error) {
	scheme := "http"
	if tlsToProxy {
		scheme = "https"
	}
	proxyURL := &url.URL{Scheme: scheme, Host: target}
	if username != "" || password != "" {
		proxyURL.User = url.UserPassword(username, password)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	return timedGet(ctx, client, testURL)
}

func timedGet(ctx context.Context, client *http.Client, testURL string) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	// A non-2xx here isn't necessarily a transport-level failure -- a
	// plain (non-CONNECT) forward proxy's own errors (407 auth
	// required, 502/504 from a dead upstream) surface as an ordinary
	// HTTP response, not a client.Do error, since as far as net/http
	// is concerned the round trip completed fine. test_url is always
	// this checker's own known-good target, so anything other than a
	// clean 2xx means the proxy hop itself didn't actually work.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("test_url returned HTTP %d", resp.StatusCode)
	}
	return ms(time.Since(start)), nil
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
