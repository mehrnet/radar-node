package subscriptionfetch

import (
	"fmt"
	"net/url"
	"regexp"
)

// hostPortRe matches a bare "host:port" line (no scheme) -- the
// simplest possible proxy-list entry.
var hostPortRe = regexp.MustCompile(`^[^\s/]+:\d+$`)

// parsePlainProxy handles the non-v2ray proxy-list case: a bare
// host:port, or a scheme://[user:pass@]host:port line for a plain
// HTTP or SOCKS proxy (not one of the v2ray-family schemes, which
// parseLine already dispatched away from this function). These become
// "tcp" probes against the proxy's own address -- a basic reachability
// check, not a real protocol-aware proxy test (verifying an HTTP
// CONNECT or SOCKS handshake actually works is its own scope, deferred
// -- see this feature's own plan). Auth credentials, if present, are
// parsed but currently unused: the tcp prober has no use for them yet.
func parsePlainProxy(line string) (DiscoveredProxy, error) {
	if hostPortRe.MatchString(line) {
		return DiscoveredProxy{
			Name:   line,
			Prober: "tcp",
			Target: line,
			Params: map[string]any{},
		}, nil
	}

	u, err := url.Parse(line)
	if err != nil || u.Hostname() == "" || u.Port() == "" {
		return DiscoveredProxy{}, fmt.Errorf("proxylist: unrecognized line")
	}
	hostPort := u.Hostname() + ":" + u.Port()
	params := map[string]any{}
	if u.User != nil {
		if username := u.User.Username(); username != "" {
			params["username"] = username
		}
		if password, ok := u.User.Password(); ok {
			params["password"] = password
		}
	}
	return DiscoveredProxy{
		Name:   defaultStr(u.Fragment, hostPort),
		Prober: "tcp",
		Target: hostPort,
		Params: params,
	}, nil
}
