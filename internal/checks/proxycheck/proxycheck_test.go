package proxycheck_test

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/checks/proxycheck"
	"github.com/mehrnet/radar-node/internal/probe"
)

// startSOCKS5Server is a minimal RFC 1928 (+ RFC 1929 auth) server --
// just enough to prove the checker's own dialer really speaks SOCKS5,
// not a general-purpose implementation. CONNECT only, IPv4/domain/IPv6
// address types, no-auth or username/password subnegotiation.
func startSOCKS5Server(t *testing.T, user, pass string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	requireAuth := user != "" || pass != ""
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSOCKS5Conn(conn, requireAuth, user, pass)
		}
	}()
	return ln.Addr().String()
}

func handleSOCKS5Conn(conn net.Conn, requireAuth bool, user, pass string) {
	defer conn.Close()
	buf := make([]byte, 262)

	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return
	}
	nmethods := int(buf[1])
	if _, err := io.ReadFull(conn, buf[:nmethods]); err != nil {
		return
	}
	method := byte(0x00)
	if requireAuth {
		method = 0x02
	}
	if _, err := conn.Write([]byte{0x05, method}); err != nil {
		return
	}

	if requireAuth {
		if _, err := io.ReadFull(conn, buf[:2]); err != nil {
			return
		}
		ulen := int(buf[1])
		uname := make([]byte, ulen)
		if _, err := io.ReadFull(conn, uname); err != nil {
			return
		}
		if _, err := io.ReadFull(conn, buf[:1]); err != nil {
			return
		}
		plen := int(buf[0])
		passwd := make([]byte, plen)
		if _, err := io.ReadFull(conn, passwd); err != nil {
			return
		}
		if string(uname) != user || string(passwd) != pass {
			_, _ = conn.Write([]byte{0x01, 0x01})
			return
		}
		if _, err := conn.Write([]byte{0x01, 0x00}); err != nil {
			return
		}
	}

	if _, err := io.ReadFull(conn, buf[:4]); err != nil {
		return
	}
	atyp := buf[3]
	var host string
	switch atyp {
	case 0x01:
		addr := make([]byte, 4)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return
		}
		host = net.IP(addr).String()
	case 0x03:
		if _, err := io.ReadFull(conn, buf[:1]); err != nil {
			return
		}
		name := make([]byte, int(buf[0]))
		if _, err := io.ReadFull(conn, name); err != nil {
			return
		}
		host = string(name)
	case 0x04:
		addr := make([]byte, 16)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return
		}
		host = net.IP(addr).String()
	default:
		return
	}
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return
	}
	port := int(portBuf[0])<<8 | int(portBuf[1])
	target := net.JoinHostPort(host, strconv.Itoa(port))

	upstream, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}

	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, conn); done <- struct{}{} }()
	go func() { io.Copy(conn, upstream); done <- struct{}{} }()
	<-done
}

// startHTTPProxyServer fakes just enough of a plain (non-CONNECT)
// forward HTTP proxy to test attemptHTTPProxy against real requests:
// a plain http:// target arrives with an absolute-form request URI,
// which this handler re-issues itself and streams the response back.
func startHTTPProxyServer(t *testing.T, user, pass string) *httptest.Server {
	t.Helper()
	requireAuth := user != "" || pass != ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requireAuth {
			gotUser, gotPass, ok := parseProxyAuth(r.Header.Get("Proxy-Authorization"))
			if !ok || gotUser != user || gotPass != pass {
				w.WriteHeader(http.StatusProxyAuthRequired)
				return
			}
		}
		resp, err := http.Get(r.URL.String())
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Request.BasicAuth only ever reads the "Authorization" header, never
// "Proxy-Authorization" -- net/http's own Transport sets the latter
// for a proxy, so this test double has to decode it itself.
func parseProxyAuth(header string) (user, pass string, ok bool) {
	const prefix = "Basic "
	if !strings.HasPrefix(header, prefix) {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(header[len(prefix):])
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func newTestTargetServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheck_SOCKS5_Success(t *testing.T) {
	target := newTestTargetServer(t)
	proxyAddr := startSOCKS5Server(t, "", "")

	c := proxycheck.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  proxyAddr,
		Timeout: 3 * time.Second,
		Params:  map[string]any{"protocol": "socks5", "test_url": target.URL},
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q extra %v", res.Error, res.Extra)
	}
	if _, ok := res.Extra["socks5_latency_ms"]; !ok {
		t.Fatalf("expected socks5_latency_ms in extra, got %v", res.Extra)
	}
	if res.LatencyMs == nil {
		t.Fatal("expected top-level latency_ms to be set")
	}
}

func TestCheck_SOCKS5_WrongCredentials(t *testing.T) {
	target := newTestTargetServer(t)
	proxyAddr := startSOCKS5Server(t, "realuser", "realpass")

	c := proxycheck.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  proxyAddr,
		Timeout: 3 * time.Second,
		Params:  map[string]any{"protocol": "socks5", "username": "wrong", "password": "wrong", "test_url": target.URL},
	})
	if res.Ok {
		t.Fatal("expected failure for wrong credentials")
	}
	if _, ok := res.Extra["socks5_error"]; !ok {
		t.Fatalf("expected socks5_error in extra, got %v", res.Extra)
	}
}

