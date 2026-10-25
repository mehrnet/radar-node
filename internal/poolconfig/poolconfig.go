// Package poolconfig builds one engine config from a pooled module's
// whole job batch -- the Go replacement for the python3 block in
// radar's xray-pool-build-config.sh. It exists because python3 is not
// guaranteed on a monitoring node: a bare VPS without it failed every
// xray job in the batch with "python3: not found", taking that whole
// node's xray coverage to zero. One small Go subcommand (this binary
// is already on every node) also avoids python3's ~20MB interpreter
// footprint per rebuild.
//
// The merge is deliberately a straight port of the shell script:
// every job's own inbound is remapped onto the port the executor
// allocated, inbounds/outbounds get per-job tags, and a routing rule
// pins each inbound to its own primary outbound so jobs sharing one
// engine process can never cross-talk.
//
// Two transport-specific exceptions are preserved verbatim from the
// script:
//   - Mux.Cool is enabled on a job's primary outbound unless its
//     transport is XHTTP (XHTTP has its own multiplexing and must not
//     be combined with mux) or its security is REALITY (community-
//     reported connection failures with that combination).
//   - A REALITY outbound's TLS ClientHello is fragmented through a
//     per-job freedom outbound + sockopt.dialerProxy, so a DPI
//     middlebox cannot fingerprint the first packet.
//
// A malformed job (no inbounds/outbounds, or an inbound missing the
// declared socks_port) is simply left out of the merged config: its
// own run step then fails alone instead of aborting every other job
// in the instance.
package poolconfig

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mehrnet/radar-node/internal/netbind"
)

// Run is the `radar-node poolconfig` entry point. Deliberately exits
// non-2 on a recognized invocation with bad arguments: module scripts
// use "did a bare invocation exit 2?" to tell an unknown subcommand
// (old binary, fall back to python3) from a real one.
func Run(args []string, _ io.Writer) error {
	fs := flag.NewFlagSet("poolconfig", flag.ContinueOnError)
	jobsPath := fs.String("jobs", "", "path to the batch's jobs JSON")
	outPath := fs.String("out", "", "path to write the merged engine config")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *jobsPath == "" || *outPath == "" {
		return errors.New("poolconfig: --jobs and --out are required")
	}

	raw, err := os.ReadFile(*jobsPath)
	if err != nil {
		return fmt.Errorf("poolconfig: read jobs: %w", err)
	}
	var jobs []job
	if err := json.Unmarshal(raw, &jobs); err != nil {
		return fmt.Errorf("poolconfig: parse jobs: %w", err)
	}

	strategy, err := domainStrategyFromEnv()
	if err != nil {
		return fmt.Errorf("poolconfig: %w", err)
	}
	binding, err := netbind.FromEnv()
	if err != nil {
		return fmt.Errorf("poolconfig: %w", err)
	}
	encoded, err := json.Marshal(buildConfig(jobs, strategy, binding))
	if err != nil {
		return fmt.Errorf("poolconfig: encode config: %w", err)
	}
	if err := os.WriteFile(*outPath, encoded, 0o600); err != nil {
		return fmt.Errorf("poolconfig: write config: %w", err)
	}
	return nil
}

type job struct {
	Index     int `json:"index"`
	AllocPort int `json:"alloc_port"`
	Params    struct {
		Config struct {
			Inbounds  []map[string]any `json:"inbounds"`
			Outbounds []map[string]any `json:"outbounds"`
		} `json:"config"`
		SocksPort any `json:"socks_port"`
	} `json:"params"`
}

type mergedConfig struct {
	Inbounds  []map[string]any `json:"inbounds"`
	Outbounds []map[string]any `json:"outbounds"`
	Routing   routing          `json:"routing"`
}

type routing struct {
	Rules []map[string]any `json:"rules"`
}

// domainStrategyFromEnv reads the agent's node-level address-family
// choice (RADAR_DOMAIN_STRATEGY, set from --domain-strategy). Empty
// means "leave every outbound's own setting alone".
func domainStrategyFromEnv() (string, error) {
	value := strings.TrimSpace(os.Getenv("RADAR_DOMAIN_STRATEGY"))
	switch strings.ToLower(value) {
	case "":
		return "", nil
	case "as_is", "asis":
		return "AsIs", nil
	case "use_ipv4", "useipv4":
		return "UseIPv4", nil
	case "use_ip", "useip":
		return "UseIP", nil
	case "use_ipv6", "useipv6":
		return "UseIPv6", nil
	default:
		return "", fmt.Errorf("unknown RADAR_DOMAIN_STRATEGY %q", value)
	}
}

