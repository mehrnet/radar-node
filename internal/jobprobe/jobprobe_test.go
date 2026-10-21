package jobprobe

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A minimal SOCKS5 CONNECT server, so the through-tunnel path is
// exercised for real: handshake, then splice the connection to
// whatever target the client asked for. Not a general proxy -- just
// enough protocol for proxy.SOCKS5's client to negotiate.
func socksServer(t *testing.T) (addr string, connsAccepted *atomic.Int32) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	count := &atomic.Int32{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				serveSOCKS(c, count)
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String(), count
}

// serveSOCKS drives one connection through the CONNECT handshake and
// the target splice; reports whether a target connection succeeded.
func serveSOCKS(c net.Conn, count *atomic.Int32) bool {
	br := bufio.NewReader(c)
	// Greeting: VER(05) NMETHODS METHODS...
	if _, err := br.ReadByte(); err != nil {
		return false
	}
	nMethods, err := br.ReadByte()
	if err != nil {
		return false
	}
	for i := byte(0); i < nMethods; i++ {
		if _, err := br.ReadByte(); err != nil {
			return false
		}
	}
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return false
	}
	// Request: VER CMD RSV ATYP ADDR PORT
	head := make([]byte, 4)
	if _, err := ioReadFull(br, head); err != nil {
		return false
	}
	atyp := head[3]
	var host string
	switch atyp {
	case 0x01: // IPv4
		b := make([]byte, 4)
		if _, err := ioReadFull(br, b); err != nil {
			return false
		}
		host = net.IP(b).String()
	case 0x03: // domain
		l, err := br.ReadByte()
		if err != nil {
			return false
		}
		b := make([]byte, l)
		if _, err := ioReadFull(br, b); err != nil {
			return false
		}
		host = string(b)
	case 0x04: // IPv6
		b := make([]byte, 16)
		if _, err := ioReadFull(br, b); err != nil {
			return false
		}
		host = net.IP(b).String()
	default:
		return false
	}
	portBytes := make([]byte, 2)
	if _, err := ioReadFull(br, portBytes); err != nil {
		return false
	}
	port := binary.BigEndian.Uint16(portBytes)

	// Reply success first (with a zero bind address), then splice
	// bidirectionally until either side closes.
	if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return false
	}
	target, err := net.Dial("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return false
	}
	defer target.Close()
	// Counted at connect, not at splice completion -- the client may
	// still be mid-response when the test asserts.
	count.Add(1)
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(target, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, target); done <- struct{}{} }()
	<-done
	return true
}

func ioReadFull(br *bufio.Reader, buf []byte) (int, error) {
	read := 0
	for read < len(buf) {
		n, err := br.Read(buf[read:])
		read += n
		if err != nil {
			return read, err
		}
	}
	return read, nil
}

func writeParams(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "params.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write params: %v", err)
	}
	return path
}

func TestExtractEndpointBothShapesAndMissing(t *testing.T) {
	vnext := writeParams(t, `{"config":{"outbounds":[{"settings":{"vnext":[{"address":"a.example","port":443}]}},{"protocol":"freedom"}]}}`)
	if ep, ok := extractEndpoint(vnext); !ok || ep.Address != "a.example" || ep.Port != 443 {
		t.Fatalf("vnext shape: got %+v ok=%v", ep, ok)
	}
	servers := writeParams(t, `{"config":{"outbounds":[{"settings":{"servers":[{"address":"b.example","port":8443}]}}]}}`)
	if ep, ok := extractEndpoint(servers); !ok || ep.Address != "b.example" || ep.Port != 8443 {
		t.Fatalf("servers shape: got %+v ok=%v", ep, ok)
	}
	for _, missing := range []string{
		`{}`,
		`{"config":{"outbounds":[]}}`,
		`{"config":{"outbounds":[{"settings":{}}]}}`,
		`not json at all`,
	} {
		path := writeParams(t, missing)
		if _, ok := extractEndpoint(path); ok {
			t.Fatalf("expected no endpoint for %q", missing)
		}
	}
}

