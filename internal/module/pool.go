package module

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/mehrnet/radar-node/internal/portalloc"
	"github.com/mehrnet/radar-node/internal/probe"
)

// PoolChecker adapts a pooled Module (m.Pool != nil, see PoolSpec) into
// a probe.BatchChecker. Unlike Checker, which runs one subprocess per
// check, PoolChecker groups a CheckBatch call's jobs into instances of
// up to Pool.MaxJobsPerInstance, and runs each instance's whole job
// batch through a single engine process: build_config writes one
// config describing every job in the instance, start launches the
// engine against it, each job is then tested (Run, Pool.TestConcurrency
// at a time) against its own already-running inbound, and stop (or
// ctx cancellation, if unset) tears the instance down before the next
// one starts. Instances run strictly sequentially -- never two engine
// processes for the same module at once -- since MaxJobsPerInstance
// is itself a proxy for "how many local ports/inbounds this VM can
// hold open at once", not a rate limit to parallelize past.
//
// There is no state carried between CheckBatch calls: every call
// (in practice, one per scheduler tick) rebuilds every instance's
// config fresh from exactly the jobs passed in, and every process
// started here is gone again before CheckBatch returns.
type PoolChecker struct {
	m Module
}

func newPoolChecker(m Module) PoolChecker { return PoolChecker{m: m} }

func (c PoolChecker) Type() string { return c.m.Name }

// Check runs opts as a batch of one -- the same engine lifecycle
// (build_config/start/stop) a real multi-job batch goes through, just
// for a single job. Used by the `probe` CLI and single triggered
// checks, neither of which has a batch to offer.
func (c PoolChecker) Check(ctx context.Context, opts probe.Options) probe.Result {
	results := c.CheckBatch(ctx, []probe.Options{opts})
	if len(results) == 0 {
		// Unreachable: CheckBatch always returns len(opts) results.
		return probe.Fail(c.Type(), opts.Target, opts.Seq, fmt.Errorf("pool: no result produced"))
	}
	return results[0]
}

// CheckBatch validates every opts entry against the module's declared
// Request schema up front (same gate Checker.Check applies per check),
// then splits whatever's left into sequential instances of up to
// Pool.MaxJobsPerInstance and runs each in turn. The returned slice is
// always len(opts), in the same order, regardless of how many
// instances that split into.
func (c PoolChecker) CheckBatch(ctx context.Context, opts []probe.Options) []probe.Result {
	results := make([]probe.Result, len(opts))
	if len(opts) == 0 {
		return results
	}

	for i, o := range opts {
		if err := validateRequest(c.m.Request, o.Params); err != nil {
			results[i] = probe.Invalid(c.Type(), o.Target, o.Seq, err.Error())
		}
	}

	maxJobs := c.m.Pool.MaxJobsPerInstance
	for start := 0; start < len(opts); start += maxJobs {
		end := start + maxJobs
		if end > len(opts) {
			end = len(opts)
		}
		c.runInstance(ctx, opts[start:end], results[start:end])
	}
	return results
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
func (c PoolChecker) runInstance(ctx context.Context, batch []probe.Options, out []probe.Result) {
	instanceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make([]poolJob, 0, len(batch))
	for i, o := range batch {
		if out[i].Error != "" || out[i].ErrorCode != "" {
			continue // already failed request validation in CheckBatch
		}
		port, err := portalloc.Alloc()
		if err != nil {
			out[i] = probe.Fail(c.Type(), o.Target, o.Seq, fmt.Errorf("allocate port: %w", err))
			continue
		}
		jobs = append(jobs, poolJob{idx: i, opts: o, port: port})
	}
	if len(jobs) == 0 {
		return
	}

	jobsPath, cleanupJobs, err := writeJobsJSON(jobs)
	if err != nil {
		failJobs(out, jobs, c.Type(), fmt.Errorf("write jobs_json: %w", err))
		return
	}
	defer cleanupJobs()

	// The .json suffix isn't decorative -- an engine like xray infers
	// its config file's format from this extension (see xray's own
	// "Failed to get format of ..." error otherwise), and build_config
	// only ever writes JSON here regardless of which engine it's for.
	configPath, cleanupConfig, err := reservePath("radar-node-pool-config-*.json")
	if err != nil {
		failJobs(out, jobs, c.Type(), fmt.Errorf("reserve config_path: %w", err))
		return
	}
	defer cleanupConfig()

	instanceEC := execContext{JobsJSONPath: jobsPath, ConfigPath: configPath}

	if _, err := runStep(instanceCtx, *c.m.Pool.BuildConfig, instanceEC); err != nil {
		failJobs(out, jobs, c.Type(), fmt.Errorf("pool build_config: %w", err))
		return
	}

	// Readiness is checked against the first job's own port only: a
	// pooled engine either comes up with every inbound it was
	// configured for or it doesn't start at all, so one representative
	// port is enough to know "the process is up" without waiting on
	// every job serially. The deadline is half of the longest job's
	// own timeout_ms, for the same reason Checker's Prepare readiness
	// already scales with timeout_ms rather than a flat constant (see
	// startLongLived) -- a slow-to-start engine under a big batch needs
	// proportionally more room, not a fixed cap indifferent to it.
	readinessTimeout := maxTimeout(jobs) / 2
	if err := startLongLived(instanceCtx, *c.m.Pool.Start, instanceEC, readinessTimeout, jobs[0].port); err != nil {
		failJobs(out, jobs, c.Type(), fmt.Errorf("pool start: %w", err))
		return
	}

	c.testJobs(instanceCtx, jobs, out)

	if c.m.Pool.Stop != nil {
		_, _ = runStep(instanceCtx, *c.m.Pool.Stop, instanceEC)
	}
}

// testJobs tests every job in one instance, Pool.TestConcurrency at a
// time, writing each result into out at that job's own idx.
func (c PoolChecker) testJobs(ctx context.Context, jobs []poolJob, out []probe.Result) {
	sem := make(chan struct{}, c.m.Pool.TestConcurrency)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j poolJob) {
			defer wg.Done()
			defer func() { <-sem }()
			out[j.idx] = c.testJob(ctx, j)
		}(j)
	}
	wg.Wait()
}

// testJob runs the module's Run step once against j's own already-
// running inbound (j.port) and collects its result -- the per-job
// equivalent of Checker.Check's own run+collect stage, sharing
// runAndCollect so the two can never drift on latency calculation or
// error wrapping.
func (c PoolChecker) testJob(ctx context.Context, j poolJob) probe.Result {
	opts := j.opts
	jobCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

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
