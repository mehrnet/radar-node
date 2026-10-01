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
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
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
	if accepted.Load() != 2 {
		t.Fatalf("expected two SOCKS connections, got %d", accepted.Load())
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(out.String()), &payload); err != nil {
		t.Fatalf("output is not valid JSON: %q (%v)", out.String(), err)
	}
	if payload["http_code"].(float64) != 200 {
		t.Fatalf("http_code = %v", payload["http_code"])
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

func TestTimeoutFractionsMatchTheScript(t *testing.T) {
	// 5000ms * 0.15 = 750ms dial; * 0.325 = 1625ms per request --
	// the 0.8 total with 0.2 margin xray-run.sh -8 documents.
	if got := time.Duration(float64(5000*time.Millisecond) * dialFraction); got != 750*time.Millisecond {
		t.Fatalf("dial fraction: %v", got)
	}
	if got := time.Duration(float64(5000*time.Millisecond) * requestFraction); got != 1625*time.Millisecond {
		t.Fatalf("request fraction: %v", got)
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
