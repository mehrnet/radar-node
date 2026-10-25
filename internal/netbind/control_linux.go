//go:build linux

package netbind

import "golang.org/x/sys/unix"

// SO_BINDTODEVICE requires CAP_NET_RAW (root on the nodes that need
// multi-WAN interface selection).
func controlInterface(_ string, fd uintptr, iface string) error {
	return unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, iface)
}
