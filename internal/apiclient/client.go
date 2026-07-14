// Package apiclient is the node's HTTP client for radar-api, per
// README.md. It is deliberately the only place in radar-node
// that knows the wire protocol's endpoint shapes.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"

	"github.com/mehrnet/radar-node/internal/wire"
)

type Client struct {
	baseURL    string
	apiKey     string // "node_id:secret", sent as-is as the bearer token
	httpClient *http.Client
}

// New builds a Client. proxyURL, if non-empty, routes every request
// to radar-api through it -- this is the node's *own* control-plane
// traffic, unrelated to any proxy links checks might test. Supported
// schemes: http, https, socks5, socks5h.
func New(baseURL, apiKey, proxyURL string) (*Client, error) {
	transport, err := buildTransport(proxyURL)
	if err != nil {
		return nil, err
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
	}, nil
}

func buildTransport(proxyURL string) (*http.Transport, error) {
	t := &http.Transport{}
	if proxyURL == "" {
		return t, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid --api-proxy %q: %w", proxyURL, err)
	}
	switch u.Scheme {
	case "http", "https":
		t.Proxy = http.ProxyURL(u)
	case "socks5", "socks5h":
		dialer, err := proxy.FromURL(u, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("build socks5 dialer for --api-proxy: %w", err)
		}
		if cd, ok := dialer.(proxy.ContextDialer); ok {
			t.DialContext = cd.DialContext
		} else {
			// Falls back to a context-ignorant dial; the x/net/proxy
			// SOCKS5 implementation has satisfied ContextDialer for
			// a long time, so this branch is effectively dead code
			// kept only so a future non-conforming Dialer degrades
			// gracefully instead of failing to compile/run.
			t.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
		}
	default:
		return nil, fmt.Errorf("unsupported --api-proxy scheme %q (want http, https, socks5, or socks5h)", u.Scheme)
	}
	return t, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return &StatusError{Code: resp.StatusCode, Body: string(respBody)}
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	return json.Unmarshal(respBody, out)
}

// StatusError is returned for any non-2xx radar-api response.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("radar-api returned %d: %s", e.Code, e.Body)
}

func (c *Client) PostResults(ctx context.Context, req wire.ResultsRequest) (*wire.ResultsResponse, error) {
	req.SpecVersion = wire.SpecVersion
	var out wire.ResultsResponse
	if err := c.do(ctx, http.MethodPost, "/v1/nodes/results", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Heartbeat returns a *HeartbeatRejectedError (wrapping the parsed
// wire.HeartbeatRejection) when radar-api responds 409 because one or
// more reported prober_id:file_hash pairs aren't recognized yet --
// the caller is expected to type-assert for this specific case and
// upload the named modules via UploadModules before retrying, rather
// than treating it as a generic failure.
func (c *Client) Heartbeat(ctx context.Context, req wire.HeartbeatRequest) (*wire.HeartbeatResponse, error) {
	req.SpecVersion = wire.SpecVersion
	var out wire.HeartbeatResponse
	err := c.do(ctx, http.MethodPost, "/v1/nodes/heartbeat", req, &out)
	if err == nil {
		return &out, nil
	}
	var statusErr *StatusError
	if errors.As(err, &statusErr) && statusErr.Code == http.StatusConflict {
		var rejection wire.HeartbeatRejection
		if jsonErr := json.Unmarshal([]byte(statusErr.Body), &rejection); jsonErr == nil && rejection.Error == wire.HeartbeatErrorModulesOutOfSync {
			return nil, &HeartbeatRejectedError{Rejection: rejection}
		}
	}
	return nil, err
}

// HeartbeatRejectedError is returned by Heartbeat when radar-api
// needs missing/changed modules pushed before it'll accept this
// node's heartbeat.
type HeartbeatRejectedError struct {
	Rejection wire.HeartbeatRejection
}

func (e *HeartbeatRejectedError) Error() string {
	return fmt.Sprintf("heartbeat rejected: %s (missing: %v)", e.Rejection.Error, e.Rejection.MissingProberIDs)
}

// UploadModules pushes the full definition of one or more modules
// radar-api doesn't recognize the current hash for yet.
func (c *Client) UploadModules(ctx context.Context, req wire.ModulesUploadRequest) (*wire.ModulesUploadResponse, error) {
	req.SpecVersion = wire.SpecVersion
	var out wire.ModulesUploadResponse
	if err := c.do(ctx, http.MethodPost, "/v1/nodes/modules", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
