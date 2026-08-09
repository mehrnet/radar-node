package subscriptionfetch

import (
	"fmt"
	"net/url"
	"strconv"
)

func parseVless(line string) (DiscoveredProxy, error) {
	u, err := url.Parse(line)
	if err != nil {
		return DiscoveredProxy{}, fmt.Errorf("vless: %w", err)
	}
	uuid := u.User.String()
	host := u.Hostname()
	if uuid == "" || host == "" {
		return DiscoveredProxy{}, fmt.Errorf("vless: missing id or host")
	}
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = 443
	}
	q := u.Query()

	outbound := map[string]any{
		"protocol": "vless",
		"settings": map[string]any{
			"vnext": []map[string]any{{
				"address": host,
				"port":    port,
				"users": []map[string]any{{
					"id":         uuid,
					"encryption": defaultStr(q.Get("encryption"), "none"),
					"flow":       q.Get("flow"),
				}},
			}},
		},
		"streamSettings": streamSettingsFor(streamSettingsOpts{
			network: q.Get("type"), security: q.Get("security"), host: q.Get("host"), path: q.Get("path"), sni: q.Get("sni"),
			fingerprint: q.Get("fp"), publicKey: q.Get("pbk"), shortID: q.Get("sid"), spiderX: q.Get("spx"),
		}),
	}

	return DiscoveredProxy{
		Name:     defaultStr(u.Fragment, host),
		Prober:   "xray",
		Target:   connectivityTestTarget,
		Params:   xrayParams(outbound),
		Identity: xrayIdentity("vless", host, port, uuid, q.Get("type"), q.Get("security"), q.Get("host"), q.Get("path"), q.Get("pbk")),
	}, nil
}
