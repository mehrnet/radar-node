package module

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/mehrnet/radar-node/internal/destgate"
	"github.com/mehrnet/radar-node/internal/portalloc"
	"github.com/mehrnet/radar-node/internal/probe"
)

// PoolChecker adapts a pooled Module (m.Pool != nil, see PoolSpec)
// into a probe.BatchChecker. Unlike Checker, which runs one subprocess
// per check, PoolChecker groups a CheckBatch call's jobs into
// instances of up to Pool.MaxJobsPerInstance and runs each instance's
// whole job batch through a single engine process: build_config
// writes one config describing every job in the instance, start
// launches the engine against it, each job is then tested (Run,
// Pool.TestConcurrency at a time) against its own already-running
// inbound, and the engine keeps running for the NEXT batch too.
//
// Warm reuse. Instances used to be torn down after every CheckBatch
// call -- one engine boot plus one build_config subprocess per module
// per tick, forever, whether or not anything had changed. Now an
// instance is reused whenever the batch it would be built from is
// byte-identical to the one it was built from last time AND the
// engine is still healthy (process alive, every job's inbound port
// accepting). "Identical" is by construction, not approximation: jobs
// are canonicalized (sorted by a content hash) before slotting, so
// input order never matters, and each job's alloc_port is pinned to
// its content hash and kept across ticks -- the same job set produces
// the same jobs_json, the same config, and therefore exactly the
// engine that is already running. Anything else (a job added, a param
// edited, a probe deleted, the engine died or wedged) tears the old
// instance down and builds the new one, exactly as the cold path did.
//
// Concurrency within one module: slots are handed out under a mutex
// and tests run outside it, so two overlapping CheckBatch calls (a
// scheduled tick plus a triggered check) share a warm engine safely;
// a teardown that arrives while another call is still testing against
// that engine is deferred until the last tester finishes, rather than
// killing the process under it.
//
// The engine's lifetime is deliberately independent of any request
// context: it is bound to its own cancelable context plus a parent-
// death signal (Linux), so it survives between ticks, dies with the
// agent, and is stopped either by the module's own stop step (if it
// declares one) or by SIGKILL on teardown. Close() stops every warm
// engine and is what the agent's shutdown path calls.
type PoolChecker struct {
	m Module

	mu    sync.Mutex
	slots map[int]*warmInstance
	// ports pins a job's content hash to its alloc_port so the same
	// job maps to the same inbound across ticks -- the keystone of
	// "same job set => same jobs_json => reuse the running engine".
	// portalloc.Alloc is stateless, so releasing a port is just
	// deleting its entry here.
	ports map[string]int
}

type warmInstance struct {
	jobsKey    string
	jobs       []poolJob
	jobsPath   string
	configPath string
	engineCtx  context.Context
	cancel     context.CancelFunc
	engine     *startedEngine
	refs       int
	retired    bool
	stopped    bool
}

func newPoolChecker(m Module) *PoolChecker {
	return &PoolChecker{m: m, slots: map[int]*warmInstance{}, ports: map[string]int{}}
}

func (c *PoolChecker) Type() string { return c.m.Name }

// Check runs opts as a batch of one -- through the same warm-instance
// machinery a real batch goes through, so even a single triggered
// check reuses a running engine when one matches. Used by the `probe`
// CLI and single triggered checks.
func (c *PoolChecker) Check(ctx context.Context, opts probe.Options) probe.Result {
	results := c.CheckBatch(ctx, []probe.Options{opts})
	if len(results) == 0 {
		// Unreachable: CheckBatch always returns len(opts) results.
		return probe.Fail(c.Type(), opts.Target, opts.Seq, fmt.Errorf("pool: no result produced"))
	}
	return results[0]
}

