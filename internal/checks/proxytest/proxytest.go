// Package proxytest implements a generic xray/sing-box proxy check:
// a probe supplies a full engine config (however it built it -- e.g.
// converted client-side from a vless://, vmess://, trojan://, ...
// share link, the way 3x-ui does it) plus which port in that config
// is the local test entry point. There is deliberately no per-
// protocol logic here at all -- whatever protocol the engine's
// config describes, this package never needs to know or care, so a
// new protocol either engine adds is supported automatically.
//
// The declared port is never actually bound as-is. A node's own
// operating environment isn't fully known in advance -- another
// service could already hold that port, or two probes could
// legitimately want to test through the "same" port concurrently --
// so the node always silently reallocates the test inbound to a real
// free local port before launching the engine, and reports results
// against whatever port it actually used. The account/probe never sees
// or needs to know this happened; the declared port only ever serves
// to identify *which* inbound in the config is the one to remap.
package proxytest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"

	"golang.org/x/net/proxy"

	"github.com/mehrnet/radar-node/internal/portalloc"
	"github.com/mehrnet/radar-node/internal/probe"
)

// portKeys are the field names either engine uses for an inbound's
// listen port -- xray uses "port"; sing-box's canonical schema uses
// "listen_port" (though it also accepts "port" in some inbound
// types), so both are checked rather than assuming one.
var portKeys = []string{"port", "listen_port"}

// Run launches engineBin with a copy of config whose declared
// inbound (the one whose port/listen_port equals declaredPort) has
// been silently remapped to a freshly allocated local port, then
// performs a real HTTP GET against target through that port via
// SOCKS5, and reports the outcome as a normal probe.Result.
//
// checkType/target/mode/seq are passed through only to shape the
// returned Result consistently with every other Checker; engineBin
// is the caller's own (xray vs sing-box) binary path.
func Run(ctx context.Context, engineBin, checkType string, opts probe.Options, config map[string]any, declaredPort float64) probe.Result {
	allocatedPort, err := portalloc.Alloc()
	if err != nil {
		return probe.Fail(checkType, opts.Target, opts.Mode, opts.Seq, fmt.Errorf("allocate port: %w", err))
	}

	remapped, remappedAny := remapInboundPort(config, declaredPort, allocatedPort)
	if !remappedAny {
		return probe.Invalid(checkType, opts.Target, opts.Mode, opts.Seq, fmt.Sprintf("no inbound in config has port/listen_port %v", declaredPort))
	}

	configPath, cleanupConfig, err := writeConfig(remapped)
	if err != nil {
		return probe.Fail(checkType, opts.Target, opts.Mode, opts.Seq, fmt.Errorf("write engine config: %w", err))
	}
	defer cleanupConfig()

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, engineBin, "run", "-c", configPath)
	if err := cmd.Start(); err != nil {
		return probe.Fail(checkType, opts.Target, opts.Mode, opts.Seq, fmt.Errorf("start %s: %w", engineBin, err))
	}
	go func() { _ = cmd.Wait() }() // reap; ctx cancellation ends the process

	readinessCtx, readyCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readyCancel()
	if err := portalloc.WaitForPort(readinessCtx, allocatedPort); err != nil {
		return probe.Fail(checkType, opts.Target, opts.Mode, opts.Seq, err)
	}

	return probeThroughSOCKS(ctx, checkType, opts, allocatedPort)
}

// remapInboundPort returns a deep-enough copy of config (only the
// inbounds slice and the one matching inbound map are actually
// copied; everything else is shared, since only those are mutated)
// with the first inbound whose port/listen_port equals declaredPort
// rewritten to newPort.
func remapInboundPort(config map[string]any, declaredPort float64, newPort int) (map[string]any, bool) {
	inboundsRaw, ok := config["inbounds"].([]any)
	if !ok {
		return config, false
	}

	newInbounds := make([]any, len(inboundsRaw))
	copy(newInbounds, inboundsRaw)
	found := false
	for i, ib := range inboundsRaw {
		inbound, ok := ib.(map[string]any)
		if !ok || found {
			continue
		}
		for _, key := range portKeys {
			if p, ok := inbound[key].(float64); ok && p == declaredPort {
				newInbound := make(map[string]any, len(inbound))
				for k, v := range inbound {
					newInbound[k] = v
				}
				newInbound[key] = float64(newPort)
				newInbounds[i] = newInbound
				found = true
				break
			}
		}
	}
	if !found {
		return config, false
	}

	out := make(map[string]any, len(config))
	for k, v := range config {
		out[k] = v
	}
	out["inbounds"] = newInbounds
	return out, true
}

func writeConfig(config map[string]any) (string, func(), error) {
	data, err := json.Marshal(config)
	if err != nil {
		return "", nil, err
	}
	f, err := os.CreateTemp("", "radar-node-engine-config-*.json")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return f.Name(), cleanup, nil
}

func probeThroughSOCKS(ctx context.Context, checkType string, opts probe.Options, socksPort int) probe.Result {
	dialer, err := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", socksPort), nil, proxy.Direct)
	if err != nil {
		return probe.Fail(checkType, opts.Target, opts.Mode, opts.Seq, fmt.Errorf("build socks5 dialer: %w", err))
	}
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		// Unreachable in practice: x/net/proxy's own SOCKS5
		// implementation has satisfied ContextDialer for a long
		// time. Kept as a safe fallback rather than a panic.
		contextDialer = contextDialerFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		})
	}

	transport := &http.Transport{DialContext: contextDialer.DialContext}
	client := &http.Client{Transport: transport}

	target := opts.Target
	if !bytes.Contains([]byte(target), []byte("://")) {
		target = "http://" + target
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return probe.Fail(checkType, opts.Target, opts.Mode, opts.Seq, err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return probe.Fail(checkType, opts.Target, opts.Mode, opts.Seq, err)
	}
	defer resp.Body.Close()

	return probe.Ok(checkType, opts.Target, opts.Mode, opts.Seq, elapsed, map[string]any{
		"http_code": resp.StatusCode,
	})
}

type contextDialerFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func (f contextDialerFunc) Dial(network, addr string) (net.Conn, error) {
	return f(context.Background(), network, addr)
}

func (f contextDialerFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return f(ctx, network, addr)
}
