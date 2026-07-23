package module_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/module"
	"github.com/mehrnet/radar-node/internal/probe"
)

func loadOne(t *testing.T, yamlBody string) module.Module {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "m.yaml", yamlBody)
	modules, err := module.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(modules) != 1 {
		t.Fatalf("expected 1 module, got %d", len(modules))
	}
	return modules[0]
}

// TestChecker_RunOnly proves the run+collect(writeout_json) path with
// a real external binary (curl), the same pattern a genuine
// HTTP-through-proxy module would use -- no prepare step needed.
func TestChecker_RunOnly_WriteoutJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	m := loadOne(t, `
name: curl-writeout
engine: curl
run:
  command: ["curl", "--silent", "--max-time", "5", "-o", "/dev/null",
            "-w", "{\"latency_ms\": %{time_total}, \"http_code\": %{http_code}}",
            "{{target}}"]
collect:
  format: writeout_json
`)

	c := module.NewChecker(m)
	if c.Type() != "curl-writeout" {
		t.Fatalf("unexpected Type(): %s", c.Type())
	}

	res := c.Check(context.Background(), probe.Options{
		Target:  srv.URL,
		Timeout: 5 * time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if res.LatencyMs == nil {
		t.Fatal("expected latency_ms to be set")
	}
	if code, _ := res.Extra["http_code"].(float64); code != 204 {
		t.Fatalf("expected http_code 204 in extra, got %v", res.Extra["http_code"])
	}
}

func TestChecker_RunOnly_Regex(t *testing.T) {
	m := loadOne(t, `
name: echo-regex
run:
  command: ["echo", "latency=12.5 ok=true"]
collect:
  format: regex
  pattern: "latency=(?P<latency_ms>[0-9.]+) ok=(?P<status>\\w+)"
`)

	c := module.NewChecker(m)
	res := c.Check(context.Background(), probe.Options{
		Target:  "irrelevant",
		Timeout: 2 * time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if res.LatencyMs == nil {
		t.Fatal("expected latency_ms to be set")
	}
	if *res.LatencyMs != 12.5 {
		t.Fatalf("expected latency_ms=12.5, got %v", *res.LatencyMs)
	}
	if res.Extra["status"] != "true" {
		t.Fatalf("expected status=true in extra, got %v", res.Extra["status"])
	}
}

func TestChecker_NonZeroExitIsFailure(t *testing.T) {
	m := loadOne(t, `
name: always-fails
run:
  command: ["sh", "-c", "exit 1"]
collect:
  format: writeout_json
`)
	// "sh -c" here is the module's own command, authored by the
	// operator in a local, trusted YAML file -- not a placeholder
	// substitution, so this does not reopen the shell-injection
	// concern the argv-only design exists to close.
	c := module.NewChecker(m)
	res := c.Check(context.Background(), probe.Options{
		Target:  "x",
		Timeout: 2 * time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
	})
	if res.Ok {
		t.Fatal("expected a non-zero exit to be reported as a failure")
	}
}

// TestChecker_ActionModule_RunsNativeImplementation proves an
// action-based module actually reaches the registered Go
// implementation (tcp_connect) rather than going anywhere near a
// subprocess -- a real TCP listener on this box, dialed through the
// module system exactly like a probe would.
func TestChecker_ActionModule_RunsNativeImplementation(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	m := loadOne(t, `
name: tcp
action: tcp_connect
`)
	c := module.NewChecker(m)
	if c.Type() != "tcp" {
		t.Fatalf("unexpected Type(): %s", c.Type())
	}
	res := c.Check(context.Background(), probe.Options{
		Target:  ln.Addr().String(),
		Timeout: 2 * time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if res.Type != "tcp" {
		t.Fatalf("expected the result to report the module's own name, got %q", res.Type)
	}
}

func TestChecker_RequestSchema_RejectsMissingRequiredParam(t *testing.T) {
	m := loadOne(t, `
name: needs-uuid
action: tcp_connect
request:
  - name: uuid
    type: string
    required: true
`)
	c := module.NewChecker(m)
	res := c.Check(context.Background(), probe.Options{
		Target:  "127.0.0.1:1",
		Timeout: time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
	})
	if res.Ok {
		t.Fatal("expected a missing required param to fail")
	}
	if res.ErrorCode != probe.ErrorCodeInvalidParams {
		t.Fatalf("expected ErrorCode %q, got %q", probe.ErrorCodeInvalidParams, res.ErrorCode)
	}
}

func TestChecker_RequestSchema_RejectsWrongType(t *testing.T) {
	m := loadOne(t, `
name: wants-bool
action: tcp_connect
request:
  - name: tls
    type: bool
`)
	c := module.NewChecker(m)
	res := c.Check(context.Background(), probe.Options{
		Target:  "127.0.0.1:1",
		Timeout: time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
		Params:  map[string]any{"tls": "not-a-bool"},
	})
	if res.Ok || res.ErrorCode != probe.ErrorCodeInvalidParams {
		t.Fatalf("expected an invalid_params rejection, got ok=%v error_code=%q", res.Ok, res.ErrorCode)
	}
}

func TestChecker_RequestSchema_AcceptsValidParams(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
	}()

	m := loadOne(t, `
name: tcp-with-schema
action: tcp_connect
request:
  - name: tls
    type: bool
    required: false
`)
	c := module.NewChecker(m)
	res := c.Check(context.Background(), probe.Options{
		Target:  ln.Addr().String(),
		Timeout: 2 * time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
		Params:  map[string]any{"tls": false},
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q (code %q)", res.Error, res.ErrorCode)
	}
}

// TestChecker_PrepareThenRun exercises the full prepare/alloc_port/
// waitForPort/run/teardown lifecycle against real subprocesses: a
// tiny Python TCP listener as the "prepare"d engine, and a Python
// client as "run" -- the same shape a real xray/sing-box module will
// use (prepare starts a local proxy listener, run connects through
// it), without depending on those binaries being available here.
func TestChecker_PrepareThenRun(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	dir := t.TempDir()
	listenerScript := filepath.Join(dir, "listener.py")
	if err := os.WriteFile(listenerScript, []byte(`
import socket, sys
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", int(sys.argv[1])))
s.listen(1)
while True:
    conn, _ = s.accept()
    conn.close()
`), 0o644); err != nil {
		t.Fatal(err)
	}
	clientScript := filepath.Join(dir, "client.py")
	if err := os.WriteFile(clientScript, []byte(`
import socket, sys, time
start = time.time()
s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=2)
s.close()
elapsed_ms = (time.time() - start) * 1000
print('{"latency_ms": %f}' % elapsed_ms)
`), 0o644); err != nil {
		t.Fatal(err)
	}

	m := loadOne(t, `
name: prepare-lifecycle
engine: fake-tunnel
prepare:
  command: ["python3", "`+listenerScript+`", "{{alloc_port}}"]
run:
  command: ["python3", "`+clientScript+`", "{{alloc_port}}"]
collect:
  format: writeout_json
`)

	c := module.NewChecker(m)
	res := c.Check(context.Background(), probe.Options{
		Target:  "unused",
		Timeout: 5 * time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
	if res.LatencyMs == nil {
		t.Fatal("expected latency_ms to be set")
	}
}

// Regression test: prepare's own readiness wait used to be a flat 3s
// regardless of the probe's own configured timeout_ms -- a slow-to-
// start engine (a real xray under concurrent load, say) could never
// succeed no matter how generous timeout_ms was set to, since 3s
// always won. It's now half of timeout_ms instead, so a probe with
// enough of its own budget gives prepare proportionally more room.
func TestChecker_PrepareReadinessScalesWithTimeout(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	dir := t.TempDir()
	// Only starts listening after a deliberate delay -- longer than
	// the old flat 3s cap, short enough to fit inside half of a
	// generous timeout_ms.
	// Loops accepting connections (like TestChecker_PrepareThenRun's own
	// listener) rather than handling just one -- readiness's own probe
	// connection and the real client's later one both connect here, and
	// a single-accept-then-exit listener would let the readiness check
	// consume the only connection the real client needed.
	slowListenerScript := filepath.Join(dir, "slow-listener.py")
	if err := os.WriteFile(slowListenerScript, []byte(`
import socket, sys, time
time.sleep(4)
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", int(sys.argv[1])))
s.listen(1)
while True:
    conn, _ = s.accept()
    conn.close()
`), 0o644); err != nil {
		t.Fatal(err)
	}
	clientScript := filepath.Join(dir, "client.py")
	if err := os.WriteFile(clientScript, []byte(`
import socket, sys
s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=2)
s.close()
print('{"latency_ms": 1}')
`), 0o644); err != nil {
		t.Fatal(err)
	}

	m := loadOne(t, `
name: slow-prepare
engine: fake-tunnel
prepare:
  command: ["python3", "`+slowListenerScript+`", "{{alloc_port}}"]
run:
  command: ["python3", "`+clientScript+`", "{{alloc_port}}"]
collect:
  format: writeout_json
`)
	c := module.NewChecker(m)

	// Half of 10s (5s) comfortably covers the listener's 4s startup
	// delay -- would have failed under the old flat 3s cap.
	res := c.Check(context.Background(), probe.Options{Target: "unused", Timeout: 10 * time.Second, Mode: probe.ModeWarm, Seq: 1})
	if !res.Ok {
		t.Fatalf("expected a generous timeout_ms to give prepare enough room for a 4s startup, got error %q", res.Error)
	}

	// Half of 2s (1s) does not cover it -- proves this is actually
	// proportional, not just "now always generous".
	res = c.Check(context.Background(), probe.Options{Target: "unused", Timeout: 2 * time.Second, Mode: probe.ModeWarm, Seq: 1})
	if res.Ok {
		t.Fatal("expected a short timeout_ms to still not give a 4s startup enough room")
	}
}
