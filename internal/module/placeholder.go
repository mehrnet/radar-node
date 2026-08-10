package module

import (
	"fmt"
	"regexp"
	"strconv"
)

// stringifyParam renders a Params value for {{param.<name>}}
// substitution. Plain strings pass through as-is; anything else
// (numbers, bools, or a nested object/array a module author
// shouldn't be using this placeholder for) falls back to a default
// Go representation rather than failing the check -- {{params_json}}
// is the intended placeholder for structured values.
func stringifyParam(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// placeholderRe matches any {{...}} token so validatePlaceholders can
// enumerate every one an argument contains, including multiple in a
// single arg (e.g. "socks5h://127.0.0.1:{{alloc_port}}").
var placeholderRe = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_.]+)\s*\}\}`)

var fixedPlaceholders = map[string]bool{
	"target":      true,
	"timeout_ms":  true,
	"params_json": true,
	"alloc_port":  true,
	// mode is the probe's own probe.Mode ("warm" or "hard"), passed
	// through verbatim -- a generic escape hatch for any module whose
	// engine can meaningfully behave differently for "realistic
	// steady-state" vs. "cold path from nothing" (see probe.Mode's own
	// doc comment), the same way xray's run step uses it to warm up a
	// muxed connection before the timed request rather than measuring
	// a fresh handshake every single check. Left for the module to
	// interpret or ignore entirely -- nothing here enforces any
	// particular behavior per mode.
	"mode": true,
	// jobs_json/config_path are pool-only (see PoolSpec in module.go):
	// jobs_json is the path to this instance's whole job batch, given
	// to build_config; config_path is where build_config writes the
	// engine config that start then reads. Both are validated here
	// alongside the non-pool placeholders rather than restricted to
	// only appear in pool steps -- the same looseness prepare/run/
	// teardown already have with each other (e.g. alloc_port is valid
	// in teardown even though no module actually uses it there).
	"jobs_json":   true,
	"config_path": true,
}

var paramPlaceholderRe = regexp.MustCompile(`^param\.[a-zA-Z0-9_-]+$`)

// validatePlaceholders rejects any {{...}} token that isn't one of
// the fixed names or a well-formed "param.<name>" reference. This is
// the load-time half of the trust boundary: an unrecognized
// placeholder fails config loading (and therefore process start),
// not a running check.
func validatePlaceholders(arg string) error {
	for _, match := range placeholderRe.FindAllStringSubmatch(arg, -1) {
		name := match[1]
		if fixedPlaceholders[name] || paramPlaceholderRe.MatchString(name) {
			continue
		}
		return fmt.Errorf("unrecognized placeholder {{%s}}", name)
	}
	return nil
}

// execContext carries the resolved values for one Check() call, or,
// for a pooled module (see pool.go), one instance-level step
// (JobsJSONPath/ConfigPath, Target/Params/AllocPort left zero) or one
// per-job Run step (Target/Params/AllocPort, JobsJSONPath/ConfigPath
// left empty) -- never a mix of both within the same execContext.
type execContext struct {
	Target         string
	TimeoutMs      int64
	Mode           string
	Params         map[string]any
	ParamsJSONPath string
	AllocPort      int
	// JobsJSONPath/ConfigPath back {{jobs_json}}/{{config_path}},
	// pool-only placeholders (see PoolSpec in module.go) resolved only
	// for a pool's build_config/start/stop steps.
	JobsJSONPath string
	ConfigPath   string
}

// resolve substitutes every {{...}} in argv against ec. Any
// placeholder not satisfiable from ec (e.g. {{param.sni}} when no
// "sni" param was supplied) resolves to an empty string -- modules
// are expected to declare sane defaults in their own command
// (equivalent to how a shell script would handle an unset var),
// since the alternative (hard error) would let a probe's choice of
// which params to include change whether a module even runs.
func (ec execContext) resolve(argv []string) []string {
	out := make([]string, len(argv))
	for i, arg := range argv {
		out[i] = placeholderRe.ReplaceAllStringFunc(arg, func(token string) string {
			name := placeholderRe.FindStringSubmatch(token)[1]
			switch {
			case name == "target":
				return ec.Target
			case name == "timeout_ms":
				return strconv.FormatInt(ec.TimeoutMs, 10)
			case name == "params_json":
				return ec.ParamsJSONPath
			case name == "alloc_port":
				return strconv.Itoa(ec.AllocPort)
			case name == "mode":
				return ec.Mode
			case name == "jobs_json":
				return ec.JobsJSONPath
			case name == "config_path":
				return ec.ConfigPath
			case paramPlaceholderRe.MatchString(name):
				return stringifyParam(ec.Params[name[len("param."):]])
			default:
				// Unreachable for a Module that passed validate(),
				// which every loaded Module has -- kept as a safe
				// fallback rather than a panic.
				return token
			}
		})
	}
	return out
}