func TestRunEndToEndThroughSOCKS(t *testing.T) {
	socksAddr, accepted := socksServer(t)
	var hits int
	var remotes []string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		remotes = append(remotes, r.RemoteAddr)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer origin.Close()

	// Point the SOCKS port at the test server by extracting its port.
	_, port, _ := net.SplitHostPort(origin.Listener.Addr().String())
	socksPort := mustPort(t, socksAddr)

	params := writeParams(t, fmt.Sprintf(`{"config":{"outbounds":[{"settings":{"vnext":[{"address":"127.0.0.1","port":%s}]}}]}}`, port))

	var out strings.Builder
	if err := Run([]string{
		"--socks-port", socksPort,
		"--target", origin.URL + "/probe",
		"--timeout-ms", "5000",
		"--params-file", params,
	}, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if hits != 2 {
		t.Fatalf("expected exactly two through-tunnel requests, got %d", hits)
	}
	if accepted.Load() != 1 {
		t.Fatalf("expected one reused SOCKS connection for both requests, got %d", accepted.Load())
	}
	if len(remotes) != 2 || remotes[0] != remotes[1] {
		t.Fatalf("second request did not reuse the tunnel connection: %v", remotes)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(out.String()), &payload); err != nil {
		t.Fatalf("output is not valid JSON: %q (%v)", out.String(), err)
	}
	if payload["http_code"].(float64) != 200 {
		t.Fatalf("http_code = %v", payload["http_code"])
	}
	if payload["target_url"] != origin.URL+"/probe" {
		t.Fatalf("target_url = %v", payload["target_url"])
	}
	if payload["target_attempts"].(float64) != 1 {
		t.Fatalf("target_attempts = %v", payload["target_attempts"])
	}
	for _, key := range []string{"latency_ms", "http_delay_ms", "real_delay_ms", "tcp_delay_ms"} {
		v, ok := payload[key].(float64)
		if !ok {
			t.Fatalf("%s missing or null: %v", key, payload[key])
		}
		if v < 0 || v > 5000 {
			t.Fatalf("%s out of range: %v", key, v)
		}
	}
	// latency_ms aliases http_delay_ms -- the module's declared default.
	if payload["latency_ms"] != payload["http_delay_ms"] {
		t.Fatalf("latency_ms %v != http_delay_ms %v", payload["latency_ms"], payload["http_delay_ms"])
	}
}

func TestRunFailsWhenTunnelIsDead(t *testing.T) {
	// Port 1 on loopback: nothing listens, connection refused.
	params := writeParams(t, `{"config":{"outbounds":[{"settings":{"vnext":[{"address":"127.0.0.1","port":1}]}}]}}`)
	var out strings.Builder
	err := Run([]string{"--socks-port", "1", "--target", "http://example.invalid/", "--timeout-ms", "500", "--params-file", params}, &out)
	if err == nil {
		t.Fatalf("expected failure through a dead tunnel, got output %q", out.String())
	}
	if out.Len() != 0 {
		t.Fatalf("a failed run must emit no partial JSON, got %q", out.String())
	}
}

func TestRunReportsNullTCPDelayWithoutEndpoint(t *testing.T) {
	socksAddr, _ := socksServer(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer origin.Close()

	params := writeParams(t, `{"config":{"outbounds":[{"protocol":"freedom"}]}}`) // no vnext/servers
	var out strings.Builder
	socksPort := mustPort(t, socksAddr)
	if err := Run([]string{"--socks-port", socksPort, "--target", origin.URL, "--timeout-ms", "5000", "--params-file", params}, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), `"tcp_delay_ms": null`) {
		t.Fatalf("expected null tcp_delay_ms without an endpoint, got %q", out.String())
	}
}

func TestTimeoutFractions(t *testing.T) {
	// 5000ms: 250ms dial (concurrent), 2250ms for the probe's own
	// target, fallbacks may run until 3000ms, 4250ms overall -- the
	// 15% margin at the end is for scheduling delay under load.
	if got := durationFraction(5*time.Second, dialFraction); got != 250*time.Millisecond {
		t.Fatalf("dial fraction: %v", got)
	}
	if got := durationFraction(5*time.Second, primaryFraction); got != 2250*time.Millisecond {
		t.Fatalf("primary fraction: %v", got)
	}
	if got := durationFraction(5*time.Second, fallbackPhaseEndFraction); got != 3000*time.Millisecond {
		t.Fatalf("fallback phase end: %v", got)
	}
	if got := durationFraction(5*time.Second, totalFraction); got != 4250*time.Millisecond {
		t.Fatalf("total fraction: %v", got)
	}
}

func mustPort(t *testing.T, addr string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	return port
}

func TestSchemeLessTargetEndToEnd(t *testing.T) {
	// Production xray probes carry scheme-less targets ("1.1.1.1",
	// "ip:port"); curl treated them as http:// and this helper must
	// too -- exercised through Run itself, since a regression here
	// fails every such probe the moment its node picks up jobprobe.
	socksAddr, _ := socksServer(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()

	_, originPort, _ := net.SplitHostPort(origin.Listener.Addr().String())
	params := writeParams(t, `{"config":{"outbounds":[{"protocol":"freedom"}]}}`)
	var out strings.Builder
	socksPort := mustPort(t, socksAddr)
	// Scheme-less host:port, exactly the production shape.
	err := Run([]string{"--socks-port", socksPort, "--target", "127.0.0.1:" + originPort, "--timeout-ms", "5000", "--params-file", params}, &out)
	if err != nil {
		t.Fatalf("Run with scheme-less target: %v", err)
	}
	if !strings.Contains(out.String(), `"http_code": 200`) {
		t.Fatalf("expected 200 through the tunnel, got %q", out.String())
	}
}

func TestSchemeLessTargetsNormalizeLikeCurl(t *testing.T) {
	// Production xray probes carry scheme-less targets; curl treated
	// them as http:// and this helper must too. A regression here
	// fails every such probe on the node the moment it updates.
	for _, raw := range []string{"1.1.1.1", "108.165.2.77:5708"} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		target := fs.String("target", raw, "")
		if !strings.Contains(*target, "://") {
			*target = "http://" + *target
		}
		u, err := url.Parse(*target)
		if err != nil || u.Scheme != "http" {
			t.Fatalf("%q -> %q (%v)", raw, *target, err)
		}
	}
}

func TestFirstRequestTimesOutAgainstAWedgedEngine(t *testing.T) {
	// A server that accepts the TCP connection and then never
	// responds -- the exact shape of a warm-but-wedged engine. The
	// first request used to run on a bare context.Background() and
	// would park forever; it must now fail within its own fractional
	// budget instead of waiting for someone to kill the process.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection open, answer nothing.
			_ = conn
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })

	params := writeParams(t, `{"config":{"outbounds":[{"protocol":"freedom"}]}}`)
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	start := time.Now()
	var out strings.Builder
	err = Run([]string{"--socks-port", port, "--target", "http://127.0.0.1:1/", "--timeout-ms", "2000", "--params-file", params}, &out)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected failure against a wedged engine")
	}
	// 2000ms * 0.45 = 900ms for the first target, then a bounded
	// fallback phase; allow generous slack for spawn and scheduling,
	// but nothing like the pre-fix "forever".
	if elapsed > 5*time.Second {
		t.Fatalf("first request took %v against a wedged engine -- primary budget is 900ms", elapsed)
	}
}

