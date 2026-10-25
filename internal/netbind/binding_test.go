package netbind

import (
	"context"
	"errors"
	"net"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name       string
		source     string
		iface      string
		wantZero   bool
		wantSource string
		wantZone   string
		wantIface  string
		wantNet    string
		wantErr    string
	}{
		{name: "zero", wantZero: true, wantNet: "tcp"},
		{name: "ipv4", source: "127.0.0.2", wantSource: "127.0.0.2", wantNet: "tcp4"},
		{name: "ipv6", source: "2001:db8::1", wantSource: "2001:db8::1", wantNet: "tcp6"},
		{name: "mapped v4", source: "::ffff:127.0.0.2", wantSource: "127.0.0.2", wantNet: "tcp4"},
		{name: "link local zone", source: "fe80::1", iface: "eth0", wantSource: "fe80::1%eth0", wantZone: "eth0", wantIface: "eth0", wantNet: "tcp6"},
		{name: "unspecified", source: "0.0.0.0", wantErr: "concrete unicast"},
		{name: "multicast", source: "ff02::1", wantErr: "concrete unicast"},
		{name: "invalid", source: "not-an-ip", wantErr: "invalid source IP"},
		{name: "control char", iface: "eth\n0", wantErr: "control character"},
		{name: "too long", iface: strings.Repeat("a", maxInterfaceLen+1), wantErr: "longer than"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binding, err := Parse(tc.source, tc.iface)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if binding.IsZero() != tc.wantZero {
				t.Fatalf("IsZero = %v, want %v", binding.IsZero(), tc.wantZero)
			}
			if binding.SourceIP.String() != tc.wantSource && tc.wantSource != "" {
				t.Fatalf("source = %q, want %q", binding.SourceIP, tc.wantSource)
			}
			if binding.SourceIP.Zone() != tc.wantZone {
				t.Fatalf("zone = %q, want %q", binding.SourceIP.Zone(), tc.wantZone)
			}
			if binding.Interface != tc.wantIface {
				t.Fatalf("interface = %q, want %q", binding.Interface, tc.wantIface)
			}
			if got := binding.Network("tcp"); got != tc.wantNet {
				t.Fatalf("Network(tcp) = %q, want %q", got, tc.wantNet)
			}
		})
	}
}

func TestZeroBindingPreservesDefaults(t *testing.T) {
	var binding Binding
	if !binding.IsZero() {
		t.Fatal("zero Binding should report IsZero")
	}
	if binding.Resolver() != nil {
		t.Fatal("zero Binding should use the default resolver")
	}
	if err := binding.SelfTest(context.Background()); err != nil {
		t.Fatalf("zero Binding self-test: %v", err)
	}
	if got := binding.Network("tcp"); got != "tcp" {
		t.Fatalf("Network(tcp) = %q, want tcp", got)
	}
}

func TestEnv(t *testing.T) {
	binding, err := Parse("127.0.0.2", "eth0")
	if err != nil {
		t.Fatal(err)
	}
	base := []string{"PATH=/bin", EnvSourceIP + "=192.0.2.1", EnvBindInterface + "=old0"}
	env := Env(base, binding)
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "192.0.2.1") || strings.Contains(joined, "old0") {
		t.Fatalf("stale binding survived: %v", env)
	}
	if !strings.Contains(joined, EnvSourceIP+"=127.0.0.2") || !strings.Contains(joined, EnvBindInterface+"=eth0") {
		t.Fatalf("new binding missing: %v", env)
	}

	t.Setenv(EnvSourceIP, "127.0.0.3")
	t.Setenv(EnvBindInterface, "eth1")
	fromEnv, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if fromEnv.SourceIP.String() != "127.0.0.3" || fromEnv.Interface != "eth1" {
		t.Fatalf("FromEnv = %+v", fromEnv)
	}
}

func TestSelfTestSourceLoopback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("127.0.0.2 is not assigned by default on the Windows CI runner")
	}
	binding, err := Parse("127.0.0.2", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := binding.SelfTest(ctx); err != nil {
		skipIfUnavailable(t, err)
		t.Fatalf("SelfTest: %v", err)
	}
}

func TestSelfTestRejectsUnassignedSource(t *testing.T) {
	binding, err := Parse("192.0.2.123", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := binding.SelfTest(ctx); err == nil {
		t.Fatal("expected an unassigned source address to fail the self-test")
	}
}

func TestSelfTestMissingInterface(t *testing.T) {
	binding, err := Parse("", "radar-definitely-missing0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := binding.SelfTest(ctx); err == nil {
		t.Fatal("expected a missing interface to fail the self-test")
	}
}

func TestDialerBindsLoopbackSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("127.0.0.2 is not assigned by default on the Windows CI runner")
	}
	binding, err := Parse("127.0.0.2", "")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := binding.DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		skipIfUnavailable(t, err)
		t.Fatalf("DialContext: %v", err)
	}
	defer client.Close()
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if remote := server.RemoteAddr().(*net.TCPAddr).IP.String(); remote != "127.0.0.2" {
		t.Fatalf("server saw source %s, want 127.0.0.2", remote)
	}
}

func TestInterfaceBindingLoopback(t *testing.T) {
	var iface string
	switch runtime.GOOS {
	case "linux":
		iface = "lo"
	case "darwin":
		iface = "lo0"
	default:
		t.Skip("loopback interface name differs on this platform")
	}
	binding, err := Parse("", iface)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := binding.SelfTest(ctx); err != nil {
		if runtime.GOOS == "darwin" {
			t.Skipf("macOS CI does not permit interface binding: %v", err)
		}
		skipIfUnavailable(t, err)
		t.Fatalf("SelfTest: %v", err)
	}
}

func TestListenICMPWithSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unprivileged ICMP is not available on Windows")
	}
	binding, err := Parse("127.0.0.2", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := binding.ListenICMP(ctx, "udp4")
	if err != nil {
		if runtime.GOOS == "darwin" {
			t.Skipf("macOS CI does not permit unprivileged ICMP: %v", err)
		}
		skipIfUnavailable(t, err)
		t.Fatalf("ListenICMP: %v", err)
	}
	defer conn.Close()
}

func skipIfUnavailable(t *testing.T, err error) {
	t.Helper()
	for _, target := range []error{syscall.EADDRNOTAVAIL, syscall.EPERM, syscall.EACCES} {
		if errors.Is(err, target) {
			t.Skipf("platform does not allow this binding in the test environment: %v", err)
		}
	}
}
