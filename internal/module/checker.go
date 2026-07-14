package module

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/mehrnet/radar-node/internal/action"
	"github.com/mehrnet/radar-node/internal/portalloc"
	"github.com/mehrnet/radar-node/internal/probe"
)

// Checker adapts a Module into a probe.Checker. It has no warm pool
// yet -- every Check() runs prepare (if any), run, collect, and
// teardown as one synchronous sequence, then the prepare process (if
// started) is killed when ctx is cancelled at the end of Check().
// Reusing a long-lived prepared process across checks (the way the
// existing xray subprocess pool already works) is the next
// optimization once real load numbers justify the added complexity.
type Checker struct {
	m Module
}

func NewChecker(m Module) Checker { return Checker{m: m} }

func (c Checker) Type() string { return c.m.Name }

// Check validates opts.Params against the module's declared Request
// schema first, regardless of execution mode -- a mismatch short-
// circuits to probe.Invalid before any real work (subprocess or
// native action) is attempted. Action-based modules then call
// straight into the registered Go implementation, in-process; the
// rest of this method is the subprocess (`run:`) path.
func (c Checker) Check(ctx context.Context, opts probe.Options) probe.Result {
	if err := validateRequest(c.m.Request, opts.Params); err != nil {
		return probe.Invalid(c.Type(), opts.Target, opts.Mode, opts.Seq, err.Error())
	}

	if c.m.Action != "" {
		checker, ok := action.Get(c.m.Action)
		if !ok {
			// Unreachable for a Module that passed validate(), which
			// every loaded Module has -- kept as a safe fallback
			// rather than a panic.
			return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, fmt.Errorf("unknown action %q", c.m.Action))
		}
		r := checker.Check(ctx, opts)
		r.Type = c.Type() // report under the module's own name, not the action's
		return r
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	port, err := portalloc.Alloc()
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, fmt.Errorf("allocate port: %w", err))
	}

	paramsPath, cleanupParams, err := writeParamsJSON(opts.Params)
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, fmt.Errorf("write params_json: %w", err))
	}
	defer cleanupParams()

	ec := execContext{
		Target:         opts.Target,
		TimeoutMs:      opts.Timeout.Milliseconds(),
		Params:         opts.Params,
		ParamsJSONPath: paramsPath,
		AllocPort:      port,
	}

	if c.m.Prepare != nil {
		if err := c.startPrepare(ctx, ec, port); err != nil {
			return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, fmt.Errorf("prepare: %w", err))
		}
	}

	start := time.Now()
	stdout, err := c.runStep(ctx, *c.m.Run, ec)
	elapsed := time.Since(start)
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, fmt.Errorf("run: %w", err))
	}

	result, err := c.m.collect(stdout)
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, fmt.Errorf("collect: %w", err))
	}

	if c.m.Teardown != nil {
		if _, err := c.runStep(ctx, *c.m.Teardown, ec); err != nil {
			// Teardown failing doesn't invalidate a result we already
			// collected; the process is getting killed by ctx
			// cancellation regardless once Check() returns.
			result.Extra["teardown_error"] = err.Error()
		}
	}

	latencyMs := result.LatencyMs
	if latencyMs == 0 {
		latencyMs = float64(elapsed) / float64(time.Millisecond)
	}
	// Multiply while still a float, then truncate once -- doing it
	// the other way (time.Duration(latencyMs) * time.Millisecond)
	// truncates the fractional millisecond first and then amplifies
	// that rounding error by 1e6, e.g. silently turning 12.5ms into
	// 12ms.
	elapsedForResult := time.Duration(latencyMs * float64(time.Millisecond))
	return probe.Ok(c.Type(), opts.Target, opts.Mode, opts.Seq, elapsedForResult, result.Extra)
}

// startPrepare launches the prepare command detached from run's
// output, bound to ctx so it's killed when Check() returns, and
// waits until either alloc_port is accepting connections (typical
// for a module that starts a local proxy inbound) or a short
// readiness deadline elapses.
func (c Checker) startPrepare(ctx context.Context, ec execContext, port int) error {
	argv := ec.resolve(c.m.Prepare.Command)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // reap; ctx cancellation ends the process

	readinessCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return portalloc.WaitForPort(readinessCtx, port)
}

func (c Checker) runStep(ctx context.Context, step Step, ec execContext) ([]byte, error) {
	runCtx := ctx
	if step.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, step.Timeout)
		defer cancel()
	}
	argv := ec.resolve(step.Command)
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stderr.Len() > 0 {
			return nil, fmt.Errorf("%w: %s", err, bytes.TrimSpace(stderr.Bytes()))
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

func writeParamsJSON(params map[string]any) (string, func(), error) {
	f, err := os.CreateTemp("", "radar-mehrnet-params-*.json")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }

	enc := json.NewEncoder(f)
	if err := enc.Encode(params); err != nil {
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
