//go:build !linux && !darwin

package netbind

import (
	"context"
	"fmt"
	"net"
	"runtime"
)

func (b Binding) ListenICMP(context.Context, string) (net.PacketConn, error) {
	return nil, fmt.Errorf("ICMP interface binding is not supported on %s; use --source-ip or a network namespace", runtime.GOOS)
}
