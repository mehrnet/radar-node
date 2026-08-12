package module_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mehrnet/radar-node/internal/module"
	"github.com/mehrnet/radar-node/internal/probe"
)

// poolFixture writes a fake "engine" (a python3 script listening on
// every job's alloc_port, driven entirely by whatever build_config
// copies into config_path) plus matching build_config/run scripts, and
// returns a pool module YAML wired to them. This is the same shape a
// real xray pool module will use -- build_config describes the whole
// instance's jobs, start brings up one process serving all of them,
// run tests one job at a time against its own already-listening port
// -- without depending on xray itself being available in this
// environment.
func poolFixture(t *testing.T, maxJobsPerInstance, testConcurrency int) (module.Module, string) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()

	logPath := filepath.Join(dir, "build-config.log")

	buildConfigScript := filepath.Join(dir, "build-config.py")
	mustWrite(t, buildConfigScript, `
import shutil, sys
with open("`+logPath+`", "a") as log:
    log.write("instance\n")
shutil.copy(sys.argv[1], sys.argv[2])
`)

	startScript := filepath.Join(dir, "start.py")
	mustWrite(t, startScript, `
import sys, json, socket, threading, time

with open(sys.argv[1]) as f:
    jobs = json.load(f)

def serve(port):
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("127.0.0.1", port))
    s.listen(5)
    while True:
        conn, _ = s.accept()
        conn.close()

for j in jobs:
    threading.Thread(target=serve, args=(j["alloc_port"],), daemon=True).start()

while True:
    time.sleep(1)
`)

	runScript := filepath.Join(dir, "run.py")
	mustWrite(t, runScript, `
import socket, sys
target, port = sys.argv[1], int(sys.argv[2])
if target == "fail":
    sys.exit(1)
s = socket.create_connection(("127.0.0.1", port), timeout=2)
s.close()
print('{"latency_ms": 1, "saw_target": "%s"}' % target)
`)

	m := loadOne(t, `
name: pool-mod
run:
  command: ["python3", "`+runScript+`", "{{target}}", "{{alloc_port}}"]
collect:
  format: writeout_json
pool:
  max_jobs_per_instance: `+strconv.Itoa(maxJobsPerInstance)+`
  test_concurrency: `+strconv.Itoa(testConcurrency)+`
  build_config:
    command: ["python3", "`+buildConfigScript+`", "{{jobs_json}}", "{{config_path}}"]
  start:
    command: ["python3", "`+startScript+`", "{{config_path}}"]
`)
	return m, logPath
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func jobOpts(target string, seq int) probe.Options {
	return probe.Options{Target: target, Timeout: 5 * time.Second, Seq: seq}
}

func TestPoolChecker_ImplementsBatchChecker(t *testing.T) {
	m, _ := poolFixture(t, 10, 2)
	checker := module.NewChecker(m)
	if _, ok := checker.(probe.BatchChecker); !ok {
		t.Fatalf("expected a pooled module's Checker to implement probe.BatchChecker, got %T", checker)
	}
}

func TestPoolChecker_NonPoolModuleDoesNotImplementBatchChecker(t *testing.T) {
	m := loadOne(t, `
name: not-pooled
run:
  command: ["echo", "hi"]
collect:
  format: writeout_json
`)
	checker := module.NewChecker(m)
	if _, ok := checker.(probe.BatchChecker); ok {
		t.Fatal("expected a non-pooled module's Checker to not implement probe.BatchChecker")
	}
}

// Regression test for a real bug this design's own manual xray
// end-to-end verification caught: {{config_path}} used to be a
// CreateTemp path with no extension at all, and a real engine (xray)
// infers a config file's format from its extension rather than
// sniffing content -- it failed outright ("Failed to get format of
// ...") against a fake test engine, like poolFixture's own python
// scripts here, would never notice since neither cares about the
// extension.
func TestPoolChecker_CheckBatch_ConfigPathHasJSONExtension(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	capturedPath := filepath.Join(dir, "captured-config-path.txt")
	captureScript := filepath.Join(dir, "capture.py")
	mustWrite(t, captureScript, `
import sys
with open(sys.argv[2], "w") as f:
    f.write(sys.argv[1])
`)

	m := loadOne(t, `
name: pool-config-path-mod
run:
  command: ["true"]
collect:
  format: writeout_json
pool:
  max_jobs_per_instance: 10
  test_concurrency: 2
  build_config:
    command: ["python3", "`+captureScript+`", "{{config_path}}", "`+capturedPath+`"]
  start:
    command: ["true"]
`)
	checker := module.NewChecker(m).(probe.BatchChecker)
	// The instance will never actually become ready (start is a no-op
	// "true"), but build_config still runs and captures config_path
	// before that failure -- all this test needs.
	checker.CheckBatch(context.Background(), []probe.Options{jobOpts("a", 1)})

	got, err := os.ReadFile(capturedPath)
	if err != nil {
		t.Fatalf("expected build_config to have run and captured config_path, got %v", err)
	}
	if !strings.HasSuffix(string(got), ".json") {
		t.Fatalf("expected config_path to end in .json (engines like xray infer format from it), got %q", got)
	}
}

