package subscriptionfetch

import (
	"encoding/base64"
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

// Parse detects the format of a fetched subscription body and returns
// the unified proxy list. A line that fails to parse (malformed URI,
// unrecognized scheme, ...) is skipped rather than failing the whole
// fetch -- one bad entry in a 200-line subscription shouldn't blank
// out the other 199 real ones.
func Parse(raw []byte) ([]DiscoveredProxy, error) {
	content := strings.TrimSpace(string(raw))
	if content == "" {
		return nil, nil
	}

	// A full xray/v2ray-style JSON config declares its outbounds
	// directly, not one-proxy-per-line -- only genuine JSON (starts
	// with '{') is ever tried this way, so nothing else parses as this
	// by accident.
	if strings.HasPrefix(content, "{") {
		return parseXrayJSON(content)
	}

	// Try base64 next -- the quintessential v2ray subscription shape:
	// the whole body is base64, decoding to a plain newline-separated
	// URI list handled the same way a raw list already is. A genuine
	// URI-list line always contains "://", which the base64 alphabet
	// never does -- cheap, reliable discriminator that avoids
	// mistaking an already-raw list for base64.
	if !strings.Contains(content, "://") {
		if decoded, ok := tryBase64(content); ok {
			content = decoded
		}
	}

	return parseLines(content), nil
}

func tryBase64(s string) (string, bool) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if decoded, err := enc.DecodeString(s); err == nil {
			return string(decoded), true
		}
	}
	return "", false
}

func parseLines(content string) []DiscoveredProxy {
	var proxies []DiscoveredProxy
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if proxy, err := parseLine(line); err == nil {
			proxies = append(proxies, proxy)
		}
	}
	return proxies
}

func parseLine(line string) (DiscoveredProxy, error) {
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
		return parsePlainProxy(line)
	}
}
