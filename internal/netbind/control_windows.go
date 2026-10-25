//go:build windows

package netbind

import (
	"math/bits"
	"strings"

	"golang.org/x/sys/windows"
)

const (
	ipUnicastIF   = 31
	ipv6UnicastIF = 31
)

func controlInterface(network string, fd uintptr, iface string) error {
	apply := func() error {
		index, err := interfaceIndex(iface)
		if err != nil {
			return err
		}
		if strings.HasSuffix(network, "6") {
			return windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IPV6, ipv6UnicastIF, index)
		}
		// Windows requires the IPv4 interface index in network byte order.
		return windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, ipUnicastIF, int(bits.ReverseBytes32(uint32(index))))
	}
	if err := apply(); err != nil {
		if _, refreshErr := refreshInterfaceIndex(iface); refreshErr != nil {
			return err
		}
		return apply()
	}
	return nil
}
