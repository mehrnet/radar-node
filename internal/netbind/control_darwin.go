//go:build darwin

package netbind

import (
	"strings"

	"golang.org/x/sys/unix"
)

func controlInterface(network string, fd uintptr, iface string) error {
	apply := func() error {
		index, err := interfaceIndex(iface)
		if err != nil {
			return err
		}
		if strings.HasSuffix(network, "6") {
			return unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, index)
		}
		return unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, index)
	}
	if err := apply(); err != nil {
		// A recreated interface can change its index; refresh once.
		if _, refreshErr := refreshInterfaceIndex(iface); refreshErr != nil {
			return err
		}
		return apply()
	}
	return nil
}
