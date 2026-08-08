package subscriptionfetch

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// connectivityTestTarget is what a discovered xray-routed proxy is
// actually tested against once it becomes an ordinary probe -- curled
// *through* the proxy's own local SOCKS inbound, not the proxy's own
// address (see install/modules/xray-run.sh: {{target}} is the URL
// fetched via --socks5-hostname, the proxy's address/port only ever
// appear inside the outbound config). A generate_204-style endpoint:
// small, fast, and its success is unambiguous.
const connectivityTestTarget = "https://www.gstatic.com/generate_204"

// SubscriptionType names exactly what a fetched body's content is,
// picked by the user rather than sniffed -- radar-api's own probe-
// creation UI exposes these same four values in a dropdown (default
// "base64-xray"). Auto-detection used to guess this instead, which
// meant one ambiguous or malformed response (an xray JSON config that
// happens to start with whitespace before "{", a URI list someone
// base64'd for no reason) could silently parse as the wrong thing.
// Making the user say which one this source actually is turns that
// into a config error the moment they set it up, not a mystery when
// zero proxies show up later.
type SubscriptionType string

const (
	// Base64Xray is the classic v2ray/xray subscription shape: the
	// whole response is base64, decoding to a newline-separated list
	// of vmess://, vless://, trojan://, or ss:// URIs.
	Base64Xray SubscriptionType = "base64-xray"
	// XrayURIList is the same URI-per-line shape as Base64Xray, just
	// not base64-encoded -- some sources publish the raw list directly.
	XrayURIList SubscriptionType = "xray-uri-list"
	// XrayJSON is a full xray/v2ray client config (an "outbounds" list)
	// supplied directly as the subscription content, one outbound per
	// discovered proxy.
	XrayJSON SubscriptionType = "xray-json"
	// ProxyList is a plain, non-v2ray proxy list: bare "host:port" or
	// "scheme://[user:pass@]host:port" lines, the shape most proxy-
	// list websites publish.
	ProxyList SubscriptionType = "proxy-list"
)

// Parse turns a fetched subscription body into the unified proxy list,
// interpreting it strictly as subType says -- no sniffing. A line that
// fails to parse within that shape (malformed URI, unrecognized
// scheme, ...) is skipped rather than failing the whole fetch; one bad
// entry in a 200-line subscription shouldn't blank out the other 199
// real ones. An unrecognized subType is the one thing that does fail
// the whole fetch (a real config mistake, not a bad subscription entry).
func Parse(raw []byte, subType SubscriptionType) ([]DiscoveredProxy, error) {
	content := strings.TrimSpace(string(raw))
	if content == "" {
		return nil, nil
	}

	switch subType {
	case XrayJSON:
		return parseXrayJSON(content)
	case Base64Xray:
		decoded, ok := tryBase64(content)
		if !ok {
			return nil, fmt.Errorf("subscription_fetch: type %q but content isn't valid base64", subType)
		}
		return parseXrayURILines(decoded), nil
	case XrayURIList:
		return parseXrayURILines(content), nil
	case ProxyList:
		return parseProxyListLines(content), nil
	default:
		return nil, fmt.Errorf("subscription_fetch: unknown subscription type %q", subType)
	}
}

func tryBase64(s string) (string, bool) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if decoded, err := enc.DecodeString(s); err == nil {
			return string(decoded), true
		}
	}
	return "", false
}

// parseXrayURILines handles the vmess/vless/trojan/ss URI-per-line
// shape (Base64Xray, once decoded, and XrayURIList directly).
func parseXrayURILines(content string) []DiscoveredProxy {
	var proxies []DiscoveredProxy
	for _, line := range eachLine(content) {
		proxy, err := parseXrayURILine(line)
		if err != nil {
			continue
		}
		proxies = append(proxies, proxy)
	}
	return proxies
}

func parseXrayURILine(line string) (DiscoveredProxy, error) {
	switch {
	case strings.HasPrefix(line, "vmess://"):
		return parseVmess(line)
	case strings.HasPrefix(line, "vless://"):
		return parseVless(line)
	case strings.HasPrefix(line, "trojan://"):
		return parseTrojan(line)
	case strings.HasPrefix(line, "ss://"):
		return parseShadowsocks(line)
	default:
		return DiscoveredProxy{}, fmt.Errorf("subscriptionfetch: unrecognized xray URI scheme")
	}
}

// parseProxyListLines handles the plain (non-v2ray) proxy-list shape
// (ProxyList) -- every line is a bare host:port or a scheme://
// [user:pass@]host:port entry, never a vmess/vless/trojan/ss URI.
func parseProxyListLines(content string) []DiscoveredProxy {
	var proxies []DiscoveredProxy
	for _, line := range eachLine(content) {
		proxy, err := parsePlainProxy(line)
		if err != nil {
			continue
		}
		proxies = append(proxies, proxy)
	}
	return proxies
}

func eachLine(content string) []string {
	var lines []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}
