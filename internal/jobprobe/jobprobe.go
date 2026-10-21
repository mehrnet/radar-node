// Package jobprobe runs one pooled module job's whole measurement set
// in a single process. It exists to replace the eight-process chain
// xray-run.sh used to spawn per job per tick (sh + python3 + two
// curls + three awks): measured against a dead target, that chain
// costs ~33ms of child CPU per job before any real network work --
// pure process-spawn overhead, which on a node running ~100 pooled
// jobs a minute is several CPU-seconds per minute of spawning alone
// (and, on a small VPS, python3's ~20MB interpreter footprint per
// concurrent job). The module scripts call this via
// `radar-node jobprobe ...` and fall back to their old chain on nodes
// whose radar-node predates the subcommand.
//
// The measurement contract is xray-run.sh's own: three figures per
// check, matching 3x-ui's three-way split -- tcp_delay_ms (raw dial to
// the proxy server, best-effort), and real_delay_ms / http_delay_ms
// (first and second request through the job's SOCKS inbound;
// http_delay_ms is also this module's latency_ms).
//
// A check tries the probe's own target first and, if that attempt
// fails at the transport level or returns a status outside 2xx/3xx,
// falls back to a small set of neutral connectivity targets (the same
// ones control-api's O3 cellular probe uses). A blocked or broken
// target must not read as "this proxy is down" -- the tunnel itself
// may be perfectly healthy. The target that answered is reported as
// target_url.
//
// Timeout budgeting: the raw dial runs concurrently with the first
// through-tunnel attempt, so it never eats the measurement's budget.
// The probe's own target gets primaryFraction of timeout_ms; fallback
// attempts share whatever remains up to fallbackPhaseEndFraction,
// divided dynamically so a fast failure leaves more time for the next
// target, and the second request gets the rest -- all capped at
// totalFraction to leave real margin before the executor's own timeout
// SIGKILLs the job. Production observed "signal: killed" at a 95% cap
// on a node running many concurrent jobs: scheduling delay alone ate
// the remaining 5%, so the cap is deliberately back at 85% (the old
// script's own 20%-margin reasoning, adjusted for jobprobe's smaller
// spawn overhead).
package jobprobe

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// Fractions of the job's full timeout_ms -- see the package comment.
const (
	dialFraction             = 0.05
	primaryFraction          = 0.45
	fallbackPhaseEndFraction = 0.60
	totalFraction            = 0.85
)

// Neutral, tiny-response targets used when the probe's configured
// target cannot be reached or does not answer 2xx/3xx. Same set the
// O3 cellular probe tries after its own latency target.
var defaultFallbackTargets = []string{
	"https://www.google.com/generate_204",
	"https://www.youtube.com/generate_204",
	"https://www.ytimg.com/generate_204",
	"https://abs.twimg.com/favicons/favicon.ico",
}

// endpoint is the {address, port} a module job's config dials
// outbound -- the same extraction xray-run.sh's python3 block did:
// params.config.outbounds[0].settings.{vnext|servers}[0].
type endpoint struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
}

type outboundSettings struct {
	Vnext   []endpoint `json:"vnext"`
	Servers []endpoint `json:"servers"`
}

type outbound struct {
	Settings outboundSettings `json:"settings"`
}

type jobConfig struct {
	Outbounds []outbound `json:"outbounds"`
}

type jobParams struct {
	Config jobConfig `json:"config"`
}

// extractEndpoint walks a params file's JSON and returns the first
// declared vnext/servers endpoint. Absent or malformed means "no dial
// to make" -- callers report tcp_delay_ms as null, exactly as the
// python3 block's except-branch did.
func extractEndpoint(paramsPath string) (endpoint, bool) {
	raw, err := os.ReadFile(paramsPath)
	if err != nil {
		return endpoint{}, false
	}
	var params jobParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return endpoint{}, false
	}
	for _, out := range params.Config.Outbounds {
		if len(out.Settings.Vnext) > 0 && out.Settings.Vnext[0].Address != "" {
			return out.Settings.Vnext[0], true
		}
		if len(out.Settings.Servers) > 0 && out.Settings.Servers[0].Address != "" {
			return out.Settings.Servers[0], true
		}
	}
	return endpoint{}, false
}

