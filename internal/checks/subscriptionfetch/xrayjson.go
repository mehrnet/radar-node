package subscriptionfetch

import (
	"encoding/json"
	"fmt"
)

// skippedOutboundProtocols are xray's own routing-plumbing outbounds
// (a real client config always has a "direct"/"freedom" pass-through
// and often a "block"), never a proxy someone actually wants monitored
// -- present in most real-world xray client configs alongside the
// proxy outbound(s) that are.
var skippedOutboundProtocols = map[string]bool{
	"freedom":   true,
	"blackhole": true,
}

// parseXrayJSON handles a full xray/v2ray client config supplied
// directly as the subscription content (rather than one URI per line)
// -- each non-routing outbound becomes its own discovered proxy, given
// its own single-outbound config (see buildXrayConfig) rather than the
// original multi-outbound one, so each is tested in isolation.
func parseXrayJSON(content string) ([]DiscoveredProxy, error) {
	var doc struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		return nil, fmt.Errorf("xray json: %w", err)
	}

	var proxies []DiscoveredProxy
	for i, outbound := range doc.Outbounds {
		protocol, _ := outbound["protocol"].(string)
		if skippedOutboundProtocols[protocol] {
			continue
		}
		tag, _ := outbound["tag"].(string)
		name := tag
		if name == "" {
			name = fmt.Sprintf("%s-%d", defaultStr(protocol, "proxy"), i)
		}
		proxies = append(proxies, DiscoveredProxy{
			Name:   name,
			Prober: "xray",
			Target: connectivityTestTarget,
			Params: xrayParams(outbound),
		})
	}
	return proxies, nil
}
