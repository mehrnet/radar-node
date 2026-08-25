package fetch

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/itchyny/gojq"
)

// A field's own extraction pipeline is capped short -- these exist to
// keep a misconfigured or hostile probe's params from turning into a
// real resource-exhaustion lever against this node, same reasoning as
// resultSchema's own MAX_EXTRA_FIELD_COUNT on the radar-api side.
const (
	maxFields        = 20
	maxStepsPerField = 5
	maxExprLen       = 500
)

// fieldStep is one stage of a field's own extraction pipeline -- a
// "fields.<name>" param value is either a single {"parser":...,
// "expr":...} object, or an array of them for a multi-step chain (e.g.
// base64-decode the whole body, then jq-select a key out of the
// decoded JSON), applied in order with each step's output feeding the
// next step's input.
type fieldStep struct {
	Parser string
	Expr   string
}

// parseFieldSteps normalizes a raw "fields.<name>" param value into a
// []fieldStep, rejecting anything malformed (not an object/array,
// missing/unrecognized parser, an expr too long, a chain too long or
// empty) as a config error the caller treats as "this one field
// doesn't populate," never an engine crash -- see Check's own doc
// comment on why a bad field is omitted rather than failing the whole
// check.
func parseFieldSteps(raw any) ([]fieldStep, error) {
	switch v := raw.(type) {
	case map[string]any:
		step, err := decodeStep(v)
		if err != nil {
			return nil, err
		}
		return []fieldStep{step}, nil
	case []any:
		if len(v) == 0 {
			return nil, fmt.Errorf("empty step chain")
		}
		if len(v) > maxStepsPerField {
			return nil, fmt.Errorf("chain exceeds %d steps", maxStepsPerField)
		}
		steps := make([]fieldStep, len(v))
		for i, raw := range v {
			m, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("step %d must be an object", i)
			}
			step, err := decodeStep(m)
			if err != nil {
				return nil, err
			}
			steps[i] = step
		}
		return steps, nil
	default:
		return nil, fmt.Errorf("must be an object or array of objects")
	}
}

func decodeStep(m map[string]any) (fieldStep, error) {
	parser, _ := m["parser"].(string)
	switch parser {
	case "base64", "jq", "regex":
	case "":
		return fieldStep{}, fmt.Errorf("missing \"parser\"")
	default:
		return fieldStep{}, fmt.Errorf("unsupported parser %q -- expected base64, jq, or regex", parser)
	}
	expr, _ := m["expr"].(string)
	if len(expr) > maxExprLen {
		return fieldStep{}, fmt.Errorf("expr exceeds %d characters", maxExprLen)
	}
	return fieldStep{Parser: parser, Expr: expr}, nil
}

// runPipeline applies steps in order to body, returning a value safe
// to place directly into a check's own Extra.
func runPipeline(ctx context.Context, steps []fieldStep, body []byte) (any, error) {
	var value any = body
	for _, step := range steps {
		var err error
		value, err = runStep(ctx, step, value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", step.Parser, err)
		}
	}
	return valueForField(value), nil
}

func runStep(ctx context.Context, step fieldStep, value any) (any, error) {
	switch step.Parser {
	case "base64":
		return applyBase64(value)
	case "jq":
		return applyJQ(ctx, step.Expr, value)
	case "regex":
		return applyRegex(step.Expr, value)
	default:
		return nil, fmt.Errorf("unsupported parser %q", step.Parser) // unreachable, decodeStep already validated this
	}
}

// toBytes coerces a step's own input into bytes -- either the
// previous step's raw output, or (a prior jq step having produced a
// structured, non-string value) a fresh JSON encoding of it, the same
// representation a real HTTP response body would have had.
func toBytes(value any) ([]byte, error) {
	switch v := value.(type) {
	case []byte:
		return v, nil
	case string:
		return []byte(v), nil
	default:
		return json.Marshal(v)
	}
}

func applyBase64(value any) (any, error) {
	b, err := toBytes(value)
	if err != nil {
		return nil, err
	}
	trimmed := string(bytes.TrimSpace(b))
	// Subscription-style base64 blobs in the wild use inconsistent
	// padding/charset conventions -- try every real variant rather
	// than assuming one.
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if decoded, err := enc.DecodeString(trimmed); err == nil {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("not valid base64")
}

func applyJQ(ctx context.Context, expr string, value any) (any, error) {
	if expr == "" {
		return nil, fmt.Errorf("missing \"expr\"")
	}
	b, err := toBytes(value)
	if err != nil {
		return nil, err
	}
	var input any
	if err := json.Unmarshal(b, &input); err != nil {
		return nil, fmt.Errorf("input isn't valid JSON: %w", err)
	}
	query, err := gojq.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid expr: %w", err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		return nil, fmt.Errorf("invalid expr: %w", err)
	}
	// Bounded by ctx, the same overall per-check timeout budget
	// everything else in this pipeline shares -- a query already
	// spent most of that budget on the HTTP fetch itself gets
	// correspondingly less room to run, not a fresh timeout of its own.
	iter := code.RunWithContext(ctx, input)
	v, ok := iter.Next()
	if !ok {
		return nil, fmt.Errorf("produced no output")
	}
	if err, ok := v.(error); ok {
		return nil, err
	}
	// jq's own default behavior for a missing key/index is to return
	// null, not to error (".nonexistent" on {} is a completely normal,
	// successful jq evaluation) -- but this pipeline's own contract is
	// "a field with nothing to report is omitted, never sent as an
	// explicit null" (see Check's own doc comment), so a null result is
	// treated the same as a genuine extraction failure here.
	if v == nil {
		return nil, fmt.Errorf("produced null")
	}
	return v, nil
}

func applyRegex(expr string, value any) (any, error) {
	if expr == "" {
		return nil, fmt.Errorf("missing \"expr\"")
	}
	b, err := toBytes(value)
	if err != nil {
		return nil, err
	}
	// Go's regexp is RE2-backed -- linear-time by construction, so an
	// account-supplied pattern (remote, untrusted input reaching a
	// BYO node) can't become a ReDoS lever the way a backtracking
	// engine's could.
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid expr: %w", err)
	}
	match := re.FindSubmatch(b)
	if match == nil {
		return nil, fmt.Errorf("no match")
	}
	// The first capture group if the pattern has one, else the whole match.
	if len(match) > 1 {
		return string(match[1]), nil
	}
	return string(match[0]), nil
}

// valueForField converts a pipeline's final value into something safe
// to marshal into a check's own Extra -- specifically, never a raw
// []byte, which encoding/json would silently base64-encode *again* on
// top of whatever decoding this pipeline already did, reading as
// garbled to anyone looking at the real check data instead of the
// plain decoded text it actually is.
func valueForField(value any) any {
	if b, ok := value.([]byte); ok {
		return string(b)
	}
	return value
}
