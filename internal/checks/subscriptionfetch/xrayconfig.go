package subscriptionfetch

// socksInboundPort is a fixed placeholder, not a real port anyone
// binds to -- xray-prepare.sh only ever uses a config's own declared
// socks_port as a *label* to find which inbound to rewrite onto
// whatever port the check executor actually allocated for that run
// (see its own comment: "never bound as-is"). Every discovered proxy
// can safely share the same declared value; nothing about it needs to
// vary per proxy.
const socksInboundPort = 10808

// buildXrayConfig wraps a single outbound into the same full-config
// shape install/modules/xray-prepare.sh expects in a probe's own
// params.config: one socks inbound (the label prepare.sh rewrites) and
// exactly the one outbound under test -- no routing block needed since
// there's nothing to route between.
func buildXrayConfig(outbound map[string]any) map[string]any {
	return map[string]any{
		"inbounds": []map[string]any{
			{
				"port":     socksInboundPort,
				"protocol": "socks",
				"settings": map[string]any{"auth": "noauth", "udp": true},
			},
		},
		"outbounds": []map[string]any{outbound},
	}
}

// xrayParams is the params object every xray-routed DiscoveredProxy
// carries -- see xray.yaml's own request schema (config + socks_port,
// both required).
func xrayParams(outbound map[string]any) map[string]any {
	return map[string]any{
		"config":     buildXrayConfig(outbound),
		"socks_port": socksInboundPort,
	}
}

func streamSettingsFor(network, security, host, path, sni string) map[string]any {
	stream := map[string]any{"network": defaultStr(network, "tcp")}
	effectiveSNI := firstNonEmpty(sni, host)
	switch security {
	case "tls":
		tlsSettings := map[string]any{}
		if effectiveSNI != "" {
			tlsSettings["serverName"] = effectiveSNI
		}
		stream["security"] = "tls"
		stream["tlsSettings"] = tlsSettings
	case "reality":
		// Not a real Reality implementation -- just preserves the
		// security tag and SNI so the config is at least self-
		// consistent; full Reality support (short IDs, public keys)
		// is out of scope for this pass.
		stream["security"] = "reality"
	}
	switch network {
	case "ws":
		stream["wsSettings"] = map[string]any{
			"path":    defaultStr(path, "/"),
			"headers": map[string]any{"Host": firstNonEmpty(host, effectiveSNI)},
		}
	case "grpc":
		stream["grpcSettings"] = map[string]any{"serviceName": path}
	}
	return stream
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
