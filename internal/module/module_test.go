package module_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mehrnet/radar-node/internal/module"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDir_ValidModule(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "echo.yaml", `
name: echo-test
engine: fake
engine_version: "1.0"
run:
  command: ["echo", "{{target}}"]
collect:
  format: regex
  pattern: "(?P<latency_ms>[0-9.]+)"
`)
	modules, err := module.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(modules) != 1 || modules[0].Name != "echo-test" {
		t.Fatalf("unexpected modules: %+v", modules)
	}
}

func TestLoadDir_MissingDirIsNotAnError(t *testing.T) {
	modules, err := module.LoadDir("/does/not/exist/at/all")
	if err != nil {
		t.Fatalf("expected no error for a missing dir, got %v", err)
	}
	if len(modules) != 0 {
		t.Fatalf("expected no modules, got %v", modules)
	}
}

func TestLoadDir_RejectsUnknownPlaceholder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: bad-mod
run:
  command: ["echo", "{{not_a_real_placeholder}}"]
collect:
  format: regex
  pattern: "(?P<latency_ms>[0-9.]+)"
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for an unrecognized placeholder")
	}
}

func TestLoadDir_AllowsModuleNamedLikeABuiltinAction(t *testing.T) {
	// There's no more reserved-name concept: every prober is a file,
	// and a module named "tcp" is exactly how the shipped default
	// tcp.yaml (action: tcp_connect) works.
	dir := t.TempDir()
	writeFile(t, dir, "tcp.yaml", `
name: tcp
action: tcp_connect
`)
	modules, err := module.LoadDir(dir)
	if err != nil {
		t.Fatalf("expected a module named %q to load fine, got %v", "tcp", err)
	}
	if len(modules) != 1 || modules[0].Action != "tcp_connect" {
		t.Fatalf("unexpected modules: %+v", modules)
	}
}

func TestLoadDir_RejectsUnknownAction(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: bad-action
action: not_a_real_action
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for an unknown action name")
	}
}

func TestLoadDir_RejectsActionAndRunTogether(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: bad-both
action: tcp_connect
run:
  command: ["echo", "hi"]
collect:
  format: writeout_json
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for a module setting both action and run.command")
	}
}

func TestLoadDir_RejectsNeitherActionNorRun(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: bad-neither
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for a module with neither action nor run.command")
	}
}

func TestLoadDir_RejectsPrepareOnActionModule(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: bad-prepare
action: tcp_connect
prepare:
  command: ["echo", "hi"]
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for an action module also setting prepare")
	}
}

func TestLoadDir_ValidatesRequestResponseSchema(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ok.yaml", `
name: schema-mod
action: tcp_connect
request:
  - name: sni
    type: string
    required: false
  - name: tls
    type: bool
response:
  - name: tls_version
    type: string
`)
	modules, err := module.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(modules[0].Request) != 2 || len(modules[0].Response) != 1 {
		t.Fatalf("unexpected schema: %+v", modules[0])
	}
}

func TestLoadDir_RejectsBadFieldType(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: bad-field-type
action: tcp_connect
request:
  - name: sni
    type: not_a_real_type
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for an unrecognized field type")
	}
}

func TestLoadDir_RejectsDuplicateFieldName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: bad-dup-field
action: tcp_connect
request:
  - name: sni
    type: string
  - name: sni
    type: string
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for a request schema declaring the same field twice")
	}
}

func TestLoadDir_AcceptsUnitAndPrimaryOnResponseField(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ok.yaml", `
name: metric-mod
action: tcp_connect
response:
  - name: mem_used_percent
    type: number
    unit: "%"
    primary: true
  - name: mem_total_bytes
    type: number
    unit: bytes