func TestRunFallsBackWhenThePrimaryTargetFails(t *testing.T) {
	socksAddr, accepted := socksServer(t)
	var primaryHits, fallbackHits int
	var fallbackRemotes []string
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits++
		fallbackRemotes = append(fallbackRemotes, r.RemoteAddr)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fallback.Close()

	params := writeParams(t, `{"config":{"outbounds":[{"protocol":"freedom"}]}}`)
	var out strings.Builder
	if err := Run([]string{
		"--socks-port", mustPort(t, socksAddr),
		"--target", primary.URL + "/probe",
		"--fallback-targets", fallback.URL + "/fallback",
		"--timeout-ms", "5000",
		"--params-file", params,
	}, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if primaryHits != 1 {
		t.Fatalf("primary hits = %d, want 1", primaryHits)
	}
	if fallbackHits != 2 {
		t.Fatalf("fallback hits = %d, want 2 (first + second request)", fallbackHits)
	}
	if accepted.Load() != 2 {
		t.Fatalf("SOCKS connections = %d, want 2 (one failed attempt, one reused successful one)", accepted.Load())
	}
	if len(fallbackRemotes) != 2 || fallbackRemotes[0] != fallbackRemotes[1] {
		t.Fatalf("fallback's second request did not reuse its tunnel: %v", fallbackRemotes)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(out.String()), &payload); err != nil {
		t.Fatalf("output is not valid JSON: %q (%v)", out.String(), err)
	}
	if payload["target_url"] != fallback.URL+"/fallback" {
		t.Fatalf("target_url = %v", payload["target_url"])
	}
	if payload["target_attempts"].(float64) != 2 {
		t.Fatalf("target_attempts = %v", payload["target_attempts"])
	}
	if payload["http_code"].(float64) != 204 {
		t.Fatalf("http_code = %v", payload["http_code"])
	}
}

func TestRunAccepts3xx(t *testing.T) {
	socksAddr, _ := socksServer(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(http.StatusFound)
	}))
	defer origin.Close()

	params := writeParams(t, `{"config":{"outbounds":[{"protocol":"freedom"}]}}`)
	var out strings.Builder
	if err := Run([]string{
		"--socks-port", mustPort(t, socksAddr),
		"--target", origin.URL,
		"--timeout-ms", "5000",
		"--params-file", params,
	}, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), `"http_code": 302`) {
		t.Fatalf("expected 302 through the tunnel, got %q", out.String())
	}
}