// CheckBatch validates every opts entry against the module's declared
// Request schema up front (same gate Checker.Check applies per check),
// canonicalizes what's left, and runs it through the warm slots. The
// returned slice is always len(opts), in the same order as the input,
// regardless of how the canonical sort rearranged anything internally.
func (c *PoolChecker) CheckBatch(ctx context.Context, opts []probe.Options) []probe.Result {
	results := make([]probe.Result, len(opts))
	if len(opts) == 0 {
		return results
	}

	pending := make([]pendingJob, 0, len(opts))
	for i, o := range opts {
		if err := validateRequest(c.m.Request, o.Params); err != nil {
			results[i] = probe.Invalid(c.Type(), o.Target, o.Seq, err.Error())
			continue
		}
		pending = append(pending, pendingJob{idx: i, key: jobKey(o), o: o})
	}
	if len(pending) == 0 {
		return results
	}

	// Canonical order: sorted by content hash, so the same set of jobs
	// always lands in the same slots in the same order whatever order
	// the scheduler happened to deliver them in -- and the jobs_json a
	// warm instance was built from stays byte-identical across ticks.
	sort.Slice(pending, func(i, j int) bool { return pending[i].key < pending[j].key })

	// Ports for jobs that have left this batch are released back
	// (portalloc is stateless; dropping the pin is the release).
	live := make(map[string]bool, len(pending))
	for _, p := range pending {
		live[p.key] = true
	}

	maxJobs := c.m.Pool.MaxJobsPerInstance
	for start, slot := 0, 0; start < len(pending); start, slot = start+maxJobs, slot+1 {
		end := start + maxJobs
		if end > len(pending) {
			end = len(pending)
		}
		c.runSlot(ctx, slot, pending[start:end], live, results)
	}
	return results
}

// pendingJob is one validated, not-yet-slotted job: its index in the
// caller's opts/results, its content-hash identity, and the options
// themselves.
type pendingJob struct {
	idx int
	key string
	o   probe.Options
}

// jobKey is a job's stable content identity: target, timeout and the
// full params, serialized deterministically (json.Marshal sorts map
// keys). Everything build_config consumes -- everything jobs_json
// carries -- is covered by it.
func jobKey(o probe.Options) string {
	paramsJSON, _ := json.Marshal(o.Params)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s", o.Target, o.Timeout.Milliseconds(), paramsJSON)))
	return hex.EncodeToString(sum[:])
}

