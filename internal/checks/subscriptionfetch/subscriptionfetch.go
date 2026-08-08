// Package subscriptionfetch implements the "subscription_fetch" prober:
// fetches a URL (via internal/checks/fetch, called directly -- there's
// no action-to-action call mechanism, and none is needed for a plain
// Go import) and parses it strictly as whichever SubscriptionType the
// probe's own "type" param says (base64-xray by default) -- a raw or
// base64-encoded URI list, a full xray-style JSON config, or a plain
// host:port proxy list -- into one unified list of discovered proxies,
// riding back to radar-api in the check's own Extra field. radar-api,
// not this package, decides what to do with that list (create/archive/
// leave-alone probes by content hash) -- this package's only job is
// turning whatever a subscription URL returns into that one normalized
// shape.
package subscriptionfetch

import (
	"context"
	"time"

	"github.com/mehrnet/radar-node/internal/checks/fetch"
	"github.com/mehrnet/radar-node/internal/probe"
)

// DiscoveredProxy is one entry in the unified list -- enough to
// construct an ordinary probe from directly: Prober/Target/Params are
// exactly a probe's own fields of the same name. radar-api computes
// each entry's own content hash from Params (and Prober/Target) once
// it receives this list; nothing here is content-hash-aware.
type DiscoveredProxy struct {
	Name   string         `json:"name"`
	Prober string         `json:"prober"`
	Target string         `json:"target"`
	Params map[string]any `json:"params"`
}

type Checker struct{}

func New() Checker { return Checker{} }

func (Checker) Type() string { return "subscription_fetch" }

func (c Checker) Check(ctx context.Context, opts probe.Options) probe.Result {
	start := time.Now()
	body, _, err := fetch.Do(ctx, opts)
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, err)
	}

	subType := SubscriptionType(opts.Param("type", string(Base64Xray)))
	proxies, err := Parse(body, subType)
	if err != nil {
		return probe.Fail(c.Type(), opts.Target, opts.Mode, opts.Seq, err)
	}
	elapsed := time.Since(start)

	// []DiscoveredProxy, not []map[string]any -- json.Marshal produces
	// the identical wire shape either way (struct tags match the field
	// names radar-api expects), but the typed slice is what every
	// parser function below actually builds and returns, so there's no
	// reason to flatten it to untyped maps here first.
	extra := map[string]any{
		"proxies":    proxies,
		"discovered": len(proxies),
	}
	return probe.Ok(c.Type(), opts.Target, opts.Mode, opts.Seq, elapsed, extra)
}
