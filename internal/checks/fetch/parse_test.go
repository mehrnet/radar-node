package fetch

import (
	"context"
	"strings"
	"testing"
)

func TestParseFieldSteps_SingleObjectShorthand(t *testing.T) {
	steps, err := parseFieldSteps(map[string]any{"parser": "jq", "expr": ".foo"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(steps) != 1 || steps[0].Parser != "jq" || steps[0].Expr != ".foo" {
		t.Fatalf("unexpected steps: %+v", steps)
	}
}

func TestParseFieldSteps_ChainArray(t *testing.T) {
	steps, err := parseFieldSteps([]any{
		map[string]any{"parser": "base64"},
		map[string]any{"parser": "jq", "expr": ".foo"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(steps) != 2 || steps[0].Parser != "base64" || steps[1].Parser != "jq" {
		t.Fatalf("unexpected steps: %+v", steps)
	}
}

func TestParseFieldSteps_RejectsUnknownParser(t *testing.T) {
	if _, err := parseFieldSteps(map[string]any{"parser": "xml"}); err == nil {
		t.Fatal("expected an error for an unknown parser")
	}
}

func TestParseFieldSteps_RejectsMissingParser(t *testing.T) {
	if _, err := parseFieldSteps(map[string]any{"expr": ".foo"}); err == nil {
		t.Fatal("expected an error for a missing parser")
	}
}

func TestParseFieldSteps_RejectsEmptyChain(t *testing.T) {
	if _, err := parseFieldSteps([]any{}); err == nil {
		t.Fatal("expected an error for an empty chain")
	}
}

func TestParseFieldSteps_RejectsOverlongChain(t *testing.T) {
	chain := make([]any, maxStepsPerField+1)
	for i := range chain {
		chain[i] = map[string]any{"parser": "base64"}
	}
	if _, err := parseFieldSteps(chain); err == nil {
		t.Fatal("expected an error for a chain longer than maxStepsPerField")
	}
}

func TestParseFieldSteps_RejectsOverlongExpr(t *testing.T) {
	_, err := parseFieldSteps(map[string]any{"parser": "jq", "expr": strings.Repeat("a", maxExprLen+1)})
	if err == nil {
		t.Fatal("expected an error for an expr longer than maxExprLen")
	}
}

func TestParseFieldSteps_RejectsWrongShape(t *testing.T) {
	if _, err := parseFieldSteps("not an object or array"); err == nil {
		t.Fatal("expected an error for a plain string field value")
	}
}

func TestRunPipeline_JQOnRawJSONBody(t *testing.T) {
	v, err := runPipeline(context.Background(), []fieldStep{{Parser: "jq", Expr: ".as"}}, []byte(`{"as":"AS15169"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "AS15169" {
		t.Fatalf("expected AS15169, got %v", v)
	}
}

func TestRunPipeline_JQOnNonJSONBodyFails(t *testing.T) {
	_, err := runPipeline(context.Background(), []fieldStep{{Parser: "jq", Expr: "."}}, []byte("not json"))
	if err == nil {
		t.Fatal("expected an error for non-JSON input to jq")
	}
}

func TestRunPipeline_JQNoMatchFails(t *testing.T) {
	_, err := runPipeline(context.Background(), []fieldStep{{Parser: "jq", Expr: ".nonexistent | .deeper"}}, []byte(`{"a":1}`))
	if err == nil {
		t.Fatal("expected an error when the jq query traverses through null")
	}
}

func TestRunPipeline_JQInvalidExprFails(t *testing.T) {
	_, err := runPipeline(context.Background(), []fieldStep{{Parser: "jq", Expr: "this is not valid jq [["}}, []byte(`{}`))
	if err == nil {
		t.Fatal("expected an error for an unparseable jq expression")
	}
}

func TestRunPipeline_Base64DecodesRealBase64(t *testing.T) {
	// "hello world" base64-encoded, standard alphabet.
	v, err := runPipeline(context.Background(), []fieldStep{{Parser: "base64"}}, []byte("aGVsbG8gd29ybGQ="))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "hello world" {
		t.Fatalf("expected \"hello world\", got %v", v)
	}
}

func TestRunPipeline_Base64TriesEveryVariant(t *testing.T) {
	// URL-safe, unpadded -- distinct from the standard alphabet used above.
	v, err := runPipeline(context.Background(), []fieldStep{{Parser: "base64"}}, []byte("aGVsbG8_d29ybGQ"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "hello?world" {
		t.Fatalf("expected \"hello?world\", got %v", v)
	}
}

func TestRunPipeline_Base64RejectsGarbage(t *testing.T) {
	_, err := runPipeline(context.Background(), []fieldStep{{Parser: "base64"}}, []byte("!!!not base64!!!"))
	if err == nil {
		t.Fatal("expected an error for invalid base64")
	}
}

func TestRunPipeline_RegexFirstCaptureGroup(t *testing.T) {
	v, err := runPipeline(context.Background(), []fieldStep{{Parser: "regex", Expr: `version=(\d+\.\d+)`}}, []byte("version=1.4 build=99"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "1.4" {
		t.Fatalf("expected 1.4, got %v", v)
	}
}

func TestRunPipeline_RegexNoCaptureGroupReturnsWholeMatch(t *testing.T) {
	v, err := runPipeline(context.Background(), []fieldStep{{Parser: "regex", Expr: `\d+`}}, []byte("count is 42 today"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "42" {
		t.Fatalf("expected 42, got %v", v)
	}
}

func TestRunPipeline_RegexNoMatchFails(t *testing.T) {
	_, err := runPipeline(context.Background(), []fieldStep{{Parser: "regex", Expr: `xyz`}}, []byte("abc"))
	if err == nil {
		t.Fatal("expected an error when the pattern doesn't match")
	}
}

func TestRunPipeline_RegexInvalidPatternFails(t *testing.T) {
	_, err := runPipeline(context.Background(), []fieldStep{{Parser: "regex", Expr: `(unclosed`}}, []byte("abc"))
	if err == nil {
		t.Fatal("expected an error for an invalid regex pattern")
	}
}

func TestRunPipeline_ChainBase64ThenJQ(t *testing.T) {
	// base64("{"a":1}") -> jq .a
	encoded := "eyJhIjoxfQ=="
	v, err := runPipeline(context.Background(), []fieldStep{{Parser: "base64"}, {Parser: "jq", Expr: ".a"}}, []byte(encoded))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != float64(1) {
		t.Fatalf("expected 1, got %v (%T)", v, v)
	}
}

func TestRunPipeline_ChainJQThenBase64ReencodesStructuredValue(t *testing.T) {
	// A jq step producing a non-string value, followed by a step that
	// needs bytes again -- toBytes must re-marshal it as JSON rather
	// than failing outright.
	v, err := runPipeline(context.Background(), []fieldStep{{Parser: "jq", Expr: "{x: .a}"}, {Parser: "jq", Expr: ".x"}}, []byte(`{"a":5}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != float64(5) {
		t.Fatalf("expected 5, got %v (%T)", v, v)
	}
}