`)
	modules, err := module.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !modules[0].Response[0].Primary || modules[0].Response[0].Unit != "%" {
		t.Fatalf("expected the first response field to be primary with unit %%, got %+v", modules[0].Response[0])
	}
	if modules[0].Response[1].Primary {
		t.Fatalf("expected the second response field not to be primary: %+v", modules[0].Response[1])
	}
}

func TestLoadDir_RejectsMoreThanOnePrimaryResponseField(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: two-primaries
action: tcp_connect
response:
  - name: a
    type: number
    primary: true
  - name: b
    type: number
    primary: true
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for more than one primary response field")
	}
}

func TestLoadDir_RejectsNonNumberPrimaryField(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: string-primary
action: tcp_connect
response:
  - name: a
    type: string
    primary: true
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for a primary field that isn't a number")
	}
}

func TestLoadDir_RejectsPrimaryOnRequestField(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: request-primary
action: tcp_connect
request:
  - name: a
    type: number
    primary: true
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for primary set on a request field")
	}
}

func TestLoadDir_AcceptsMultipleSummaryFieldsPlusGroupAndDisplay(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ok.yaml", `
name: multi-metric-mod
action: tcp_connect
response:
  - name: cpu_percent
    type: number
    unit: "%"
    summary: true
    group: cpu
    display: gauge
  - name: mem_used_percent
    type: number
    unit: "%"
    summary: true
    group: memory
    display: gauge
  - name: uptime_seconds
    type: number
    unit: s
    group: system
    display: stat
`)
	modules, err := module.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fields := modules[0].Response
	if !fields[0].Summary || fields[0].Group != "cpu" || fields[0].Display != "gauge" {
		t.Fatalf("expected cpu_percent to be summary/cpu/gauge, got %+v", fields[0])
	}
	if !fields[1].Summary || fields[1].Group != "memory" {
		t.Fatalf("expected mem_used_percent to be summary/memory, got %+v", fields[1])
	}
	if fields[2].Summary {
		t.Fatalf("expected uptime_seconds not to be summary, got %+v", fields[2])
	}
	if fields[2].Display != "stat" {
		t.Fatalf("expected uptime_seconds display to be stat, got %+v", fields[2])
	}
}

func TestLoadDir_RejectsNonNumberSummaryField(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: string-summary
action: tcp_connect
response:
  - name: a
    type: string
    summary: true
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for a summary field that isn't a number")
	}
}

func TestLoadDir_RejectsSummaryOnRequestField(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: request-summary
action: tcp_connect
request:
  - name: a
    type: number
    summary: true
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for summary set on a request field")
	}
}

func TestLoadDir_RejectsDuplicateName(t *testing.T) {
	dir := t.TempDir()
	body := `
name: dup
run:
  command: ["echo", "{{target}}"]
collect:
  format: regex
  pattern: "(?P<latency_ms>[0-9.]+)"
`
	writeFile(t, dir, "a.yaml", body)
	writeFile(t, dir, "b.yaml", body)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for two files defining the same module name")
	}
}

func TestLoadDir_RejectsBadRegexPattern(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: bad-regex
run:
  command: ["echo", "hi"]
collect:
  format: regex
  pattern: "("
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for an unparseable regex")
	}
}

func TestLoadDir_RejectsRegexMissingLatencyGroup(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: no-latency-group
run:
  command: ["echo", "hi"]
collect:
  format: regex
  pattern: "(?P<foo>.*)"
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for a regex with no latency_ms group")
	}
}

func TestLoadDir_RejectsEmptyRunCommand(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: empty-run
run:
  command: []
collect:
  format: writeout_json
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for an empty run.command")
	}
}

func TestLoadDir_AcceptsParamPlaceholder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ok.yaml", `
name: param-mod
run:
  command: ["echo", "{{param.sni}}", "{{params_json}}", "{{alloc_port}}", "{{timeout_ms}}"]
collect:
  format: writeout_json
`)
	if _, err := module.LoadDir(dir); err != nil {
		t.Fatalf("expected all fixed + param.* placeholders to be accepted, got %v", err)
	}
}

func validPoolYAML(extra string) string {
	return `
name: pool-mod
run:
  command: ["echo", "{{target}}", "{{alloc_port}}"]
collect:
  format: writeout_json
pool:
  max_jobs_per_instance: 100
  test_concurrency: 20
  build_config:
    command: ["build-config", "{{jobs_json}}", "{{config_path}}"]
  start:
    command: ["engine", "{{config_path}}"]
