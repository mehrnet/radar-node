package output_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mehrnet/radar-node/internal/output"
	"github.com/mehrnet/radar-node/internal/probe"
)

func sampleEnvelope() probe.Envelope {
	latency := 12.5
	return probe.Envelope{
		Ok: false,
		Results: []probe.Result{
			{Ok: true, Type: "tcp", Target: "1.2.3.4:443", Mode: probe.ModeWarm, Seq: 1, LatencyMs: &latency},
			{Ok: false, Type: "tcp", Target: "1.2.3.4:444", Mode: probe.ModeWarm, Seq: 1, Error: "connection refused"},
		},
	}
}

func TestWriteJSON_Envelope(t *testing.T) {
	var buf bytes.Buffer
	if err := output.Write(output.FormatJSON, &buf, sampleEnvelope()); err != nil {
		t.Fatal(err)
	}

	var got probe.Envelope
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output was not valid JSON: %v\n%s", err, buf.String())
	}
	if got.Ok != false || len(got.Results) != 2 {
		t.Fatalf("unexpected envelope: %+v", got)
	}
}

func TestWriteCSV_HasHeaderAndRows(t *testing.T) {
	var buf bytes.Buffer
	if err := output.Write(output.FormatCSV, &buf, sampleEnvelope()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 { // header + 2 results
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), buf.String())
	}
	if !strings.HasPrefix(lines[0], "ok,type,target") {
		t.Fatalf("unexpected header: %q", lines[0])
	}
}

func TestWriteTable_HasHeaderAndRows(t *testing.T) {
	var buf bytes.Buffer
	if err := output.Write(output.FormatTable, &buf, sampleEnvelope()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), buf.String())
	}
}

func TestWrite_UnknownFormat(t *testing.T) {
	var buf bytes.Buffer
	if err := output.Write("yaml", &buf, sampleEnvelope()); err == nil {
		t.Fatal("expected an error for an unknown format")
	}
}
