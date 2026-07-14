package module

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
)

// collectResult is the format-agnostic outcome of parsing a run
// step's stdout, before it's wrapped into a probe.Result.
type collectResult struct {
	LatencyMs float64
	Extra     map[string]any
}

func (m *Module) collect(stdout []byte) (collectResult, error) {
	switch m.Collect.Format {
	case "writeout_json":
		return collectWriteoutJSON(stdout)
	case "regex":
		return collectRegex(m.compiledPattern, stdout)
	default:
		// Unreachable for a Module that passed validate().
		return collectResult{}, fmt.Errorf("unknown collect format %q", m.Collect.Format)
	}
}

// collectWriteoutJSON expects stdout to be exactly one JSON object
// with at least a numeric "latency_ms" key (mirrors curl -w's JSON
// writeout format, the pattern the existing checkray Python code
// already uses for its own curl invocations). Every key becomes
// Result.Extra, including latency_ms itself, so nothing is lost even
// though it's also surfaced as the top-level field.
func collectWriteoutJSON(stdout []byte) (collectResult, error) {
	var extra map[string]any
	if err := json.Unmarshal(stdout, &extra); err != nil {
		return collectResult{}, fmt.Errorf("parse writeout_json stdout: %w", err)
	}
	latency, ok := extra["latency_ms"].(float64)
	if !ok {
		return collectResult{}, fmt.Errorf("writeout_json stdout missing numeric \"latency_ms\" key")
	}
	return collectResult{LatencyMs: latency, Extra: extra}, nil
}

// collectRegex applies re to stdout; the required "latency_ms" named
// group is parsed as a float, every other named group becomes a
// string in Extra.
func collectRegex(re *regexp.Regexp, stdout []byte) (collectResult, error) {
	match := re.FindSubmatch(stdout)
	if match == nil {
		return collectResult{}, fmt.Errorf("collect.pattern did not match run output")
	}

	extra := map[string]any{}
	var latencyMs float64
	var haveLatency bool
	for i, name := range re.SubexpNames() {
		if i == 0 || name == "" {
			continue
		}
		value := string(match[i])
		if name == "latency_ms" {
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return collectResult{}, fmt.Errorf("latency_ms group %q is not a number: %w", value, err)
			}
			latencyMs = v
			haveLatency = true
			continue
		}
		extra[name] = value
	}
	if !haveLatency {
		// Unreachable for a Module that passed validate(), which
		// requires the pattern to declare this group.
		return collectResult{}, fmt.Errorf("collect.pattern matched but produced no latency_ms")
	}
	return collectResult{LatencyMs: latencyMs, Extra: extra}, nil
}