` + extra
}

func TestLoadDir_AcceptsValidPoolModule(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ok.yaml", validPoolYAML(""))
	modules, err := module.LoadDir(dir)
	if err != nil {
		t.Fatalf("expected a valid pool module to load, got %v", err)
	}
	if len(modules) != 1 || modules[0].Pool == nil {
		t.Fatalf("unexpected modules: %+v", modules)
	}
	p := modules[0].Pool
	if p.MaxJobsPerInstance != 100 || p.TestConcurrency != 20 {
		t.Fatalf("unexpected pool spec: %+v", p)
	}
}

func TestLoadDir_AcceptsPoolModuleWithStop(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ok.yaml", validPoolYAML(`
  stop:
    command: ["engine-stop"]
`))
	modules, err := module.LoadDir(dir)
	if err != nil {
		t.Fatalf("expected a pool module with stop to load, got %v", err)
	}
	if modules[0].Pool.Stop == nil {
		t.Fatal("expected pool.stop to be set")
	}
}

func TestLoadDir_RejectsPoolOnActionModule(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: bad-pool-action
action: tcp_connect
pool:
  max_jobs_per_instance: 10
  test_concurrency: 5
  build_config:
    command: ["build-config"]
  start:
    command: ["engine"]
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for an action module also setting pool")
	}
}

func TestLoadDir_RejectsPoolWithPrepare(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", strings.Replace(validPoolYAML(""), "run:", `prepare:
  command: ["echo", "hi"]
run:`, 1))
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for a pool module also setting prepare")
	}
}

func TestLoadDir_RejectsPoolWithTeardown(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", validPoolYAML(`
teardown:
  command: ["echo", "bye"]
`))
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for a pool module also setting teardown")
	}
}

func TestLoadDir_RejectsPoolWithoutRun(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: bad-pool-no-run
pool:
  max_jobs_per_instance: 10
  test_concurrency: 5
  build_config:
    command: ["build-config"]
  start:
    command: ["engine"]
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for a pool module without run.command")
	}
}

func TestLoadDir_RejectsPoolMissingBuildConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: bad-pool-no-build-config
run:
  command: ["echo", "{{target}}"]
collect:
  format: writeout_json
pool:
  max_jobs_per_instance: 10
  test_concurrency: 5
  start:
    command: ["engine"]
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for a pool module missing build_config")
	}
}

func TestLoadDir_RejectsPoolMissingStart(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: bad-pool-no-start
run:
  command: ["echo", "{{target}}"]
collect:
  format: writeout_json
pool:
  max_jobs_per_instance: 10
  test_concurrency: 5
  build_config:
    command: ["build-config"]
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for a pool module missing start")
	}
}

func TestLoadDir_RejectsPoolZeroMaxJobsPerInstance(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: bad-pool-zero-max
run:
  command: ["echo", "{{target}}"]
collect:
  format: writeout_json
pool:
  max_jobs_per_instance: 0
  test_concurrency: 5
  build_config:
    command: ["build-config"]
  start:
    command: ["engine"]
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for pool.max_jobs_per_instance <= 0")
	}
}

func TestLoadDir_RejectsPoolZeroTestConcurrency(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: bad-pool-zero-concurrency
run:
  command: ["echo", "{{target}}"]
collect:
  format: writeout_json
pool:
  max_jobs_per_instance: 10
  test_concurrency: 0
  build_config:
    command: ["build-config"]
  start:
    command: ["engine"]
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for pool.test_concurrency <= 0")
	}
}

func TestLoadDir_RejectsUnknownPlaceholderInPoolSteps(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", strings.Replace(validPoolYAML(""), "{{jobs_json}}", "{{not_a_real_placeholder}}", 1))
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for an unrecognized placeholder in pool.build_config")
	}
}

func TestLoadDir_AcceptsJobsJSONAndConfigPathPlaceholders(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ok.yaml", validPoolYAML(""))
	if _, err := module.LoadDir(dir); err != nil {
		t.Fatalf("expected jobs_json/config_path placeholders to be accepted in pool steps, got %v", err)
	}
}

func TestResolveDirPlaceholders_SubstitutesPoolStepArgv(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ok.yaml", `
name: pool-dirs
run:
  command: ["__TOOLS_DIR__/engine-run"]
collect:
  format: writeout_json
