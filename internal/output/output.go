// Package output formats a probe.Envelope for the CLI's --format flag.
package output

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/mehrnet/radar-node/internal/probe"
)

type Format string

const (
	FormatJSON  Format = "json"
	FormatCSV   Format = "csv"
	FormatTable Format = "table"
)

var columns = []string{"ok", "type", "target", "mode", "seq", "latency_ms", "error", "extra"}

func Write(format Format, w io.Writer, env probe.Envelope) error {
	switch format {
	case FormatJSON:
		return writeJSON(w, env)
	case FormatCSV:
		return writeRows(csv.NewWriter(w), env)
	case FormatTable:
		return writeTable(w, env)
	default:
		return fmt.Errorf("unknown output format %q", format)
	}
}

func writeJSON(w io.Writer, env probe.Envelope) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(env)
}

type rowWriter interface {
	Write(record []string) error
	Flush()
	Error() error
}

func writeRows(w rowWriter, env probe.Envelope) error {
	if err := w.Write(columns); err != nil {
		return err
	}
	for _, r := range env.Results {
		if err := w.Write(row(r)); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func writeTable(w io.Writer, env probe.Envelope) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, tabJoin(columns)); err != nil {
		return err
	}
	for _, r := range env.Results {
		if _, err := fmt.Fprintln(tw, tabJoin(row(r))); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func tabJoin(fields []string) string {
	out := fields[0]
	for _, f := range fields[1:] {
		out += "\t" + f
	}
	return out
}

func row(r probe.Result) []string {
	latency := ""
	if r.LatencyMs != nil {
		latency = strconv.FormatFloat(*r.LatencyMs, 'f', 2, 64)
	}
	extra := ""
	if len(r.Extra) > 0 {
		if b, err := json.Marshal(r.Extra); err == nil {
			extra = string(b)
		}
	}
	return []string{
		strconv.FormatBool(r.Ok),
		r.Type,
		r.Target,
		string(r.Mode),
		strconv.Itoa(r.Seq),
		latency,
		r.Error,
		extra,
	}
}
