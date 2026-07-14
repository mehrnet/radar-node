// Package action is the library of native Go implementations a
// module's YAML `action:` field can reference, called in-process
// instead of spawning a `command:` subprocess. Every module is still
// just a file; `action:` only picks which built-in does the work.
// Most actions here have zero subprocess overhead (tcp/udp/dns/icmp/
// http/system); xray_proxy_test/singbox_proxy_test are the exception
// -- they still shell out to the engine binary, since reimplementing
// an xray/sing-box-compatible client isn't reasonable, but the
// protocol-agnostic config handling and port remapping around that
// subprocess is real Go logic that doesn't fit the argv-template
// `command:` model, so it lives here rather than as a `run:` module.
package action

import (
	"github.com/mehrnet/radar-node/internal/checks/dns"
	"github.com/mehrnet/radar-node/internal/checks/httpcheck"
	"github.com/mehrnet/radar-node/internal/checks/icmp"
	"github.com/mehrnet/radar-node/internal/checks/proxytest"
	"github.com/mehrnet/radar-node/internal/checks/system"
	"github.com/mehrnet/radar-node/internal/checks/tcp"
	"github.com/mehrnet/radar-node/internal/checks/udp"
	"github.com/mehrnet/radar-node/internal/probe"
)

// Registry maps an action name to its implementation. Action names
// are an internal library, distinct from prober names: a loaded
// module's `name:` is what a job's `prober` field refers to, and any
// number of differently-configured modules can reference the same
// action.
var Registry = map[string]probe.Checker{
	"tcp_connect":        tcp.New(),
	"udp_probe":          udp.New(),
	"dns_query":          dns.New(),
	"icmp_ping":          icmp.New(),
	"http_request":       httpcheck.New(),
	"system_stats":       system.New(),
	"xray_proxy_test":    proxytest.NewXray(),
	"singbox_proxy_test": proxytest.NewSingBox(),
}

// Get returns the Checker registered under name, if any.
func Get(name string) (probe.Checker, bool) {
	c, ok := Registry[name]
	return c, ok
}
