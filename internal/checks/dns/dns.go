// Package dns implements a DNS resolution check.
package dns

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/mehrnet/radar-node/internal/probe"
)

type Checker struct{}

func New() Checker { return Checker{} }

func (Checker) Type() string { return "dns" }

// Check resolves opts.Target (a hostname). Supported params:
//
//	record: "a" (default) or "aaaa"
//	server: "host:port" of a specific DNS server to query instead of
//	        the system resolver
func (c Checker) Check(ctx context.Context, opts probe.Options) probe.Result {
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	resolver := net.DefaultResolver
	if server := opts.Param("server", ""); server != "" {
		resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{}
				return d.DialContext(ctx, network, server)
			},
		}
	}

	record := opts.Param("record", "a")
	network := "ip4"
	if record == "aaaa" {
		network = "ip6"
	}

	start := time.Now()
	addrs, err := resolver.LookupNetIP(ctx, network, opts.Target)
	elapsed := time.Since(start)
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, err)
	}
	if len(addrs) == 0 {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, fmt.Errorf("no %s records found", record))
	}

	records := make([]string, len(addrs))
	for i, a := range addrs {
		records[i] = a.Unmap().String()
	}

	return probe.Ok(c.Type(), opts.Target, opts.Mode, opts.Seq, elapsed, map[string]any{
		"record":  record,
		"answers": records,
	})
}