// hashJobs is the canonical batch's identity: the exact jobs_json
// bytes this slot would be built from, ports included.
func hashJobs(jobs []poolJob) string {
	h := sha256.New()
	for _, j := range jobs {
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00%s\n", j.opts.Target, j.opts.Timeout.Milliseconds(), j.port, mustParamsJSON(j.opts.Params))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func mustParamsJSON(params map[string]any) string {
	b, err := json.Marshal(params)
	if err != nil {
		return ""
	}
	return string(b)
}

// runSlot runs one slot's jobs against its warm instance, building
// (or rebuilding) the engine only when the instance is missing,
// stale, or unhealthy. Building happens under c.mu (slot and port
// state); testing happens outside it, holding only a reference that
// defers any teardown the next rebuild might want to do.
func (c *PoolChecker) runSlot(ctx context.Context, slot int, chunk []pendingJob, live map[string]bool, results []probe.Result) {
	c.mu.Lock()

	jobs := make([]poolJob, len(chunk))
	for i, p := range chunk {
		port, ok := c.ports[p.key]
		if !ok {
			var err error
			port, err = portalloc.Alloc()
			if err != nil {
				c.mu.Unlock()
				for _, q := range chunk {
					results[q.idx] = probe.Fail(c.Type(), q.o.Target, q.o.Seq, fmt.Errorf("allocate port: %w", err))
				}
				return
			}
			c.ports[p.key] = port
		}
		jobs[i] = poolJob{idx: p.idx, opts: p.o, port: port}
	}
	for k := range c.ports {
		if !live[k] {
			delete(c.ports, k)
		}
	}
	key := hashJobs(jobs)

	w := c.slots[slot]
	if w != nil && !w.retired && w.jobsKey == key && c.healthyLocked(w) {
		w.refs++
		c.mu.Unlock()
		c.testJobs(ctx, jobs, results)
		c.releaseSlot(slot)
		return
	}
	if w != nil {
		w.retired = true
		if w.refs == 0 {
			c.teardownLocked(w)
			delete(c.slots, slot)
		}
		// A retire in flight (refs > 0) leaves the slot mapped and
		// retired; the final releaseSlot tears it down and clears it,
		// and this rebuild takes its place under a fresh slot entry.
	}
	built, err := c.buildAndStartLocked(jobs, key)
	if err != nil {
		c.mu.Unlock()
		for _, j := range jobs {
			results[j.idx] = probe.Fail(c.Type(), j.opts.Target, j.opts.Seq, err)
		}
		return
	}
	built.refs = 1
	c.slots[slot] = built
	c.mu.Unlock()

	c.testJobs(ctx, jobs, results)
	c.releaseSlot(slot)
}

// releaseSlot drops one tester's reference to a slot's instance,
// tearing it down if it was retired while still in use.
func (c *PoolChecker) releaseSlot(slot int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.slots[slot]
	if w == nil {
		return
	}
	w.refs--
	if w.retired && w.refs <= 0 {
		c.teardownLocked(w)
		delete(c.slots, slot)
	}
}

// healthyLocked reports whether a warm instance can serve this batch:
// the engine process has not exited, and every job's inbound port
// still accepts a connection. Local dials -- microseconds each -- so
// checking all of them costs nothing next to the engine reboot it
// avoids.
func (c *PoolChecker) healthyLocked(w *warmInstance) bool {
	select {
	case <-w.engine.done:
		return false
	default:
	}
	for _, j := range w.jobs {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", j.port), 100*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
	}
	return true
}

// teardownLocked stops a warm instance: the module's own stop step
// first (if declared), then SIGKILL via the engine context, then the
// reaped process's temp files.
func (c *PoolChecker) teardownLocked(w *warmInstance) {
	if w.stopped {
		return
	}
	w.stopped = true
	if c.m.Pool.Stop != nil {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, _ = runStep(sctx, *c.m.Pool.Stop, execContext{JobsJSONPath: w.jobsPath, ConfigPath: w.configPath})
		cancel()
	}
	w.cancel()
	<-w.engine.done
	_ = os.Remove(w.jobsPath)
	_ = os.Remove(w.configPath)
}

// buildAndStartLocked is the cold path: write jobs_json, run
// build_config, start the engine on its own long-lived context and
// wait for readiness. Callers must hold c.mu.
func (c *PoolChecker) buildAndStartLocked(jobs []poolJob, key string) (*warmInstance, error) {
	jobsPath, cleanupJobs, err := writeJobsJSON(jobs)
	if err != nil {
		return nil, fmt.Errorf("write jobs_json: %w", err)
	}
	// The .json suffix isn't decorative -- an engine like xray infers
	// its config file's format from this extension (see xray's own
	// "Failed to get format of ..." error otherwise), and build_config
	// only ever writes JSON here regardless of which engine it's for.
	configPath, cleanupConfig, err := reservePath("radar-node-pool-config-*.json")
	if err != nil {
		cleanupJobs()
		return nil, fmt.Errorf("reserve config_path: %w", err)
	}
	ec := execContext{JobsJSONPath: jobsPath, ConfigPath: configPath}

	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelBuild()
	if _, err := runStep(buildCtx, *c.m.Pool.BuildConfig, ec); err != nil {
		cleanupJobs()
		cleanupConfig()
		return nil, fmt.Errorf("pool build_config: %w", err)
	}

	// The engine's context is deliberately NOT the request's: a warm
	// engine must outlive the CheckBatch call that (re)built it. Its
	// lifetime ends at teardown or Close, and Pdeathsig covers the
	// parent dying first.
	engineCtx, cancelEngine := context.WithCancel(context.Background())
	// Readiness is checked against the first job's own port only: a
	// pooled engine either comes up with every inbound it was
	// configured for or it doesn't start at all -- same reasoning the
	// cold path always used, with the same timeout scaling.
	readinessTimeout := maxTimeout(jobs) / 2
	engine, err := startEngine(engineCtx, *c.m.Pool.Start, ec, readinessTimeout, jobs[0].port)
	if err != nil {
		cancelEngine()
		cleanupJobs()
		cleanupConfig()
		return nil, fmt.Errorf("pool start: %w", err)
	}

	return &warmInstance{
		jobsKey:    key,
		jobs:       jobs,
		jobsPath:   jobsPath,
		configPath: configPath,
		engineCtx:  engineCtx,
		cancel:     cancelEngine,
		engine:     engine,
	}, nil
}

// Close stops every warm instance this checker owns. Called from the
// agent's shutdown path; safe to call more than once.
func (c *PoolChecker) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for slot, w := range c.slots {
		if w.refs == 0 {
			c.teardownLocked(w)
			delete(c.slots, slot)
		} else {
			w.retired = true // torn down by the last in-flight tester
		}
	}
}

// poolJob is one instance's-worth of a single opts entry, carrying the
// port allocated for it -- everything build_config needs to describe
// this job in the engine config it writes, and everything the later
// per-job Run step needs to test it.
type poolJob struct {
	idx  int // index into this instance's own batch/out slices (runInstance's params), not CheckBatch's full opts
	opts probe.Options
	port int
}

// jobJSON is poolJob's on-disk shape for {{jobs_json}} -- a build_config
// script's only view into the batch, so every field an engine config
// could plausibly need is included rather than guessing which subset
// a not-yet-written module will actually use.
type jobJSON struct {
	Index     int            `json:"index"`
	Target    string         `json:"target"`
	AllocPort int            `json:"alloc_port"`
	TimeoutMs int64          `json:"timeout_ms"`
	Params    map[string]any `json:"params,omitempty"`
}

