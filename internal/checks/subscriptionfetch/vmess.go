package subscriptionfetch

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// vmessShare is the de-facto "vmess share standard" JSON payload
// carried base64-encoded after "vmess://" -- field names are the
// short, fixed abbreviations every vmess-generating tool already uses
// (ps=name, add=address, aid=alterId, scy=security/cipher, net=
// transport, tls=security layer), not something this package invents.
type vmessShare struct {
	Ps   string `json:"ps"`
	Add  string `json:"add"`
	Port any    `json:"port"` // some generators emit a string, some a number
	ID   string `json:"id"`
	Aid  any    `json:"aid"`
	Scy  string `json:"scy"`
	Net  string `json:"net"`
	Host string `json:"host"`
	Path string `json:"path"`
	TLS  string `json:"tls"`
	SNI  string `json:"sni"`
}

func parseVmess(line string) (DiscoveredProxy, error) {
	encoded := strings.TrimPrefix(line, "vmess://")
	decoded, ok := tryBase64(encoded)
	if !ok {
		return DiscoveredProxy{}, fmt.Errorf("vmess: not valid base64")
	}

	var v vmessShare
	if err := json.Unmarshal([]byte(decoded), &v); err != nil {
		return DiscoveredProxy{}, fmt.Errorf("vmess: %w", err)
	}
	if v.Add == "" || v.ID == "" {
		return DiscoveredProxy{}, fmt.Errorf("vmess: missing address or id")
	}

	port := anyToInt(v.Port, 443)
	alterID := anyToInt(v.Aid, 0)

	outbound := map[string]any{
		"protocol": "vmess",
		"settings": map[string]any{
			"vnext": []map[string]any{{
				"address": v.Add,
				"port":    port,
				"users": []map[string]any{{
					"id":       v.ID,
					"alterId":  alterID,
					"security": defaultStr(v.Scy, "auto"),
				}},
			}},
		},
		"streamSettings": streamSettingsFor(v.Net, tlsToSecurity(v.TLS), v.Host, v.Path, v.SNI),
	}

	return DiscoveredProxy{
		Name:   defaultStr(v.Ps, v.Add),
		Prober: "xray",
		Target: connectivityTestTarget,
		Params: xrayParams(outbound),
	}, nil
}

// tlsToSecurity normalizes the vmess share standard's own `tls` field
// ("tls", "", or occasionally "none") to the same "security" vocabulary
// vless/trojan URIs already use natively, so streamSettingsFor can
// handle every scheme identically.
func tlsToSecurity(tls string) string {
	if tls == "tls" {
		return "tls"
	}
	return ""
}

func anyToInt(v any, def int) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case string:
		if n, err := strconv.Atoi(t); err == nil {
			return n
		}
	}
	return def
}