// dialTCPDelay makes the best-effort raw dial and returns its
// milliseconds, or nil for any failure -- never fails the run.
func dialTCPDelay(ep endpoint, timeout time.Duration) *float64 {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ep.Address, fmt.Sprint(ep.Port)), timeout)
	if err != nil {
		return nil
	}
	_ = conn.Close()
	ms := float64(time.Since(start)) / float64(time.Millisecond)
	return &ms
}

// requestThroughSOCKS performs one GET through the job's SOCKS
// inbound, returning (elapsed, httpCode). Elapsed uses curl's
// time_total semantics -- connect through the tunnel plus response
// headers plus the fully-drained body -- since that is what the
// writeout figures the module reports were always measured against.
func requestThroughSOCKS(ctx context.Context, socksAddr, target string) (time.Duration, int, error) {
	dialer, err := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
	if err != nil {
		return 0, 0, fmt.Errorf("socks dialer: %w", err)
	}
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return 0, 0, errors.New("socks dialer does not support contexts")
	}
	transport := &http.Transport{DialContext: contextDialer.DialContext, Proxy: nil}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, 0, err
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return 0, 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return time.Since(start), resp.StatusCode, nil
}

// jsonNumber renders a float with the same %.3f shape the awk
// conversions produced, so stored values are byte-identical in
// precision to the script's. nil-safe for the best-effort dial.
func jsonNumber(v *float64) json.RawMessage {
	if v == nil {
		return json.RawMessage("null")
	}
	return json.RawMessage(fmt.Sprintf("%.3f", *v))
}