// runInstance runs one instance's full lifecycle -- allocate a port
// per job, build the engine config, start the engine, test every job,
// stop the engine -- writing each job's probe.Result into out (indexed
// the same as batch). A failure at the build_config/start stage fails
// every job in the instance identically, since none of them ever got
// tested; a per-job failure during testing only fails that one job.
func (c *PoolChecker) testJobs(ctx context.Context, jobs []poolJob, out []probe.Result) {
	sem := make(chan struct{}, c.m.Pool.TestConcurrency)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j poolJob) {
			defer wg.Done()
			out[j.idx] = c.testJob(ctx, sem, j)
		}(j)
	}
	wg.Wait()
}

// testJob runs the module's Run step once against j's own already-
// running inbound (j.port) and collects its result -- the per-job
// equivalent of Checker.Check's own run+collect stage, sharing
// runAndCollect so the two can never drift on latency calculation or
// error wrapping.
//
// Waits for j's own destination to be clear (destgate.Wait) *before*
// taking a slot from sem, not after -- taking sem first would let one
// busy destination's queue of blocked jobs exhaust this instance's
// whole TestConcurrency budget, starving jobs against destinations
// that aren't busy at all.
//
// The wait is deliberately its own budget (destgate's own configured
// maxWait), not opts.Timeout -- confirmed in production that a
// heavily-shared destination (dozens of jobs resolving to one
// physical host) with a short opts.Timeout meant only the very first
// job per floor window could ever get a real attempt in, and every
// other one failed waiting, every cycle, permanently. Once the wait
// clears, the job gets a full, fresh opts.Timeout-scoped budget of
// its own to actually run in.
func (c *PoolChecker) testJob(ctx context.Context, sem chan struct{}, j poolJob) probe.Result {
	opts := j.opts

	if err := destgate.Wait(ctx, opts.Destination); err != nil {
		// Skip, not Fail -- never actually ran, so nothing to report at
		// all (see probe.Result.Skip's own doc comment); Target/Type
		// are still set purely so the caller (internal/agent, the one
		// place in this codebase that actually logs -- this package
		// deliberately never does) can log something useful about which
		// job this was.
		return probe.Result{Skip: true, Type: c.Type(), Target: opts.Target, Seq: opts.Seq}
	}
	jobCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	sem <- struct{}{}
	defer func() { <-sem }()

	paramsPath, cleanup, err := writeParamsJSON(opts.Params)
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Seq, fmt.Errorf("write params_json: %w", err))
	}
	defer cleanup()

	ec := execContext{
		Target:         opts.Target,
		TimeoutMs:      opts.Timeout.Milliseconds(),
		Params:         opts.Params,
		ParamsJSONPath: paramsPath,
		AllocPort:      j.port,
	}
	return c.m.runAndCollect(jobCtx, *c.m.Run, ec, opts)
}

// failJobs fails every job in jobs identically -- used when an
// instance never got far enough (build_config/start) for any
// individual job to be meaningfully attempted.
func failJobs(out []probe.Result, jobs []poolJob, checkType string, err error) {
	for _, j := range jobs {
		out[j.idx] = probe.Fail(checkType, j.opts.Target, j.opts.Seq, err)
	}
}

// maxTimeout returns the longest Options.Timeout among jobs, or a 5s
// floor if somehow all are zero -- mirroring Checker's own timeout
// fallback (see agent.runCheck) so a pool instance never gets a zero-
// length readiness deadline.
func maxTimeout(jobs []poolJob) time.Duration {
	var max time.Duration
	for _, j := range jobs {
		if j.opts.Timeout > max {
			max = j.opts.Timeout
		}
	}
	if max <= 0 {
		max = 5 * time.Second
	}
	return max
}

func writeJobsJSON(jobs []poolJob) (string, func(), error) {
	entries := make([]jobJSON, len(jobs))
	for i, j := range jobs {
		entries[i] = jobJSON{
			Index:     j.idx,
			Target:    j.opts.Target,
			AllocPort: j.port,
			TimeoutMs: j.opts.Timeout.Milliseconds(),
			Params:    j.opts.Params,
		}
	}

	f, err := os.CreateTemp("", "radar-node-pool-jobs-*.json")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }

	enc := json.NewEncoder(f)
	if err := enc.Encode(entries); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return f.Name(), cleanup, nil
}

// reservePath allocates a unique, currently-empty file path for
// build_config to write its engine config to -- CreateTemp is used
// purely for its collision-free naming; the file itself is closed
// (and left empty) immediately, since build_config is what actually
// writes the real content.
func reservePath(pattern string) (string, func(), error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", nil, err
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}
