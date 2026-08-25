// Package fetch implements a generic HTTP fetch-and-parse check --
// method/headers/body for reaching any HTTP API at all (a public one,
// or a paid/private one via an Authorization header the account
// supplies), plus an optional per-field "fields" parser pipeline (see
// parse.go: base64/jq/regex, chainable) for turning that response into
// whichever named data points an account actually wants -- their own
// choice of which parts of a third-party API's response matter to
// them, not something this binary has to know in advance.
//
// Deliberately mechanical: no protocol-specific business logic lives
// here -- see this project's own README on why that split matters:
// real per-protocol parsing/conversion work (e.g. proxy-subscription
// content, see radar-api's lib/subscriptionParse.ts) belongs server-
// side (fixed with a git push) rather than in this binary (fixed only
// on however long a fleet takes to pick up a tagged release).
package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mehrnet/radar-node/internal/probe"
)

// opts.Target is user-supplied (an account's own choice of URL, same
// trust level as a subscription URL) -- an unbounded read is a
// resource-exhaustion bug, not just a missing nice-to-have. 10MB
// comfortably covers any real API response; anything beyond that is
// far more likely a misconfigured or hostile URL than a legitimate one.
const maxBodyBytes = 10 * 1024 * 1024

// Same reasoning as maxFields (see parse.go) -- an unbounded custom
// header map is a config-bloat/DoS lever, not a real use case.
const maxHeaders = 20

var allowedMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true}

type Checker struct{}

func New() Checker { return Checker{} }

func (Checker) Type() string { return "fetch" }

func do(ctx context.Context, target string, timeout time.Duration, method string, headers map[string]string, body string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := target
	if !strings.Contains(url, "://") {
		url = "https://" + url
	}

	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// No warm/hard transport distinction, unlike httpcheck -- this
	// check's own schedule is whatever an account picked, not a tight
	// monitoring interval, so there's no meaningful connection reuse
	// to optimize for.
	client := &http.Client{Transport: &http.Transport{TLSHandshakeTimeout: 10 * time.Second}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if len(respBody) > maxBodyBytes {
		respBody = respBody[:maxBodyBytes]
	}
	return respBody, resp.StatusCode, nil
}

// Check performs the configured request and, for each entry in the
// "fields" param, runs that field's own parser pipeline against the
// raw response body -- reporting successfully-extracted fields
// alongside the always-present http_code/bytes.
//
// A field whose pipeline errors (bad expr, no match, wrong content
// type) is simply omitted from Extra -- the same "absent, not sent as
// null" convention the wire protocol already uses everywhere else --
// rather than failing the whole check: the request itself may have
// genuinely succeeded, and one misconfigured field's own mistake
// shouldn't read as "this API is down." http_code/bytes are set last,
// deliberately overwriting a user field of the same name rather than
// the reverse -- the built-in observability fields always win.
func (c Checker) Check(ctx context.Context, opts probe.Options) probe.Result {
	method := strings.ToUpper(opts.Param("method", "GET"))
	if !allowedMethods[method] {
		return probe.Fail(c.Type(), opts.Target, opts.Seq, fmt.Errorf("unsupported method %q -- expected GET, POST, PUT, PATCH, DELETE, or HEAD", method))
	}

	headers, err := parseHeaders(opts.Params["headers"])
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Seq, fmt.Errorf("headers: %w", err))
	}

	body := opts.Param("body", "")

	fieldsRaw, _ := opts.Params["fields"].(map[string]any)
	if len(fieldsRaw) > maxFields {
		return probe.Fail(c.Type(), opts.Target, opts.Seq, fmt.Errorf("fields: at most %d allowed, got %d", maxFields, len(fieldsRaw)))
	}

	start := time.Now()
	respBody, statusCode, err := do(ctx, opts.Target, opts.Timeout, method, headers, body)
	elapsed := time.Since(start)
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Seq, err)
	}

	extra := map[string]any{}
	for name, raw := range fieldsRaw {
		steps, err := parseFieldSteps(raw)
		if err != nil {
			continue
		}
		value, err := runPipeline(ctx, steps, respBody)
		if err != nil {
			continue
		}
		extra[name] = value
	}
	extra["http_code"] = statusCode
	extra["bytes"] = len(respBody)

	return probe.Ok(c.Type(), opts.Target, opts.Seq, elapsed, extra)
}

func parseHeaders(raw any) (map[string]string, error) {
	if raw == nil {
		return nil, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("must be an object")
	}
	if len(m) > maxHeaders {
		return nil, fmt.Errorf("at most %d allowed, got %d", maxHeaders, len(m))
	}
	headers := make(map[string]string, len(m))
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%q must be a string", k)
		}
		headers[k] = s
	}
	return headers, nil
}