func TestCheck_HTTPProxy_Success(t *testing.T) {
	target := newTestTargetServer(t)
	proxySrv := startHTTPProxyServer(t, "", "")
	proxyAddr := proxySrv.Listener.Addr().String()

	c := proxycheck.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  proxyAddr,
		Timeout: 3 * time.Second,
		Params:  map[string]any{"protocol": "http", "test_url": target.URL},
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q extra %v", res.Error, res.Extra)
	}
	if _, ok := res.Extra["http_latency_ms"]; !ok {
		t.Fatalf("expected http_latency_ms in extra, got %v", res.Extra)
	}
}

func TestCheck_HTTPProxy_Auth(t *testing.T) {
	target := newTestTargetServer(t)
	proxySrv := startHTTPProxyServer(t, "realuser", "realpass")
	proxyAddr := proxySrv.Listener.Addr().String()

	c := proxycheck.New()
	resOk := c.Check(context.Background(), probe.Options{
		Target:  proxyAddr,
		Timeout: 3 * time.Second,
		Params:  map[string]any{"protocol": "http", "username": "realuser", "password": "realpass", "test_url": target.URL},
	})
	if !resOk.Ok {
		t.Fatalf("expected ok with correct credentials, got error %q", resOk.Error)
	}

	resFail := c.Check(context.Background(), probe.Options{
		Target:  proxyAddr,
		Timeout: 3 * time.Second,
		Params:  map[string]any{"protocol": "http", "username": "wrong", "password": "wrong", "test_url": target.URL},
	})
	if resFail.Ok {
		t.Fatal("expected failure for wrong credentials")
	}
}

func TestCheck_Dual_OneWorks(t *testing.T) {
	target := newTestTargetServer(t)
	// Only a SOCKS5 server is listening -- no explicit protocol param,
	// so both are attempted; SOCKS5 should succeed and HTTP should
	// fail against the very same address (it isn't speaking HTTP).
	proxyAddr := startSOCKS5Server(t, "", "")

	c := proxycheck.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  proxyAddr,
		Timeout: 3 * time.Second,
		Params:  map[string]any{"test_url": target.URL},
	})
	if !res.Ok {
		t.Fatalf("expected ok (socks5 half of the dual probe should succeed), got error %q extra %v", res.Error, res.Extra)
	}
	if _, ok := res.Extra["socks5_latency_ms"]; !ok {
		t.Fatalf("expected socks5_latency_ms, got %v", res.Extra)
	}
	if _, ok := res.Extra["http_error"]; !ok {
		t.Fatalf("expected http_error since this address doesn't speak HTTP, got %v", res.Extra)
	}
}

func TestCheck_Dual_BothFail(t *testing.T) {
	target := newTestTargetServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // closed port -- neither protocol can even connect

	c := proxycheck.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  addr,
		Timeout: 2 * time.Second,
		Params:  map[string]any{"test_url": target.URL},
	})
	if res.Ok {
		t.Fatal("expected failure when nothing is listening")
	}
	if _, ok := res.Extra["socks5_error"]; !ok {
		t.Fatal("expected socks5_error")
	}
	if _, ok := res.Extra["http_error"]; !ok {
		t.Fatal("expected http_error")
	}
}

func TestCheck_InvalidProtocol(t *testing.T) {
	c := proxycheck.New()
	res := c.Check(context.Background(), probe.Options{
		Target:  "127.0.0.1:1",
		Timeout: time.Second,
		Params:  map[string]any{"protocol": "quic"},
	})
	if res.Ok {
		t.Fatal("expected failure for an unrecognized protocol param")
	}
	if res.ErrorCode != probe.ErrorCodeInvalidParams {
		t.Fatalf("expected ErrorCodeInvalidParams, got %q", res.ErrorCode)
	}
}
