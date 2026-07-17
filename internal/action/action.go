// Package action is the library of native Go implementations a
// module's YAML `action:` field can reference, called in-process
// instead of spawning a `command:` subprocess. Every module is still
// just a file; `action:` only picks which built-in does the work.
//
// xray_proxy_test/singbox_proxy_test used to live here (shelling out
// to whatever xray/sing-box binary an operator had manually placed on
// PATH or pointed at via XRAY_BIN/SINGBOX_BIN) -- removed now that
// engine binaries are a managed install.sh concern (see
// --install-xray/--install-wireguard/--install-openvpn, fetched from
// mehrnet/static-builds), not something this binary reaches for
// itself. The equivalent probers are ordinary run:-based modules now
// (see examples/modules/xray.yaml), like wireguard/openvpn always
// were, rather than a special-cased built-in action for xray/sing-box
// specifically.
package action

import (
	"github.com/mehrnet/radar-node/internal/checks/dns"
	"github.com/mehrnet/radar-node/internal/checks/httpcheck"
	"github.com/mehrnet/radar-node/internal/checks/icmp"
	"github.com/mehrnet/radar-node/internal/checks/system"
	"github.com/mehrnet/radar-node/internal/checks/tcp"
	"github.com/mehrnet/radar-node/internal/checks/udp"
	"github.com/mehrnet/radar-node/internal/probe"
)

// Registry maps an action name to its implementation. Action names
// are an internal library, distinct from prober names: a loaded
// module's `name:` is what a probe's `prober` field refers to, and any
// number of differently-configured modules can reference the same
// action.
var Registry = map[string]probe.Checker{
	"tcp_connect":  tcp.New(),
	"udp_probe":    udp.New(),
	"dns_query":    dns.New(),
	"icmp_ping":    icmp.New(),
	"http_request": httpcheck.New(),
	"system_stats": system.New(),
}

// Get returns the Checker registered under name, if any.
func Get(name string) (probe.Checker, bool) {
	c, ok := Registry[name]
	return c, ok
}