// applyDomainStrategy forces an outbound's server/target resolution
// onto the node-level address family. On a cellular vantage with a
// broken or partial IPv6 path, an AAAA answer must not make a healthy
// proxy look dead. Existing sockopt keys (dialerProxy, etc.) are
// preserved.
func applyDomainStrategy(outbound map[string]any, strategy string) {
	if strategy == "" {
		return
	}
	stream, _ := outbound["streamSettings"].(map[string]any)
	if stream == nil {
		stream = map[string]any{}
	}
	sockopt, _ := stream["sockopt"].(map[string]any)
	if sockopt == nil {
		sockopt = map[string]any{}
	}
	sockopt["domainStrategy"] = strategy
	stream["sockopt"] = sockopt
	outbound["streamSettings"] = stream
}

// applyBinding makes every generated engine outbound use the node's
// selected path. sendThrough is the source-address option;
// sockopt.interface is Xray's own device binding, implemented by the
// shipped Xray on Linux, macOS, and Windows. A link-local source has
// no usable zone in sendThrough, so it relies on the interface option.
func applyBinding(outbound map[string]any, binding netbind.Binding) {
	if binding.IsZero() {
		return
	}
	if binding.SourceIP.IsValid() && !binding.SourceIP.IsLinkLocalUnicast() {
		outbound["sendThrough"] = binding.SourceIP.WithZone("").String()
	}
	if binding.Interface != "" {
		stream, _ := outbound["streamSettings"].(map[string]any)
		if stream == nil {
			stream = map[string]any{}
		}
		sockopt, _ := stream["sockopt"].(map[string]any)
		if sockopt == nil {
			sockopt = map[string]any{}
		}
		sockopt["interface"] = binding.Interface
		stream["sockopt"] = sockopt
		outbound["streamSettings"] = stream
	}
}

func buildConfig(jobs []job, domainStrategy string, binding netbind.Binding) mergedConfig {
	merged := mergedConfig{
		Inbounds:  []map[string]any{},
		Outbounds: []map[string]any{},
		Routing:   routing{Rules: []map[string]any{}},
	}
	for _, j := range jobs {
		declaredPort, ok := asInt(j.Params.SocksPort)
		if !ok || len(j.Params.Config.Inbounds) == 0 || len(j.Params.Config.Outbounds) == 0 {
			continue
		}

		inbound, ok := remapInbound(j.Params.Config.Inbounds, declaredPort, j.AllocPort)
		if !ok {
			continue
		}
		inTag := fmt.Sprintf("job-%d-in", j.Index)
		inbound["tag"] = inTag
		merged.Inbounds = append(merged.Inbounds, inbound)

		primaryTag := ""
		for i, outbound := range j.Params.Config.Outbounds {
			tag := fmt.Sprintf("job-%d-out-%d", j.Index, i)
			outbound["tag"] = tag
			applyDomainStrategy(outbound, domainStrategy)
			applyBinding(outbound, binding)
			if primaryTag == "" {
				primaryTag = tag
				stream, _ := outbound["streamSettings"].(map[string]any)
				network, _ := stream["network"].(string)
				security, _ := stream["security"].(string)
				isReality := security == "reality"
				if network != "xhttp" && !isReality {
					outbound["mux"] = map[string]any{"enabled": true, "concurrency": 8}
				}
				if isReality {
					fragTag := fmt.Sprintf("job-%d-frag", j.Index)
					sockopt, _ := stream["sockopt"].(map[string]any)
					if sockopt == nil {
						sockopt = map[string]any{}
					}
					sockopt["dialerProxy"] = fragTag
					stream["sockopt"] = sockopt
					outbound["streamSettings"] = stream
					frag := map[string]any{
						"tag":      fragTag,
						"protocol": "freedom",
						"settings": map[string]any{
							"fragment": map[string]any{
								"packets":  "tlshello",
								"length":   "10-100",
								"interval": "10-20",
							},
						},
					}
					applyDomainStrategy(frag, domainStrategy)
					applyBinding(frag, binding)
					merged.Outbounds = append(merged.Outbounds, frag)
				}
			}
			merged.Outbounds = append(merged.Outbounds, outbound)
		}
		merged.Routing.Rules = append(merged.Routing.Rules, map[string]any{
			"type":        "field",
			"inboundTag":  []string{inTag},
			"outboundTag": primaryTag,
		})
	}
	return merged
}

// remapInbound finds the inbound whose port/listen_port is the job's
// declared socks_port and copies it onto the executor's allocated
// port. The declared port is only a label: it may collide with another
// concurrent job's, so it is never bound as-is.
func remapInbound(inbounds []map[string]any, declaredPort, allocPort int) (map[string]any, bool) {
	for _, raw := range inbounds {
		for _, key := range []string{"port", "listen_port"} {
			if port, ok := asInt(raw[key]); ok && port == declaredPort {
				inbound := make(map[string]any, len(raw))
				for k, v := range raw {
					inbound[k] = v
				}
				inbound[key] = allocPort
				return inbound, true
			}
		}
	}
	return nil, false
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case int:
		return n, true
	case int64:
		return int(n), true
	default:
		return 0, false
	}
}
