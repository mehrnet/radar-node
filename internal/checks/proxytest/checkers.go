package proxytest

import (
	"context"
	"fmt"
	"os"

	"github.com/mehrnet/radar-node/internal/probe"
)

// XrayChecker and SingBoxChecker are generic, protocol-agnostic
// proxy-through-engine checks. Both expect the same two params:
//
//	config:      the engine's full client config (object) -- however
//	             the caller built it (converted from a share link,
//	             hand-written, exported from another tool)
//	socks_port:  which inbound (by its port/listen_port) in that
//	             config is the local test entry point (number)
//
// Neither reads anything protocol-specific out of config; that's the
// whole point -- a new protocol either engine adds needs no change
// here.
type XrayChecker struct{ Bin string }

// NewXray defaults Bin from XRAY_BIN, falling back to a bare "xray"
// looked up on PATH -- matches the env-var convention the xray-vless
// example module already uses.
func NewXray() XrayChecker {
	bin := os.Getenv("XRAY_BIN")
	if bin == "" {
		bin = "xray"
	}
	return XrayChecker{Bin: bin}
}

func (c XrayChecker) Type() string { return "xray_proxy_test" }

func (c XrayChecker) Check(ctx context.Context, opts probe.Options) probe.Result {
	config, port, err := extractParams(opts)
	if err != nil {
		return probe.Invalid(c.Type(), opts.Target, opts.Mode, opts.Seq, err.Error())
	}
	return Run(ctx, c.Bin, c.Type(), opts, config, port)
}

type SingBoxChecker struct{ Bin string }

func NewSingBox() SingBoxChecker {
	bin := os.Getenv("SINGBOX_BIN")
	if bin == "" {
		bin = "sing-box"
	}
	return SingBoxChecker{Bin: bin}
}

func (c SingBoxChecker) Type() string { return "singbox_proxy_test" }

func (c SingBoxChecker) Check(ctx context.Context, opts probe.Options) probe.Result {
	config, port, err := extractParams(opts)
	if err != nil {
		return probe.Invalid(c.Type(), opts.Target, opts.Mode, opts.Seq, err.Error())
	}
	return Run(ctx, c.Bin, c.Type(), opts, config, port)
}

// extractParams re-checks config/socks_port defensively -- normally
// already guaranteed by the calling module's declared request schema
// (see internal/module/checker.go), but these Checkers are directly
// callable via the action registry regardless of that, so they don't
// rely on it.
func extractParams(opts probe.Options) (map[string]any, float64, error) {
	config, ok := opts.Params["config"].(map[string]any)
	if !ok {
		return nil, 0, fmt.Errorf("missing or invalid required param %q", "config")
	}
	port, ok := opts.Params["socks_port"].(float64)
	if !ok {
		return nil, 0, fmt.Errorf("missing or invalid required param %q", "socks_port")
	}
	return config, port, nil
}
