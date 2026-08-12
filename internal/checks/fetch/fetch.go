// Package fetch implements a raw HTTP GET that retains the response
// body, unlike httpcheck's own request (which drains and discards it
// to keep pooled connections reusable -- a normal check only cares
// about latency/status, never the body itself). subscriptionfetch
// imports this package directly to get the raw bytes it then parses;
// fetch's own Checker exists so the same logic is independently
// reachable as an ordinary probe too, not just as a building block.
package fetch

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mehrnet/radar-node/internal/probe"
)

// maxBodyBytes caps how much of a response this reads -- opts.Target
// is a user-supplied URL (a subscription URL is exactly this: content
// from a third party the account owner chose to point at), so an
// unbounded read is a resource-exhaustion bug, not just a missing nice-
// to-have. 10MB comfortably covers even a large real-world subscription
// list; anything beyond that is far more likely a misconfigured or
// hostile URL than a legitimate one.
const maxBodyBytes = 10 * 1024 * 1024

type Checker struct{}

func New() Checker { return Checker{} }

func (Checker) Type() string { return "fetch" }

// Do performs the GET and returns the response body (capped at
// maxBodyBytes), the HTTP status code, and any error. Shared by
// Checker.Check below and subscriptionfetch, which calls this
// directly rather than going through the action registry -- there's
// no action-to-action call mechanism in this codebase, and none is
// needed for a plain Go function call.
func Do(ctx context.Context, opts probe.Options) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	url := opts.Target
	if !strings.Contains(url, "://") {
		url = "https://" + url
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}

	// No warm/hard transport distinction, unlike httpcheck -- a
	// subscription fetch is infrequent (its own schedule, not a tight
	// monitoring interval) and one-shot per run, so there's no
	// meaningful connection reuse to optimize for.
	client := &http.Client{Transport: &http.Transport{TLSHandshakeTimeout: 10 * time.Second}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if len(body) > maxBodyBytes {
		body = body[:maxBodyBytes]
	}
	return body, resp.StatusCode, nil
}

// Check performs a GET against opts.Target and reports the byte count
// as Extra -- the raw body itself is only useful to a caller that
// actually parses it (subscriptionfetch), not to a plain check result.
func (c Checker) Check(ctx context.Context, opts probe.Options) probe.Result {
	start := time.Now()
	body, statusCode, err := Do(ctx, opts)
	elapsed := time.Since(start)
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Seq, err)
	}

	extra := map[string]any{
		"http_code": statusCode,
		"bytes":     len(body),
	}
	return probe.Ok(c.Type(), opts.Target, opts.Seq, elapsed, extra)
}