pool:
  max_jobs_per_instance: 10
  test_concurrency: 5
  build_config:
    command: ["__TOOLS_DIR__/build-config", "__MODULES_DIR__/out.json"]
  start:
    command: ["__TOOLS_DIR__/engine-start"]
  stop:
    command: ["__TOOLS_DIR__/engine-stop"]
`)
	modules, err := module.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	resolved := modules[0].ResolveDirPlaceholders("/modules", "/tools")
	if resolved.Pool.BuildConfig.Command[0] != "/tools/build-config" {
		t.Fatalf("build_config not resolved: %+v", resolved.Pool.BuildConfig.Command)
	}
	if resolved.Pool.BuildConfig.Command[1] != "/modules/out.json" {
		t.Fatalf("build_config not resolved: %+v", resolved.Pool.BuildConfig.Command)
	}
	if resolved.Pool.Start.Command[0] != "/tools/engine-start" {
		t.Fatalf("start not resolved: %+v", resolved.Pool.Start.Command)
	}
	if resolved.Pool.Stop.Command[0] != "/tools/engine-stop" {
		t.Fatalf("stop not resolved: %+v", resolved.Pool.Stop.Command)
	}
	// The original module (pre-resolution) must be untouched -- same
	// invariant ResolveDirPlaceholders already guarantees for prepare/
	// run/teardown.
	if modules[0].Pool.BuildConfig.Command[0] != "__TOOLS_DIR__/build-config" {
		t.Fatalf("original module's pool steps were mutated: %+v", modules[0].Pool.BuildConfig.Command)
	}
}

func TestParseBytes_ParsesOSArchAndInstall(t *testing.T) {
	m, err := module.ParseBytes([]byte(`
name: xray
version: "26.3.27-1"
url: https://radar.mehrnet.com/install/modules/xray.yaml
os: [linux, darwin, windows]
arch: [amd64, arm64]
install:
  - name: xray
    kind: binary
    version: "26.3.27-1"
    url: https://radar.mehrnet.com/releases/xray/xray_latest_{os}_{arch}.{ext}
    path: __TOOLS_DIR__/xray
  - name: xray-prepare.sh
    kind: file
    url: https://radar.mehrnet.com/install/modules/xray-prepare.sh
    path: __MODULES_DIR__/xray-prepare.sh
  - name: xray-run.sh
    kind: file
    url: https://radar.mehrnet.com/install/modules/xray-run.sh
    path: __MODULES_DIR__/xray-run.sh
run:
  command: ["echo", "{{target}}"]
collect:
  format: writeout_json
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.OS) != 3 || m.OS[0] != "linux" || m.OS[2] != "windows" {
		t.Errorf("expected os: [linux, darwin, windows], got %v", m.OS)
	}
	if len(m.Arch) != 2 || m.Arch[0] != "amd64" {
		t.Errorf("expected arch: [amd64, arm64], got %v", m.Arch)
	}
	if len(m.Install) != 3 {
		t.Fatalf("expected exactly three install entries, got %d", len(m.Install))
	}
	dep := m.Install[0]
	if dep.Name != "xray" || dep.Version != "26.3.27-1" || dep.IsFile() {
		t.Errorf("unexpected install dependency: %+v", dep)
	}
	if !m.Install[1].IsFile() || m.Install[1].Name != "xray-prepare.sh" {
		t.Errorf("expected xray-prepare.sh as a file entry, got %+v", m.Install[1])
	}
	if !m.Install[2].IsFile() || m.Install[2].Name != "xray-run.sh" {
		t.Errorf("expected xray-run.sh as a file entry, got %+v", m.Install[2])
	}
}

func TestInstallDependency_ResolveURL_SubstitutesPlatformAndPicksExt(t *testing.T) {
	dep := module.InstallDependency{URL: "https://radar.mehrnet.com/releases/xray/xray_latest_{os}_{arch}.{ext}"}
	if got := dep.ResolveURL("linux", "amd64"); got != "https://radar.mehrnet.com/releases/xray/xray_latest_linux_amd64.tar.gz" {
		t.Errorf("linux/amd64: got %q", got)
	}
	if got := dep.ResolveURL("windows", "arm64"); got != "https://radar.mehrnet.com/releases/xray/xray_latest_windows_arm64.zip" {
		t.Errorf("windows/arm64: expected .zip, got %q", got)
	}
}

