// Package dns implements a DNS resolution check.
package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/mehrnet/radar-node/internal/probe"
)

type Checker struct{}

func New() Checker { return Checker{} }

func (Checker) Type() string { return "dns" }

// isNotFound reports whether err is the specific "resolved with zero
// results" shape net.Resolver produces, for every record type this
// checker supports -- Ok, not Fail, either way (see Check's own doc
// comment on why zero results is never itself a failure here). Go's
// resolver reports both NXDOMAIN (the name doesn't exist at all) and
// NODATA (the name exists, just not with this record type) as this
// exact same *net.DNSError with IsNotFound set; there's no way to
// tell them apart through the standard library short of a full DNS
// client able to inspect the raw response code, so both alike come
// back as "ok, zero answers" here. A real transport failure (timeout,
// refused, servfail) is a different DNSError shape and stays a
// genuine Fail, same as a caller/config error (an unsupported record
// type, a non-IP ptr target) that never reaches the network at all.
func isNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// Check resolves opts.Target against the record type named by the
// "record" param (default "a"). Supported values: a, aaaa, ns, mx,
// txt, cname, ptr, srv. For ptr, opts.Target must be an IP address (a
// reverse lookup has no other kind of target). Supported params:
//
//	record:  "a" (default), "aaaa", "ns", "mx", "txt", "cname", "ptr", or "srv"
//	server:  "host:port" of a specific DNS server to query instead of
//	         the system resolver
//	service: srv only -- e.g. "sip". Combined with proto to build the
//	         standard _service._proto.<target> query name (RFC 2782).
//	         Leave both service and proto empty to query opts.Target
//	         directly as an already-fully-formed SRV name instead.
//	proto:   srv only -- "tcp" or "udp", paired with service above.
//
// Every record type reports its results as answers ([]string) -- mx
// additionally reports each answer's own preference in a parallel
// preferences ([]int) array, and srv reports parallel priorities/
// weights/ports ([]int each), since none of that is something a
// caller could drop without losing real information a plain string
// can't carry.
//
// A query resolving successfully but finding zero records of the
// requested type is Ok, never Fail, for every record type -- a domain
// not accepting mail, not publishing TXT records, having no CNAME,
// lacking reverse DNS, or (yes, even) having no A/AAAA/NS records
// under this particular query are all just facts a caller building
// their own domain-health tooling on top of this data can act on
// however they choose; this check's own Ok only ever answers "did the
// DNS query itself succeed," never "did I like what it found." A
// genuine failure -- timeout, refused, servfail, an unsupported record
// type, a non-IP ptr target -- still fails normally.
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

	record := strings.ToLower(opts.Param("record", "a"))
	start := time.Now()
	answers, extra, err := lookup(ctx, resolver, record, opts)
	elapsed := time.Since(start)
	if err != nil && isNotFound(err) {
		err = nil
		answers = []string{}
	}
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Seq, err)
	}

	result := map[string]any{"record": record, "answers": answers}
	for k, v := range extra {
		result[k] = v
	}
	return probe.Ok(c.Type(), opts.Target, opts.Seq, elapsed, result)
}

// lookup dispatches to the net.Resolver method for record, normalizing
// every result to a flat []string (a record type's own extra
// structured data -- mx's preferences, srv's priorities/weights/ports
// -- rides along in extra instead, see Check's own doc comment) -- a
// uniform shape callers can render the same way regardless of which
// type was actually queried.
func lookup(ctx context.Context, resolver *net.Resolver, record string, opts probe.Options) (answers []string, extra map[string]any, err error) {
	target := opts.Target
	switch record {
	case "a", "aaaa":
		network := "ip4"
		if record == "aaaa" {
			network = "ip6"
		}
		addrs, lookupErr := resolver.LookupNetIP(ctx, network, target)
		if lookupErr != nil {
			return nil, nil, lookupErr
		}
		answers = make([]string, len(addrs))
		for i, a := range addrs {
			answers[i] = a.Unmap().String()
		}
		return answers, nil, nil

	case "ns":
		recs, lookupErr := resolver.LookupNS(ctx, target)
		if lookupErr != nil {
			return nil, nil, lookupErr
		}
		answers = make([]string, len(recs))
		for i, r := range recs {
			answers[i] = strings.TrimSuffix(r.Host, ".")
		}
		return answers, nil, nil

	case "mx":
		recs, lookupErr := resolver.LookupMX(ctx, target)
		if lookupErr != nil {
			return nil, nil, lookupErr
		}
		answers = make([]string, len(recs))
		preferences := make([]int, len(recs))
		for i, r := range recs {
			answers[i] = strings.TrimSuffix(r.Host, ".")
			preferences[i] = int(r.Pref)
		}
		return answers, map[string]any{"preferences": preferences}, nil

	case "txt":
		recs, lookupErr := resolver.LookupTXT(ctx, target)
		if lookupErr != nil {
			return nil, nil, lookupErr
		}
		return recs, nil, nil

	case "cname":
		cname, lookupErr := resolver.LookupCNAME(ctx, target)
		if lookupErr != nil {
			return nil, nil, lookupErr
		}
		return []string{strings.TrimSuffix(cname, ".")}, nil, nil

	case "ptr":
		if net.ParseIP(target) == nil {
			return nil, nil, fmt.Errorf("target must be an IP address for a PTR lookup, got %q", target)
		}
		recs, lookupErr := resolver.LookupAddr(ctx, target)
		if lookupErr != nil {
			return nil, nil, lookupErr
		}
		answers = make([]string, len(recs))
		for i, r := range recs {
			answers[i] = strings.TrimSuffix(r, ".")
		}
		return answers, nil, nil

	case "srv":
		// RFC 2782's own construction (_service._proto.name) when both
		// are given; an operator who already has the full SRV query
		// name in hand can instead leave both empty and put it straight
		// in target -- LookupSRV treats that as a literal name rather
		// than adding an empty ".." prefix (see its own doc comment).
		service := opts.Param("service", "")
		proto := opts.Param("proto", "")
		_, recs, lookupErr := resolver.LookupSRV(ctx, service, proto, target)
		if lookupErr != nil {
			return nil, nil, lookupErr
		}
		answers = make([]string, len(recs))
		priorities := make([]int, len(recs))
		weights := make([]int, len(recs))
		ports := make([]int, len(recs))
		for i, r := range recs {
			answers[i] = strings.TrimSuffix(r.Target, ".")
			priorities[i] = int(r.Priority)
			weights[i] = int(r.Weight)
			ports[i] = int(r.Port)
		}
		return answers, map[string]any{"priorities": priorities, "weights": weights, "ports": ports}, nil

	default:
		return nil, nil, fmt.Errorf("unsupported record type %q -- expected a, aaaa, ns, mx, txt, cname, ptr, or srv", record)
	}
}
