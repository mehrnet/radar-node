// Package portalloc finds free local TCP ports for a subprocess (an
// xray/sing-box instance, say) to bind, and waits for one to become
// ready to accept connections. Shared by internal/module's
// prepare/alloc_port mechanism and any native action that needs to
// launch and talk to a local proxy engine itself.
package portalloc

import (
	"context"
	"fmt"
	"net"
	"time"
)

// Alloc reserves a free TCP port by briefly binding to it, then
// releases it before the caller's real listener binds. This has an
// unavoidable small TOCTOU race (another process could grab the port
// first) but is the standard, good-enough approach every "find a free
// port for a subprocess" utility uses.
func Alloc() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}

// WaitForPort blocks until port accepts a connection or ctx is done.
func WaitForPort(ctx context.Context, port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("did not become ready on port %d: %w", port, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}