func TestInstallDependency_ResolveURL_SubstitutesVersion(t *testing.T) {
	dep := module.InstallDependency{Version: "26.3.27", URL: "https://radar.mehrnet.com/releases/xray/xray_{version}_{os}_{arch}.{ext}"}
	if got := dep.ResolveURL("linux", "amd64"); got != "https://radar.mehrnet.com/releases/xray/xray_26.3.27_linux_amd64.tar.gz" {
		t.Errorf("got %q", got)
	}
}

func TestInstallDependency_ResolveURL_NoVersionPlaceholderIsANoop(t *testing.T) {
	dep := module.InstallDependency{Version: "26.3.27", URL: "https://radar.mehrnet.com/releases/xray/xray_latest_{os}_{arch}.{ext}"}
	if got := dep.ResolveURL("linux", "amd64"); got != "https://radar.mehrnet.com/releases/xray/xray_latest_linux_amd64.tar.gz" {
		t.Errorf("got %q", got)
	}
}

// Regression test for a real production incident: install.sh writing
// a module's own YAML verbatim (see moduleinstall.go's own comment on
// why) left every prepare/run command's own __MODULES_DIR__/
// __TOOLS_DIR__ references unresolved -- nothing had ever substituted
// them anywhere else, since that substitution used to be install.sh's
// own job for the *whole file*, command blocks included. Every
// xray/wireguard/openvpn check across the fleet started failing
// ("prepare: did not become ready ... context deadline exceeded")
// the moment that verbatim-write change shipped. ResolveDirPlaceholders
// is what registry.LoadModules now calls to fix this at load time
// instead, regardless of how the module got onto disk.
func TestResolveDirPlaceholders_SubstitutesCommandArgv(t *testing.T) {
	m, err := module.ParseBytes([]byte(`
name: xray
install:
  - name: xray
    kind: binary
    url: https://radar.mehrnet.com/releases/xray/xray_latest_{os}_{arch}.{ext}
    path: __TOOLS_DIR__/xray
prepare:
  command: ["/bin/sh", "__MODULES_DIR__/xray-prepare.sh", "{{params_json}}", "{{alloc_port}}"]
run:
  command: ["/bin/sh", "__MODULES_DIR__/xray-run.sh", "{{alloc_port}}", "{{target}}"]
collect:
  format: writeout_json
`))
	if err != nil {
		t.Fatal(err)
	}
	resolved := m.ResolveDirPlaceholders("/etc/radar-node/modules.d", "/etc/radar-node/tools")
	wantPrepare := []string{"/bin/sh", "/etc/radar-node/modules.d/xray-prepare.sh", "{{params_json}}", "{{alloc_port}}"}
	if !reflect.DeepEqual(resolved.Prepare.Command, wantPrepare) {
		t.Errorf("prepare.command: got %v, want %v", resolved.Prepare.Command, wantPrepare)
	}
	wantRun := []string{"/bin/sh", "/etc/radar-node/modules.d/xray-run.sh", "{{alloc_port}}", "{{target}}"}
	if !reflect.DeepEqual(resolved.Run.Command, wantRun) {
		t.Errorf("run.command: got %v, want %v", resolved.Run.Command, wantRun)
	}
	// {{...}} placeholders are a completely separate mechanism
	// (resolved per-probe at run time, not here) -- untouched.
	if resolved.Prepare.Command[2] != "{{params_json}}" {
		t.Errorf("expected {{params_json}} left alone, got %q", resolved.Prepare.Command[2])
	}
	// install[].path is a *different* placeholder use (resolved by
	// moduleinstall.go at fetch/install time, never by this) -- also
	// untouched here.
	if resolved.Install[0].Path != "__TOOLS_DIR__/xray" {
		t.Errorf("expected install[].path left alone, got %q", resolved.Install[0].Path)
	}
	// The module's own identity (used for the heartbeat content-
	// addressing handshake) must keep reflecting the literal on-disk
	// bytes, not this resolved copy.
	if resolved.RawYAML != m.RawYAML || resolved.FileHash != m.FileHash {
		t.Error("expected RawYAML/FileHash unchanged by resolution")
	}
}

