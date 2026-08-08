package subscriptionfetch

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// parseShadowsocks handles both the current SIP002 URI form
// (ss://base64(method:password)@host:port#name, userinfo alone is
// encoded) and the older legacy form (ss://base64(method:password@
// host:port)#name, the entire authority is encoded) -- SIP002 is
// tried first since it's what every current generator emits.
func parseShadowsocks(line string) (DiscoveredProxy, error) {
	if proxy, err := parseShadowsocksSIP002(line); err == nil {
		return proxy, nil
	}
	return parseShadowsocksLegacy(line)
}

func parseShadowsocksSIP002(line string) (DiscoveredProxy, error) {
	u, err := url.Parse(line)
	if err != nil || u.Hostname() == "" || u.User == nil {
		return DiscoveredProxy{}, fmt.Errorf("shadowsocks: not SIP002 form")
	}
	decoded, ok := tryBase64(u.User.String())
	if !ok {
		return DiscoveredProxy{}, fmt.Errorf("shadowsocks: userinfo not base64")
	}
	method, password, ok := strings.Cut(decoded, ":")
	if !ok {
		return DiscoveredProxy{}, fmt.Errorf("shadowsocks: malformed method:password")
	}
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		return DiscoveredProxy{}, fmt.Errorf("shadowsocks: missing port")
	}
	return buildShadowsocksProxy(defaultStr(u.Fragment, u.Hostname()), method, password, u.Hostname(), port), nil
}

func parseShadowsocksLegacy(line string) (DiscoveredProxy, error) {
	rest := strings.TrimPrefix(line, "ss://")
	rest, fragment, _ := strings.Cut(rest, "#")
	rest, _, _ = strings.Cut(rest, "?")
	decoded, ok := tryBase64(rest)
	if !ok {
		return DiscoveredProxy{}, fmt.Errorf("shadowsocks: not valid base64")
	}
	methodPass, hostPort, ok := strings.Cut(decoded, "@")
	if !ok {
		return DiscoveredProxy{}, fmt.Errorf("shadowsocks: malformed legacy uri")
	}
	method, password, ok := strings.Cut(methodPass, ":")
	if !ok {
		return DiscoveredProxy{}, fmt.Errorf("shadowsocks: malformed method:password")
	}
	host, portStr, ok := strings.Cut(hostPort, ":")
	if !ok {
		return DiscoveredProxy{}, fmt.Errorf("shadowsocks: malformed host:port")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return DiscoveredProxy{}, fmt.Errorf("shadowsocks: invalid port")
	}
	name, _ := url.QueryUnescape(fragment)
	return buildShadowsocksProxy(defaultStr(name, host), method, password, host, port), nil
}

func buildShadowsocksProxy(name, method, password, host string, port int) DiscoveredProxy {
	outbound := map[string]any{
		"protocol": "shadowsocks",
		"settings": map[string]any{
			"servers": []map[string]any{{
				"address":  host,
				"port":     port,
				"method":   method,
				"password": password,
			}},
		},
	}
	return DiscoveredProxy{
		Name:   name,
		Prober: "xray",
		Target: connectivityTestTarget,
		Params: xrayParams(outbound),
	}
}
