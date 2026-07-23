package registry_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/probe"
	"github.com/mehrnet/radar-node/internal/registry"
)

func TestDefault_LoadsAllSixBuiltins(t *testing.T) {
	reg, err := registry.Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tcp", "udp", "dns", "icmp", "http", "https", "system"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("expected %q to be registered by default", name)
		}
		_, hash, manifest, ok := reg.RawYAML(name)
		if !ok || hash == "" {
			t.Errorf("expected %q to have a non-empty file hash, got %q (ok=%v)", name, hash, ok)
		}
		if manifest.Name != name || manifest.Action == "" {
			t.Errorf("expected %q's manifest to have a matching name and a non-empty action, got %+v", name, manifest)
		}
	}
	if got := len(reg.ProberHashes()); got != 7 {
		t.Errorf("expected 7 prober_id:file_hash pairs, got %d: %v", got, reg.ProberHashes())
	}
}

func TestDefault_TCPActionRunsForReal(t *testing.T) {
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

	reg, err := registry.Default()
	if err != nil {
		t.Fatal(err)
	}
	checker, ok := reg.Get("tcp")
	if !ok {
		t.Fatal("expected tcp to be registered")
	}
	res := checker.Check(context.Background(), probe.Options{
		Target:  ln.Addr().String(),
		Timeout: 2 * time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
	})
	if !res.Ok {
		t.Fatalf("expected ok, got error %q", res.Error)
	}
}

// Regression test for a real production incident: every xray/
// wireguard/openvpn check across the fleet started failing the moment
// install.sh began writing a module's own YAML verbatim -- nothing
// else had ever resolved __MODULES_DIR__/__TOOLS_DIR__ inside a
// prepare/run/teardown command, that substitution used to be
// install.sh's own job for the *whole file*. This exercises the real
// fix end to end: LoadModules resolves them at load time, regardless
// of what's literally on disk, so the actual subprocess this module's
// "run" step launches sees real, resolved paths.
func TestLoadModules_ResolvesDirPlaceholdersInCommand_RealSubprocess(t *testing.T) {
	dir := t.TempDir()
	toolsDir := filepath.Join(t.TempDir(), "tools")
	// Genuinely unresolved, this module would try to run a shell
	// script at the literal path "__MODULES_DIR__/echo-dirs.sh",
	// which doesn't exist -- exactly the "did not become ready"/exit-
	// failure shape the real incident produced.
	if err := writeFile(dir, "echo-dirs.yaml", `
name: echo-dirs
run:
  command: ["/bin/sh", "-c", "printf '{\"latency_ms\":1,\"modules_dir\":\"%s\",\"tools_dir\":\"%s\"}' __MODULES_DIR__ __TOOLS_DIR__"]
collect:
  format: writeout_json
`); err != nil {
		t.Fatal(err)
	}

	reg, err := registry.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.LoadModules(dir, toolsDir); err != nil {
		t.Fatal(err)
	}
	checker, ok := reg.Get("echo-dirs")
	if !ok {
		t.Fatal("expected echo-dirs to be registered")
	}
	res := checker.Check(context.Background(), probe.Options{Target: "x", Timeout: 2 * time.Second, Mode: probe.ModeWarm, Seq: 1})
	if !res.Ok {
		t.Fatalf("expected the resolved command to actually run, got error %q", res.Error)
	}
	if got := res.Extra["modules_dir"]; got != dir {
		t.Errorf("expected __MODULES_DIR__ resolved to %q, got %v", dir, got)
	}
	if got := res.Extra["tools_dir"]; got != toolsDir {
		t.Errorf("expected __TOOLS_DIR__ resolved to %q, got %v", toolsDir, got)
	}
}

func TestLoadModules_OverridesEmbeddedDefaultByName(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(dir, "tcp.yaml", `
name: tcp
action: tcp_connect
request:
  - name: must_have
    type: string
    required: true
`); err != nil {
		t.Fatal(err)
	}

	reg, err := registry.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.LoadModules(dir, ""); err != nil {
		t.Fatal(err)
	}
	checker, _ := reg.Get("tcp")
	res := checker.Check(context.Background(), probe.Options{
		Target:  "127.0.0.1:1",
		Timeout: time.Second,
		Mode:    probe.ModeWarm,
		Seq:     1,
	})
	if res.Ok || res.ErrorCode != probe.ErrorCodeInvalidParams {
		t.Fatalf("expected the overriding tcp.yaml's stricter schema to reject this, got ok=%v error_code=%q", res.Ok, res.ErrorCode)
	}
}

func TestDefault_ModuleVersions_EmptyForBuiltinsWithNeither(t *testing.T) {
	reg, err := registry.Default()
	if err != nil {
		t.Fatal(err)
	}
	// The embedded tcp/udp/dns/... defaults have no version/url of
	// their own -- they're versioned implicitly with radar-node's own
	// release, so none of them should show up here at all.
	if got := reg.ModuleVersions(); len(got) != 0 {
		t.Errorf("expected no module versions for the embedded defaults, got %+v", got)
	}
}

func TestLoadModules_ModuleVersions_ReportsVersionAndURLWhenPresent(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(dir, "xray.yaml", `
name: xray
version: "26.3.27-1"
url: https://radar.mehrnet.com/install/modules/xray.yaml
run:
  command: ["true"]
collect:
  format: writeout_json
`); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(dir, "wireguard.yaml", `
name: wireguard
run:
  command: ["true"]
collect:
  format: writeout_json
`); err != nil {
		t.Fatal(err)
	}

	reg, err := registry.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.LoadModules(dir, ""); err != nil {
		t.Fatal(err)
	}

	versions := reg.ModuleVersions()
	xray, ok := versions["xray"]
	if !ok {
		t.Fatalf("expected \"xray\" in ModuleVersions, got %+v", versions)
	}
	if xray.Version == nil || *xray.Version != "26.3.27-1" {
		t.Errorf("expected xray version 26.3.27-1, got %v", xray.Version)
	}
	if xray.URL == nil || *xray.URL != "https://radar.mehrnet.com/install/modules/xray.yaml" {
		t.Errorf("expected xray's url, got %v", xray.URL)
	}

	// wireguard.yaml declared neither -- absent from the map entirely,
	// same as the embedded builtins, not present with null fields.
	if _, ok := versions["wireguard"]; ok {
		t.Errorf("expected \"wireguard\" to be absent from ModuleVersions (no version/url declared), got an entry")
	}
}

func writeFile(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
}
