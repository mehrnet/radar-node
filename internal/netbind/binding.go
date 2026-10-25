// Package netbind selects the local source address and/or egress
// interface used by a node's probe connections. A zero Binding keeps
// the OS default path, exactly as before this package existed.
package netbind

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"syscall"
)

const (
	EnvSourceIP      = "RADAR_SOURCE_IP"
	EnvBindInterface = "RADAR_BIND_INTERFACE"
	maxInterfaceLen  = 64
)

// Binding is immutable for the life of a process. SourceIP and
// Interface are optional and compose: the interface constrains egress,
// the source address selects which local address to use.
type Binding struct {
	SourceIP  netip.Addr
	Interface string
}

// Parse validates operator-supplied source/interface values. An
// interface that does not exist yet is accepted here; SelfTest reports
// that at runtime so a cellular interface may appear after boot.
func Parse(sourceIP, iface string) (Binding, error) {
	var binding Binding

	if raw := strings.TrimSpace(sourceIP); raw != "" {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return Binding{}, fmt.Errorf("invalid source IP %q: %w", raw, err)
		}
		addr = addr.Unmap()
		if addr.IsUnspecified() || addr.IsMulticast() {
			return Binding{}, fmt.Errorf("invalid source IP %q: must be a concrete unicast address", raw)
		}
		binding.SourceIP = addr
	}

	binding.Interface = strings.TrimSpace(iface)
	if binding.Interface != "" {
		if len(binding.Interface) > maxInterfaceLen {
			return Binding{}, fmt.Errorf("bind interface name is longer than %d bytes", maxInterfaceLen)
		}
		for _, r := range binding.Interface {
			if r < 0x20 || r == 0x7f {
				return Binding{}, fmt.Errorf("bind interface name contains a control character")
			}
		}
	}
	if binding.SourceIP.Is6() && binding.SourceIP.IsLinkLocalUnicast() &&
		binding.SourceIP.Zone() == "" && binding.Interface != "" {
		binding.SourceIP = binding.SourceIP.WithZone(binding.Interface)
	}

	return binding, nil
}

func (b Binding) IsZero() bool {
	return !b.SourceIP.IsValid() && b.Interface == ""
}

func (b Binding) String() string {
	var parts []string
	if b.SourceIP.IsValid() {
		parts = append(parts, "source "+b.SourceIP.String())
	}
	if b.Interface != "" {
		parts = append(parts, "interface "+b.Interface)
	}
	return strings.Join(parts, ", ")
}

// Network forces the address family when a source IP is configured, so
// a hostname target cannot resolve to an address the bound socket
// cannot use.
func (b Binding) Network(base string) string {
	if !b.SourceIP.IsValid() {
		return base
	}
	switch base {
	case "tcp", "udp", "ip":
	default:
		return base
	}
	if b.SourceIP.Is4() {
		return base + "4"
	}
	return base + "6"
}

// Dialer returns a dialer carrying the binding and, when any binding
// is configured, a resolver whose nameserver traffic uses the same
// path. The resolver's own inner dial carries the binding but no
// resolver, which is what avoids recursion.
func (b Binding) Dialer(network string) *net.Dialer {
	dialer := b.dialer(network)
	if !b.IsZero() {
		dialer.Resolver = b.resolver()
	}
	return dialer
}

func (b Binding) dialer(network string) *net.Dialer {
	dialer := &net.Dialer{}
	if b.SourceIP.IsValid() {
		ip := net.IP(b.SourceIP.AsSlice())
		if strings.HasPrefix(network, "udp") {
			dialer.LocalAddr = &net.UDPAddr{IP: ip, Zone: b.SourceIP.Zone()}
		} else {
			dialer.LocalAddr = &net.TCPAddr{IP: ip, Zone: b.SourceIP.Zone()}
		}
	}
	if b.Interface != "" {
		dialer.Control = b.control
	}
	return dialer
}

// DialContext is suitable for net/http transports and other code that
// wants the binding without constructing its own dialer.
func (b Binding) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return b.Dialer(network).DialContext(ctx, b.Network(network), address)
}

// Resolver returns nil for a zero binding so callers can preserve the
// platform default resolver path exactly.
func (b Binding) Resolver() *net.Resolver {
	if b.IsZero() {
		return nil
	}
	return b.resolver()
}

func (b Binding) resolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return b.dialer(network).DialContext(ctx, b.Network(network), address)
		},
	}
}

// LookupNetIP resolves through the bound resolver when configured.
func (b Binding) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	if b.IsZero() {
		return net.DefaultResolver.LookupNetIP(ctx, network, host)
	}
	return b.resolver().LookupNetIP(ctx, network, host)
}

// ResolveIPAddr is the ICMP-friendly equivalent of LookupNetIP.
func (b Binding) ResolveIPAddr(ctx context.Context, network, host string) (*net.IPAddr, error) {
	if b.IsZero() {
		return net.ResolveIPAddr(network, host)
	}
	addrs, err := b.LookupNetIP(ctx, network, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, &net.AddrError{Err: "no suitable address", Addr: host}
	}
	return &net.IPAddr{IP: net.IP(addrs[0].AsSlice()), Zone: addrs[0].Zone()}, nil
}

// SelfTest verifies the interface exists, the source address is
// available, and this process may apply the platform bind option. It
// deliberately does not send traffic, so it cannot be mistaken for a
// target check.
func (b Binding) SelfTest(ctx context.Context) error {
	if b.IsZero() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	network := "udp"
	address := ":0"
	if b.SourceIP.IsValid() {
		network = b.Network("udp")
		address = net.JoinHostPort(b.SourceIP.String(), "0")
	}
	listenConfig := net.ListenConfig{}
	if b.Interface != "" {
		listenConfig.Control = b.control
	}
	conn, err := listenConfig.ListenPacket(ctx, network, address)
	if err != nil {
		return fmt.Errorf("outbound binding %s: %w", b.String(), err)
	}
	_ = conn.Close()
	return nil
}

func (b Binding) control(network, address string, raw syscall.RawConn) error {
	if b.Interface == "" {
		return nil
	}
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		sockErr = controlInterface(network, fd, b.Interface)
	}); err != nil {
		return err
	}
	if sockErr != nil {
		return fmt.Errorf("bind interface %q: %w", b.Interface, sockErr)
	}
	return nil
}

var interfaceIndexes sync.Map

func interfaceIndex(name string) (int, error) {
	if cached, ok := interfaceIndexes.Load(name); ok {
		return cached.(int), nil
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return 0, err
	}
	interfaceIndexes.Store(name, iface.Index)
	return iface.Index, nil
}

func refreshInterfaceIndex(name string) (int, error) {
	interfaceIndexes.Delete(name)
	return interfaceIndex(name)
}

// Env merges the binding into base, replacing any inherited values so
// a module subprocess can never silently use a different WAN.
func Env(base []string, b Binding) []string {
	out := make([]string, 0, len(base)+2)
	for _, entry := range base {
		if strings.HasPrefix(entry, EnvSourceIP+"=") || strings.HasPrefix(entry, EnvBindInterface+"=") {
			continue
		}
		out = append(out, entry)
	}
	if b.SourceIP.IsValid() {
		out = append(out, EnvSourceIP+"="+b.SourceIP.String())
	}
	if b.Interface != "" {
		out = append(out, EnvBindInterface+"="+b.Interface)
	}
	return out
}

func FromEnv() (Binding, error) {
	return Parse(os.Getenv(EnvSourceIP), os.Getenv(EnvBindInterface))
}