// Run is the jobprobe entry point: parses args, runs the three
// measurements, prints the writeout JSON to stdout. A failure of
// either through-tunnel request fails the whole run non-zero --
// the same set -e behaviour xray-run.sh had, where a curl failure
// meant the step itself failed and no partial JSON was emitted.
func Run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("jobprobe", flag.ContinueOnError)
	socksPort := fs.Int("socks-port", 0, "the job's allocated SOCKS inbound port")
	target := fs.String("target", "", "URL to request through the tunnel")
	timeoutMs := fs.Int("timeout-ms", 5000, "the job's full timeout budget in milliseconds")
	paramsFile := fs.String("params-file", "", "path to the job's params JSON")
	fallbackTargets := fs.String("fallback-targets", "", "comma-separated targets to try when the primary fails (default: built-in neutral set)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *socksPort <= 0 || *target == "" || *paramsFile == "" {
		return errors.New("jobprobe: --socks-port, --target and --params-file are all required")
	}
	// Production xray probes carry scheme-less targets ("1.1.1.1",
	// "108.165.2.77:5708") alongside full URLs. curl, which this
	// replaces, silently treats a scheme-less target as http:// --
	// Go's request builder rejects it instead. Normalize exactly the
	// way curl did, or every scheme-less-target probe would fail the
	// moment its node picked up the jobprobe path.
	if !strings.Contains(*target, "://") {
		*target = "http://" + *target
	}

	timeout := time.Duration(*timeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	socksAddr := fmt.Sprintf("127.0.0.1:%d", *socksPort)

	start := time.Now()
	overallDeadline := start.Add(durationFraction(timeout, totalFraction))
	fallbackDeadline := start.Add(durationFraction(timeout, fallbackPhaseEndFraction))

	// The raw dial is diagnostic only; run it alongside the first
	// through-tunnel attempt so a slow dial cannot consume the
	// measurement's own budget.
	dialDone := make(chan *float64, 1)
	if ep, ok := extractEndpoint(*paramsFile); ok {
		go func() { dialDone <- dialTCPDelay(ep, durationFraction(timeout, dialFraction)) }()
	} else {
		dialDone <- nil
	}

	fallbacks := defaultFallbackTargets
	if *fallbackTargets != "" {
		fallbacks = nil
		for _, entry := range strings.Split(*fallbackTargets, ",") {
			if trimmed := strings.TrimSpace(entry); trimmed != "" {
				fallbacks = append(fallbacks, trimmed)
			}
		}
	}
	targets := probeTargets(*target, fallbacks)

	attempts := make([]targetAttempt, 0, len(targets))
	var successTarget string
	var realDelay float64
	for i, candidate := range targets {
		var attemptTimeout time.Duration
		if i == 0 {
			attemptTimeout = durationFraction(timeout, primaryFraction)
		} else {
			remaining := time.Until(fallbackDeadline)
			if remaining <= 0 {
				break
			}
			// Divide what is left evenly among the fallbacks still to
			// try: a fast failure leaves each later target more time
			// than a slow one does.
			attemptTimeout = remaining / time.Duration(len(targets)-i)
		}
		if attemptTimeout <= 0 {
			break
		}
		ctx, cancel := context.WithTimeout(context.Background(), attemptTimeout)
		elapsed, code, err := requestThroughSOCKS(ctx, socksAddr, candidate)
		cancel()
		if err == nil && code >= 200 && code < 400 {
			successTarget = candidate
			realDelay = float64(elapsed) / float64(time.Millisecond)
			break
		}
		if err == nil {
			err = fmt.Errorf("target returned HTTP %d", code)
		}
		attempts = append(attempts, targetAttempt{target: candidate, err: err})
	}
	tcpDelay := <-dialDone
	if successTarget == "" {
		return fmt.Errorf("jobprobe: %s", formatAttempts(attempts))
	}

	// The second request gets whatever remains before the overall
	// deadline, with a small floor so a nearly-exhausted fallback
	// phase does not reduce it to zero (the executor's own timeout
	// still bounds the process).
	remaining := time.Until(overallDeadline)
	if floor := durationFraction(timeout, 0.1); remaining < floor {
		remaining = floor
	}
	ctx, cancel := context.WithTimeout(context.Background(), remaining)
	defer cancel()
	httpElapsed, httpCode, err := requestThroughSOCKS(ctx, socksAddr, successTarget)
	if err != nil {
		return fmt.Errorf("jobprobe: second request through tunnel (%s): %w", successTarget, err)
	}
	httpDelay := float64(httpElapsed) / float64(time.Millisecond)
	encodedTarget, _ := json.Marshal(successTarget)

	// Field order matches xray-run.sh's own printf so diffs between
	// node versions running either implementation stay readable;
	// latency_ms aliases http_delay_ms, the module's declared default.
	_, err = fmt.Fprintf(stdout,
		`{"latency_ms": %s, "http_code": %d, "http_delay_ms": %s, "real_delay_ms": %s, "tcp_delay_ms": %s, "target_url": %s, "target_attempts": %d}`+"\n",
		jsonNumber(&httpDelay), httpCode, jsonNumber(&httpDelay), jsonNumber(&realDelay), jsonNumber(tcpDelay), encodedTarget, len(attempts)+1)
	return err
}

type targetAttempt struct {
	target string
	err    error
}

func durationFraction(total time.Duration, fraction float64) time.Duration {
	return time.Duration(float64(total) * fraction)
}

func probeTargets(primary string, fallbacks []string) []string {
	targets := []string{primary}
	seen := map[string]bool{primary: true}
	for _, target := range fallbacks {
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		targets = append(targets, target)
	}
	return targets
}

func formatAttempts(attempts []targetAttempt) string {
	parts := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		parts = append(parts, fmt.Sprintf("%s: %v", attempt.target, attempt.err))
	}
	message := "all targets failed (" + strings.Join(parts, "; ") + ")"
	if len(message) > 600 {
		message = message[:600] + "..."
	}
	return message
}