func TestLoadDir_RejectsInstallDependencyMissingName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: bad-install
install:
  - url: https://example.com/bad_{os}_{arch}.{ext}
run:
  command: ["echo", "{{target}}"]
collect:
  format: writeout_json
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for an install dependency missing name")
	}
}

func TestLoadDir_RejectsInstallDependencyMissingURL(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: bad-install
install:
  - name: bad
run:
  command: ["echo", "{{target}}"]
collect:
  format: writeout_json
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for an install dependency missing url")
	}
}

func TestLoadDir_RejectsInstallDependencyMissingPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: bad-install
install:
  - name: bad
    url: https://example.com/bad_{os}_{arch}.{ext}
run:
  command: ["echo", "{{target}}"]
collect:
  format: writeout_json
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for an install dependency missing path")
	}
}

// A locally-installed module's path may already be a resolved
// absolute path, not the __TOOLS_DIR__/__MODULES_DIR__ placeholder --
// install.sh's own legacy substitution rewrites every file it deploys
// under modules.d, module YAML included, before this package ever
// sees it (see a real production incident this regression test
// reproduces: v0.26 crash-looped on every already-updated node for
// exactly this reason). LoadDir must tolerate that; only
// moduleinstall's own write path enforces the strict placeholder
// form.
func TestLoadDir_AcceptsAlreadyResolvedAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "openvpn.yaml", `
name: openvpn
install:
  - name: openvpn
    kind: binary
    url: https://example.com/openvpn_{os}_{arch}.{ext}
    path: /etc/radar-node/tools/openvpn
run:
  command: ["echo", "{{target}}"]
collect:
  format: writeout_json
`)
	if _, err := module.LoadDir(dir); err != nil {
		t.Fatalf("expected an already-resolved absolute path to load without error, got %v", err)
	}
}

func TestLoadDir_RejectsInstallDependencyInvalidKind(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
name: bad-install
install:
  - name: bad
    kind: archive
    url: https://example.com/bad_{os}_{arch}.{ext}
    path: __TOOLS_DIR__/bad
run:
  command: ["echo", "{{target}}"]
collect:
  format: writeout_json
`)
	if _, err := module.LoadDir(dir); err == nil {
		t.Fatal("expected an error for an install dependency with an invalid kind")
	}
}

// Regression test against the *real* production module manifests
// (radar.mehrnet.com/install/modules/*.yaml, canonically sourced from
// here) -- specifically, that every binary install: dependency's
// {version} placeholder actually resolves to something (a non-empty
// Version field), so a manifest edit can never silently regress back
// to a mutable "_latest_"-only URL with no version-pinned fetch path
// at all. Doesn't hit the network -- ResolveURL is pure string
// substitution -- so this can't tell a *correct* version string from
// a wrong one, only that one is present and does get substituted in.
func TestLoadDir_ProductionModuleManifestsResolveVersionedURLs(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("..", "..", "install", "modules"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("production module manifests not found at %s (%v) -- skipping", dir, err)
	}
	modules, err := module.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir(%s): %v", dir, err)
	}
	byName := make(map[string]module.Module, len(modules))
	for _, m := range modules {
		byName[m.Name] = m
	}
	for _, name := range []string{"xray", "wireguard", "openvpn"} {
		m, ok := byName[name]
		if !ok {
			t.Errorf("expected module %q to be loaded from %s", name, dir)
			continue
		}
		for _, dep := range m.Install {
			if dep.IsFile() {
				continue
			}
			if dep.Version == "" {
				t.Errorf("%s: binary dependency %q has no Version -- {version} in its URL (%q) will resolve to an empty string", name, dep.Name, dep.URL)
				continue
			}
			resolved := dep.ResolveURL("linux", "amd64")
			if strings.Contains(resolved, "{version}") {
				t.Errorf("%s: dependency %q's URL still contains a literal {version} after resolving: %q", name, dep.Name, resolved)
			}
			if !strings.Contains(resolved, dep.Version) {
				t.Errorf("%s: dependency %q's resolved URL %q doesn't contain its own Version %q", name, dep.Name, resolved, dep.Version)
			}
		}
	}
}
