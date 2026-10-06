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
// The measurement contract is xray-run.sh's own, reproduced exactly:
// three figures per check, matching 3x-ui's three-way split --
// tcp_delay_ms (raw dial to the proxy server, best-effort), and
// real_delay_ms / http_delay_ms (first and second request through the
// job's SOCKS inbound, the second riding the mux-warmed connection;
// http_delay_ms is also this module's latency_ms). Timeout budgeting
// follows the same 0.15 + 0.325 + 0.325 = 0.8 split of timeout_ms the
// script documents -- the remaining 0.2 is margin, now process-spawn
// margin that mostly no longer exists but is kept so timing semantics
// are byte-identical to the script being replaced.
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

// Fractional split of timeout_ms across the three sequential steps --
// see the package comment. Kept as named constants so the arithmetic
// is greppable against xray-run.sh's own documented split.
const (
	dialFraction    = 0.15
	requestFraction = 0.325
)

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

	var tcpDelay *float64
	if ep, ok := extractEndpoint(*paramsFile); ok {
		tcpDelay = dialTCPDelay(ep, time.Duration(float64(timeout)*dialFraction))
	}

	// Both requests carry the same fractional budget. The first one
	// originally ran on a bare context.Background() -- unbounded -- on
	// the theory that the caller's outer timeout would kill the whole
	// process anyway. That theory has a hole a production incident
	// walked straight through: a warm engine that accepts its inbound
	// TCP connection and then never responds leaves the request parked
	// in the transport forever, and "somebody will SIGKILL me" is not
	// a timeout strategy -- it makes the caller's deadline the only
	// thing between one wedged engine and a stalled scheduler tick.
	ctx1, cancel1 := context.WithTimeout(context.Background(), time.Duration(float64(timeout)*requestFraction))
	defer cancel1()
	realElapsed, _, err := requestThroughSOCKS(ctx1, socksAddr, *target)
	if err != nil {
		return fmt.Errorf("jobprobe: first request through tunnel: %w", err)
	}
	realDelay := float64(realElapsed) / float64(time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(float64(timeout)*requestFraction))
	defer cancel()
	httpElapsed, httpCode, err := requestThroughSOCKS(ctx, socksAddr, *target)
	if err != nil {
		return fmt.Errorf("jobprobe: second request through tunnel: %w", err)
	}
	httpDelay := float64(httpElapsed) / float64(time.Millisecond)

	// Field order matches xray-run.sh's own printf so diffs between
	// node versions running either implementation stay readable;
	// latency_ms aliases http_delay_ms, the module's declared default.
	_, err = fmt.Fprintf(stdout,
		`{"latency_ms": %s, "http_code": %d, "http_delay_ms": %s, "real_delay_ms": %s, "tcp_delay_ms": %s}`+"\n",
		jsonNumber(&httpDelay), httpCode, jsonNumber(&httpDelay), jsonNumber(&realDelay), jsonNumber(tcpDelay))
	return err
}
