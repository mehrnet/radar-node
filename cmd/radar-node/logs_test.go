package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func parse(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("fixture is not valid json: %v", err)
	}
	return m
}

func TestFormat(t *testing.T) {
	line := `{"ts":"2026-09-07T07:22:48.806Z","level":"error","src":"agent","msg":"heartbeat failed","err":"connection refused","status":401}`
	got := format(parse(t, line))

	// The time only -- the date is the same for every line on screen
	// and eats the width the message needs.
	if !strings.HasPrefix(got, "07:22:48 ERROR agent") {
		t.Fatalf("expected a time/level/src prefix, got %q", got)
	}
	if !strings.Contains(got, "heartbeat failed") {
		t.Fatalf("message missing: %q", got)
	}
	// Fields sorted, so the same entry always renders identically
	// instead of in Go's map iteration order.
	if !strings.Contains(got, "err=connection refused  status=401") {
		t.Fatalf("expected sorted trailing fields, got %q", got)
	}
}

func TestFormatToleratesMissingFields(t *testing.T) {
	// A line from some other producer, or a future version with a
	// different shape, must not panic the reader.
	got := format(parse(t, `{"msg":"only a message"}`))
	if !strings.Contains(got, "only a message") {
		t.Fatalf("got %q", got)
	}
}

func TestLevelRank(t *testing.T) {
	if levelRank("debug") >= levelRank("info") || levelRank("info") >= levelRank("warn") || levelRank("warn") >= levelRank("error") {
		t.Fatal("levels must order debug < info < warn < error for --level to mean \"or worse\"")
	}
	// An unset or unrecognized level disables filtering rather than
	// hiding everything.
	if levelRank("") != 0 || levelRank("nonsense") != 0 {
		t.Fatal("an unknown level must not filter anything out")
	}
}
