package subscriptionfetch

import (
	"encoding/json"
	"fmt"
)

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

// streamSettingsOpts groups every URI query param streamSettingsFor
// might need. The reality-specific fields (fingerprint/publicKey/
// shortID/spiderX) are only ever populated by vless/trojan's own URI
// query params -- vmess's share-standard JSON has no such fields, so
// vmess always leaves them zero-valued, which is exactly "not present"
// to the reality branch below.
type streamSettingsOpts struct {
	network, security, host, path, sni       string
	fingerprint, publicKey, shortID, spiderX string
	// headerType is tcp's own obfuscation mode (the only value in real
	// use is "http", camouflaging the stream as plaintext HTTP -- an
	// empty/absent value means no obfuscation, xray-core's own default,
	// so the tcp case below only adds anything when it's set). mode/
	// extra are xhttp-specific: mode picks which of xhttp's transfer
	// modes to use ("auto" lets the client and server negotiate one --
	// see the xhttp case below for why that's the fallback rather than
	// leaving it unset), extra is a server-tuned opaque JSON object
	// (buffer sizes, padding, etc.) passed through verbatim, never
	// interpreted here.
	headerType, mode, extra string
}

func streamSettingsFor(o streamSettingsOpts) map[string]any {
	stream := map[string]any{"network": defaultStr(o.network, "tcp")}
	effectiveSNI := firstNonEmpty(o.sni, o.host)
	switch o.security {
	case "tls":
		tlsSettings := map[string]any{}
		if effectiveSNI != "" {
			tlsSettings["serverName"] = effectiveSNI
		}
		stream["security"] = "tls"
		stream["tlsSettings"] = tlsSettings
	case "reality":
		// Unlike plain TLS, Reality's handshake genuinely can't
		// complete without its own auth material (publicKey/shortID at
		// minimum) -- a config missing these isn't "less secure", it's
		// non-functional, xray rejects the connection outright and the
		// probe just sits at "no data yet" forever. All four of these
		// come straight from the same query params the subscription's
		// own URI already carries (fp/pbk/sid/spx).
		realitySettings := map[string]any{}
		if effectiveSNI != "" {
			realitySettings["serverName"] = effectiveSNI
		}
		if o.fingerprint != "" {
			realitySettings["fingerprint"] = o.fingerprint
		}
		if o.publicKey != "" {
			realitySettings["publicKey"] = o.publicKey
		}
		if o.shortID != "" {
			realitySettings["shortId"] = o.shortID
		}
		if o.spiderX != "" {
			realitySettings["spiderX"] = o.spiderX
		}
		stream["security"] = "reality"
		stream["realitySettings"] = realitySettings
	}
	switch o.network {
	case "tcp":
		// Only real xray-core value for this is "http" -- plaintext TCP
		// dressed up as an ordinary HTTP request so it doesn't stand out
		// on the wire. Left alone (no tcpSettings at all) when unset,
		// matching xray-core's own "no camouflage" default.
		if o.headerType == "http" {
			stream["tcpSettings"] = map[string]any{
				"header": map[string]any{
					"type": "http",
					"request": map[string]any{
						"path":    []string{defaultStr(o.path, "/")},
						"headers": map[string]any{"Host": []string{firstNonEmpty(o.host, effectiveSNI)}},
					},
				},
			}
		}
	case "ws":
		stream["wsSettings"] = map[string]any{
			"path":    defaultStr(o.path, "/"),
			"headers": map[string]any{"Host": firstNonEmpty(o.host, effectiveSNI)},
		}
	case "grpc":
		stream["grpcSettings"] = map[string]any{"serviceName": o.path}
	case "xhttp":
		// Previously completely unhandled -- a subscription's xhttp
		// entries all fell through this switch with none of path/host/
		// mode/extra ever making it into the outbound at all, silently
		// producing a structurally incomplete config (confirmed in
		// production: 100% check failure across every xhttp entry, on
		// every node, regardless of xray-core version -- xhttp genuinely
		// can't negotiate without at least a matching path).
		xhttpSettings := map[string]any{
			"path": defaultStr(o.path, "/"),
			"host": firstNonEmpty(o.host, effectiveSNI),
			// "auto" (not xray-core's own unset-defaults-to-"auto"
			// behavior left implicit) -- explicit here since this is the
			// one xhttp field a URI's own mode= param might legitimately
			// omit while still expecting auto-negotiation, not "packet-up"
			// or "stream-up" specifically.
			"mode": defaultStr(o.mode, "auto"),
		}
		// extra is server-tuned opaque JSON (buffer sizes, padding,
		// etc.) -- passed through as-is rather than modeled field by
		// field, since xray-core's own accepted shape here has changed
		// across versions and isn't this parser's concern to track.
		// Silently dropped (not a fatal error) if it fails to parse --
		// a probe missing one server's own tuning hints degrades to
		// xhttp's built-in defaults for those fields rather than never
		// getting created at all.
		if o.extra != "" {
			var extra map[string]any
			if err := json.Unmarshal([]byte(o.extra), &extra); err == nil {
				xhttpSettings["extra"] = extra
			}
		}
		stream["xhttpSettings"] = xhttpSettings
	}
	return stream
}

// xrayIdentity builds the stable "same server" key radar-api's own
// computeContentHash prefers over hashing the raw params blob -- see
// its comment for the production report this fixes: a Reality-secured
// config's serverName/shortId/spiderX/fingerprint (exactly the four
// fields deliberately left out of this function's own parameters)
// rotate every few minutes as normal camouflage, with the underlying
// server completely unchanged. Hashing the full params treated every
// rotation as a brand new proxy -- archiving the "old" probe and
// creating a fresh one, losing all its check history -- on every
// single subscription re-fetch. publicKey is kept (it's the server's
// own Reality keypair, not something that rotates per-connection the
// way the other four do) since it's still a meaningful part of "is
// this actually the same server" for two configs that otherwise
// collide on host/port/credential alone.
func xrayIdentity(protocol, address string, port int, credential, network, security, host, path, publicKey string) string {
	return fmt.Sprintf("%s|%s|%d|%s|%s|%s|%s|%s|%s", protocol, address, port, credential, network, security, host, path, publicKey)
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