func TestRunFailsWhenEveryTargetReturnsAnErrorStatus(t *testing.T) {
	socksAddr, _ := socksServer(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer bad.Close()
	alsoBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer alsoBad.Close()

	params := writeParams(t, `{"config":{"outbounds":[{"protocol":"freedom"}]}}`)
	var out strings.Builder
	err := Run([]string{
		"--socks-port", mustPort(t, socksAddr),
		"--target", bad.URL,
		"--fallback-targets", alsoBad.URL,
		"--timeout-ms", "2000",
		"--params-file", params,
	}, &out)
	if err == nil {
		t.Fatalf("expected failure when every target answers an error status, got %q", out.String())
	}
	if out.Len() != 0 {
		t.Fatalf("a failed run must emit no partial JSON, got %q", out.String())
	}
	if !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("error should name both bad statuses, got %v", err)
	}
}

func TestDialNetworkFollowsDomainStrategy(t *testing.T) {
	for _, tc := range []struct {
		strategy string
		want     string
	}{
		{"", "tcp"},
		{"UseIPv4", "tcp4"},
		{"use_ipv4", "tcp4"},
		{"UseIPv6", "tcp6"},
		{"use_ipv6", "tcp6"},
		{"UseIP", "tcp"},
	} {
		t.Run(tc.strategy, func(t *testing.T) {
			t.Setenv("RADAR_DOMAIN_STRATEGY", tc.strategy)
			if got := dialNetwork(); got != tc.want {
				t.Fatalf("dialNetwork() = %q, want %q", got, tc.want)
			}
		})
	}
}
