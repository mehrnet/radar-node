// Package journal is radar-node's structured logging: one JSON object
// per line, with the same envelope radar-api uses (ts, level, src, msg,
// plus whatever the call site adds).
//
// The point of matching envelopes is that a node's logs and the API's
// logs can be read, filtered and shipped by the same tooling. Before
// this, a node emitted log.Printf prose -- "agent: heartbeat failed: %v"
// -- which is fine to watch scroll past and useless to filter, count or
// correlate with anything on the server side.
//
// Built on log/slog from the standard library rather than a logging
// dependency: the whole requirement is "serialize an object to a line",
// the service manager (systemd, launchd) owns rotation and retention,
// and `radar-node logs` reads it back out.
//
// Output goes to stderr. Under systemd both streams land in the journal
// anyway, and stderr is the one that survives a caller redirecting
// stdout to consume a command's real output.
package journal

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"
)

var logger *slog.Logger

func init() {
	SetLevel("info")
}

// SetLevel accepts the same names radar-api's journal uses. An
// unrecognized name falls back to info rather than failing: a bad value
// in a service file should not stop a node from reporting at all.
func SetLevel(name string) {
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			// slog's defaults are "time"/"msg"/"level" with an
			// RFC3339 time and upper-case levels. Renamed and
			// lower-cased so a line from a node is shaped exactly like
			// a line from radar-api.
			switch a.Key {
			case slog.TimeKey:
				return slog.String("ts", a.Value.Time().UTC().Format(time.RFC3339Nano))
			case slog.LevelKey:
				return slog.String("level", strings.ToLower(a.Value.String()))
			}
			return a
		},
	}))
}

// src names the subsystem ("agent", "scheduler", "module"), so a reader
// can filter to one concern instead of grepping message text.
func log(level slog.Level, src, msg string, args ...any) {
	if !logger.Enabled(context.Background(), level) {
		return
	}
	logger.Log(context.Background(), level, msg, append([]any{"src", src}, args...)...)
}

func Debug(src, msg string, args ...any) { log(slog.LevelDebug, src, msg, args...) }
func Info(src, msg string, args ...any)  { log(slog.LevelInfo, src, msg, args...) }
func Warn(src, msg string, args ...any)  { log(slog.LevelWarn, src, msg, args...) }
func Error(src, msg string, args ...any) { log(slog.LevelError, src, msg, args...) }
