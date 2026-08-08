package subscriptionfetch

import (
	"fmt"
	"net/url"
	"strconv"
)

func parseTrojan(line string) (DiscoveredProxy, error) {
	u, err := url.Parse(line)
	if err != nil {
		return DiscoveredProxy{}, fmt.Errorf("trojan: %w", err)
	}
	password := u.User.String()
	host := u.Hostname()
	if password == "" || host == "" {
		return DiscoveredProxy{}, fmt.Errorf("trojan: missing password or host")
	}
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = 443
	}
	q := u.Query()

	// Trojan is TLS by default -- unlike vless/vmess, there's no
	// "plaintext trojan" in normal use, so security defaults to tls
	// here even though the shared streamSettingsFor helper otherwise
	// only adds TLS when explicitly asked.
	security := defaultStr(q.Get("security"), "tls")

	outbound := map[string]any{
		"protocol": "trojan",
		"settings": map[string]any{
			"servers": []map[string]any{{
				"address":  host,
				"port":     port,
				"password": password,
			}},
		},
		"streamSettings": streamSettingsFor(q.Get("type"), security, q.Get("host"), q.Get("path"), q.Get("sni")),
	}

	return DiscoveredProxy{
		Name:   defaultStr(u.Fragment, host),
		Prober: "xray",
		Target: connectivityTestTarget,
		Params: xrayParams(outbound),
	}, nil
}