func TestPoolChecker_CheckBatch_TestsEveryJobAgainstItsOwnPort(t *testing.T) {
	m, _ := poolFixture(t, 10, 2)
	checker := module.NewChecker(m).(probe.BatchChecker)

	opts := []probe.Options{jobOpts("a", 1), jobOpts("b", 2), jobOpts("c", 3)}
	results := checker.CheckBatch(context.Background(), opts)

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	for i, r := range results {
		if !r.Ok {
			t.Fatalf("job %d: expected ok, got error %q", i, r.Error)
		}
		if r.Target != opts[i].Target {
			t.Fatalf("job %d: expected target %q, got %q", i, opts[i].Target, r.Target)
		}
		if r.Seq != opts[i].Seq {
			t.Fatalf("job %d: expected seq %d, got %d", i, opts[i].Seq, r.Seq)
		}
		if r.Extra["saw_target"] != opts[i].Target {
			t.Fatalf("job %d: expected run to see its own target %q, got %v", i, opts[i].Target, r.Extra["saw_target"])
		}
	}
}

func TestPoolChecker_CheckBatch_SplitsIntoMultipleSequentialInstances(t *testing.T) {
	m, logPath := poolFixture(t, 2, 2)
	checker := module.NewChecker(m).(probe.BatchChecker)

	opts := make([]probe.Options, 5)
	for i := range opts {
		opts[i] = jobOpts("t", i+1)
	}
	results := checker.CheckBatch(context.Background(), opts)

	if len(results) != 5 {
		t.Fatalf("expected 5 results, got %d", len(results))
	}
	for i, r := range results {
		if !r.Ok {
			t.Fatalf("job %d: expected ok, got error %q", i, r.Error)
		}
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// 5 jobs at 2 per instance must split into 3 instances (2+2+1) --
	// one build_config invocation, one log line, per instance.
	wantInstances := 3
	gotLines := 0
	for _, b := range log {
		if b == '\n' {
			gotLines++
		}
	}
	if gotLines != wantInstances {
		t.Fatalf("expected %d instances (build_config invocations), got %d (log: %q)", wantInstances, gotLines, log)
	}
}

func TestPoolChecker_CheckBatch_JobFailureIsIsolated(t *testing.T) {
	m, _ := poolFixture(t, 10, 2)
	checker := module.NewChecker(m).(probe.BatchChecker)

	opts := []probe.Options{jobOpts("ok-1", 1), jobOpts("fail", 2), jobOpts("ok-2", 3)}
	results := checker.CheckBatch(context.Background(), opts)

	if !results[0].Ok || !results[2].Ok {
		t.Fatalf("expected the two non-failing jobs to succeed, got %+v / %+v", results[0], results[2])
	}
	if results[1].Ok {
		t.Fatal("expected the deliberately failing job to fail")
	}
}

func TestPoolChecker_CheckBatch_BuildConfigFailureFailsWholeInstance(t *testing.T) {
	m := loadOne(t, `
name: pool-bad-build-config
run:
  command: ["echo", "unreached"]
collect:
  format: writeout_json
pool:
  max_jobs_per_instance: 10
  test_concurrency: 2
  build_config:
    command: ["sh", "-c", "exit 1"]
  start:
    command: ["sh", "-c", "exit 1"]
`)
	checker := module.NewChecker(m).(probe.BatchChecker)

	opts := []probe.Options{jobOpts("a", 1), jobOpts("b", 2)}
	results := checker.CheckBatch(context.Background(), opts)

	for i, r := range results {
		if r.Ok {
			t.Fatalf("job %d: expected build_config failure to fail every job in the instance, got ok", i)
		}
	}
}

func TestPoolChecker_CheckBatch_RequestSchemaValidatesEachJobIndependently(t *testing.T) {
	schemaModule := loadOneWithRunScriptRequiringParam(t)
	checker := module.NewChecker(schemaModule).(probe.BatchChecker)

	opts := []probe.Options{
		{Target: "a", Timeout: 5 * time.Second, Seq: 1, Params: map[string]any{"uuid": "x"}},
		{Target: "b", Timeout: 5 * time.Second, Seq: 2}, // missing required uuid
	}
	results := checker.CheckBatch(context.Background(), opts)

	if !results[0].Ok {
		t.Fatalf("expected job 0 (valid params) to succeed, got error %q", results[0].Error)
	}
	if results[1].Ok || results[1].ErrorCode != probe.ErrorCodeInvalidParams {
		t.Fatalf("expected job 1 (missing required param) to be invalid_params, got ok=%v code=%q", results[1].Ok, results[1].ErrorCode)
	}
}

func loadOneWithRunScriptRequiringParam(t *testing.T) module.Module {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()

	buildConfigScript := filepath.Join(dir, "build-config.py")
	mustWrite(t, buildConfigScript, `
import shutil, sys
shutil.copy(sys.argv[1], sys.argv[2])
`)
	startScript := filepath.Join(dir, "start.py")
	mustWrite(t, startScript, `
import sys, json, socket, threading, time

with open(sys.argv[1]) as f:
    jobs = json.load(f)

def serve(port):
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("127.0.0.1", port))
    s.listen(5)
    while True:
        conn, _ = s.accept()
        conn.close()

for j in jobs:
    threading.Thread(target=serve, args=(j["alloc_port"],), daemon=True).start()

while True:
    time.sleep(1)
`)
	runScript := filepath.Join(dir, "run.py")
	mustWrite(t, runScript, `
import socket, sys
port = int(sys.argv[1])
s = socket.create_connection(("127.0.0.1", port), timeout=2)
s.close()
print('{"latency_ms": 1}')
`)

	return loadOne(t, `
name: pool-schema-mod
request:
  - name: uuid
    type: string
    required: true
run:
  command: ["python3", "`+runScript+`", "{{alloc_port}}"]
collect:
  format: writeout_json
pool:
  max_jobs_per_instance: 10
  test_concurrency: 2
  build_config:
    command: ["python3", "`+buildConfigScript+`", "{{jobs_json}}", "{{config_path}}"]
  start:
    command: ["python3", "`+startScript+`", "{{config_path}}"]
`)
}
