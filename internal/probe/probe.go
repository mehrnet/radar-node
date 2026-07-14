// Package probe defines the shared types every check implementation
// (native or, later, custom-module driven) must produce and consume.
package probe

import (
	"context"
	"time"
)

// Mode selects how a check should be performed when the underlying
// protocol supports it. Warm favors realistic steady-state numbers
// (e.g. reused connections); Hard measures a cold path from nothing.
type Mode string

const (
	ModeWarm Mode = "warm"
	ModeHard Mode = "hard"
)

// Options carries everything a Checker needs to run a single probe
// against a single target.
type Options struct {
	Target  string
	Timeout time.Duration
	Mode    Mode
	// Seq is the 1-indexed attempt number when Count > 1, so results
	// stay identifiable once flattened into a single output array.
	Seq int
	// Params carries check-specific parameters (DNS record type, DNS
	// server override, HTTP method, an entire structured proxy
	// config for a custom module, ...) without forcing every Checker
	// to share one bloated options struct. Typed any (not
	// map[string]string) because a custom module's {{params_json}}
	// placeholder needs the true, possibly-nested value a job
	// supplied -- flattening to strings here would silently defeat
	// that mechanism for every custom module, even though native
	// checks only ever read scalar string values out of it via
	// Param().
	Params map[string]any
}

// Param returns opts.Params[key] as a string, or def if unset or not
// a plain string. This is what every native Checker uses; a custom
// module instead receives the full Params value verbatim through its
// {{params_json}} placeholder, see internal/module.
func (o Options) Param(key, def string) string {
	if v, ok := o.Params[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return def
}

// Result is the one canonical shape every check type must produce,
// native or custom. Extra carries anything check-specific that
// doesn't belong in the fixed fields (e.g. resolved DNS records,
// HTTP status code, TLS/DNS/connect timing breakdown).
type Result struct {
	Ok        bool     `json:"ok"`
	Type      string   `json:"type"`
	Target    string   `json:"target"`
	Mode      Mode     `json:"mode,omitempty"`
	Seq       int      `json:"seq,omitempty"`
	LatencyMs *float64 `json:"latency_ms,omitempty"`
	Error     string   `json:"error,omitempty"`
	// ErrorCode is a small, closed, machine-parseable enum -- unset
	// for a normal probe failure (network error, timeout, ...), set
	// to ErrorCodeInvalidParams when a job's request didn't match its
	// module's declared request schema and the probe was never
	// actually attempted. Lets a UI distinguish "this is
	// misconfigured" from "the target is actually down" instead of
	// pattern-matching the free-text Error string.
	ErrorCode string         `json:"error_code,omitempty"`
	Extra     map[string]any `json:"extra,omitempty"`
}

// ErrorCodeInvalidParams marks a Result produced by request-schema
// validation rejecting a job before any real probe/action ran.
const ErrorCodeInvalidParams = "invalid_params"

// Envelope is the top-level shape printed by the CLI: a single ok
// flag summarizing the whole run, plus every individual result.
type Envelope struct {
	Ok      bool     `json:"ok"`
	Results []Result `json:"results"`
}

// Checker performs one probe type. Implementations must respect
// ctx cancellation/deadline on every blocking operation -- targets
// are attacker-controlled input on public nodes, so a Checker that
// ignores ctx is a resource-exhaustion bug, not just a bug.
type Checker interface {
	// Type returns the stable name used in --type and in Result.Type.
	Type() string
	Check(ctx context.Context, opts Options) Result
}

func latency(d time.Duration) *float64 {
	ms := float64(d) / float64(time.Millisecond)
	return &ms
}

// Ok builds a successful Result with a measured latency.
func Ok(checkType, target string, mode Mode, seq int, elapsed time.Duration, extra map[string]any) Result {
	return Result{
		Ok:        true,
		Type:      checkType,
		Target:    target,
		Mode:      mode,
		Seq:       seq,
		LatencyMs: latency(elapsed),
		Extra:     extra,
	}
}

// Fail builds a failed Result. elapsed may be zero if the failure
// happened before any timing was meaningful to report.
func Fail(checkType, target string, mode Mode, seq int, err error) Result {
	return Result{
		Ok:     false,
		Type:   checkType,
		Target: target,
		Mode:   mode,
		Seq:    seq,
		Error:  err.Error(),
	}
}

// Invalid builds a failed Result for a job whose params didn't match
// its module's declared request schema -- no probe/action was ever
// attempted, unlike Fail which always represents a real attempt that
// didn't succeed.
func Invalid(checkType, target string, mode Mode, seq int, msg string) Result {
	return Result{
		Ok:        false,
		Type:      checkType,
		Target:    target,
		Mode:      mode,
		Seq:       seq,
		Error:     msg,
		ErrorCode: ErrorCodeInvalidParams,
	}
}
