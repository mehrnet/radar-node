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

// optionalRecordTypes are the ones where a query that comes back with
// zero results is a legitimate, common state, not a failure -- a
// domain not accepting mail (mx), not publishing any TXT records, not
// having a CNAME (it's a plain A/AAAA name instead), or an IP with no
// reverse DNS entry (ptr) are all completely normal. a, aaaa, and ns
// are deliberately NOT here: a hostname that doesn't resolve at all,
// or a domain with no nameservers, both mean the domain itself is
// broken, not "this optional feature isn't in use" -- exactly the
// original, only, behavior before ns/mx/txt/cname/ptr were added.
//
// Go's resolver reports both cases -- NXDOMAIN (the name doesn't exist
// at all) and NODATA (the name exists, just not with this record
// type) -- as the exact same *net.DNSError with IsNotFound set; there
// is no way to tell them apart through the standard library short of
// a full DNS client able to inspect the raw response code. This means
// an optional-type check on a genuinely nonexistent domain reads as
// "ok, zero records" here rather than failing -- an accepted, disclosed
// limitation of staying on net.Resolver instead of a dedicated DNS
// library: pair an ns or a/aaaa check on the same target for that.
var optionalRecordTypes = map[string]bool{"mx": true, "txt": true, "cname": true, "ptr": true}

// isNotFound reports whether err is the specific "resolved with zero
// results" shape net.Resolver produces for NXDOMAIN/NODATA alike (see
// optionalRecordTypes above) -- as opposed to a real transport failure
// (timeout, refused, servfail), which stays a hard failure regardless
// of record type.
func isNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// Check resolves opts.Target against the record type named by the
// "record" param (default "a"). Supported values: a, aaaa, ns, mx,
// txt, cname, ptr. For ptr, opts.Target must be an IP address (a
// reverse lookup has no other kind of target). Supported params:
//
//	record: "a" (default), "aaaa", "ns", "mx", "txt", "cname", or "ptr"
//	server: "host:port" of a specific DNS server to query instead of
//	        the system resolver
//
// Every record type reports its results as answers ([]string) --
// mx additionally reports each answer's own preference value as a
// parallel preferences ([]int) array, since a mail exchanger's
// priority isn't something a caller can drop without losing real
// information the way it can for e.g. a TXT record's own ordering.
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
	answers, extra, err := lookup(ctx, resolver, record, opts.Target)
	elapsed := time.Since(start)
	if err != nil && optionalRecordTypes[record] && isNotFound(err) {
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
// every result to a flat []string (mx's own preference values ride
// along in extra instead, see Check's own doc comment) -- a uniform
// shape callers can render the same way regardless of which type was
// actually queried.
func lookup(ctx context.Context, resolver *net.Resolver, record, target string) (answers []string, extra map[string]any, err error) {
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

	default:
		return nil, nil, fmt.Errorf("unsupported record type %q -- expected a, aaaa, ns, mx, txt, cname, or ptr", record)
	}
}
