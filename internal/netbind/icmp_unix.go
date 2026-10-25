//go:build linux || darwin

package netbind

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"syscall"
)

const sysIPStripHdr = 0x17

// ListenICMP opens the same datagram-oriented ICMP socket as
// x/net/icmp's "udp4"/"udp6" mode, but applies the binding before
// bind. x/net/icmp does not expose that socket-control hook.
func (b Binding) ListenICMP(ctx context.Context, network string) (net.PacketConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var family, protocol int
	switch network {
	case "udp4":
		family, protocol = syscall.AF_INET, 1
	case "udp6":
		family, protocol = syscall.AF_INET6, 58
	default:
		return nil, fmt.Errorf("netbind: unsupported ICMP network %q", network)
	}

	fd, err := syscall.Socket(family, syscall.SOCK_DGRAM, protocol)
	if err != nil {
		return nil, os.NewSyscallError("socket", err)
	}
	closeFD := true
	defer func() {
		if closeFD {
			_ = syscall.Close(fd)
		}
	}()

	if runtime.GOOS == "darwin" && family == syscall.AF_INET {
		// Matches x/net/icmp's own Darwin handling: read replies
		// without the IP header prepended.
		if err := syscall.SetsockoptInt(fd, 0, sysIPStripHdr, 1); err != nil {
			return nil, os.NewSyscallError("setsockopt", err)
		}
	}
	if b.Interface != "" {
		if err := controlInterface(network, uintptr(fd), b.Interface); err != nil {
			return nil, err
		}
	}

	sockaddr, err := icmpSockaddr(family, b, network)
	if err != nil {
		return nil, err
	}
	if err := syscall.Bind(fd, sockaddr); err != nil {
		return nil, os.NewSyscallError("bind", err)
	}

	file := os.NewFile(uintptr(fd), "datagram-oriented icmp")
	if file == nil {
		return nil, fmt.Errorf("netbind: wrap ICMP socket")
	}
	// os.NewFile owns the descriptor from here on, including on error.
	closeFD = false
	conn, err := net.FilePacketConn(file)
	_ = file.Close()
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func icmpSockaddr(family int, b Binding, network string) (syscall.Sockaddr, error) {
	source := b.SourceIP
	if source.IsValid() {
		if family == syscall.AF_INET && !source.Is4() {
			return nil, fmt.Errorf("netbind: source %s is not IPv4", source)
		}
		if family == syscall.AF_INET6 && !source.Is6() {
			return nil, fmt.Errorf("netbind: source %s is not IPv6", source)
		}
	}

	if family == syscall.AF_INET {
		var addr [4]byte
		if source.IsValid() {
			addr = source.As4()
		}
		return &syscall.SockaddrInet4{Addr: addr}, nil
	}

	var addr [16]byte
	if source.IsValid() {
		addr = source.As16()
	}
	zoneID := uint32(0)
	if source.IsValid() {
		switch {
		case source.Zone() != "":
			if parsed, err := strconv.ParseUint(source.Zone(), 10, 32); err == nil {
				zoneID = uint32(parsed)
			} else {
				index, err := interfaceIndex(source.Zone())
				if err != nil {
					return nil, err
				}
				zoneID = uint32(index)
			}
		case source.IsLinkLocalUnicast() && b.Interface != "":
			index, err := interfaceIndex(b.Interface)
			if err != nil {
				return nil, err
			}
			zoneID = uint32(index)
		}
	}
	return &syscall.SockaddrInet6{Addr: addr, ZoneId: zoneID}, nil
}
