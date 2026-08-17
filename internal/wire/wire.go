// Package wire defines the JSON shapes exchanged between radar-node
// and radar-api, per README.md. Changing a field here is a
// breaking change to that spec and must be reflected in SpecVersion.
//
// There is no more server-computed dispatch. A node syncs its own
// probe assignment via content-hash comparison (POST /v1/nodes/
// heartbeat's probe_hash/probes, spec_version 2+) rather than an
// incremental delta stream -- it sends its own last-known hash, and
// the server replies either "you're caught up" (hash matches, nothing
// sent back) or the full current assignment (mismatch), which this
// agent applies as a replace, not a merge. Events still carries
// "triggered" entries only (manual/Quick Check runs), delivered
// inline on every heartbeat regardless of hash match -- unaffected by
// this protocol. Results are keyed by a node-generated RunID instead
// of a server-issued assignment id.
package wire

import (
	"github.com/mehrnet/radar-node/internal/module"
	"github.com/mehrnet/radar-node/internal/probe"
)

const SpecVersion = 2

// ProbeSnapshot is the full current definition of a probe, as carried
// by every Event -- a node applies this directly to its local cache
// with no follow-up lookup of any kind.
type ProbeSnapshot struct {
	ID              string         `json:"id"`
	Target          string         `json:"target"`
	Prober          string         `json:"prober"`
	ProbeCount      int            `json:"probe_count"`
	TimeoutMs       int            `json:"timeout_ms"`
	ScheduleType    string         `json:"schedule_type"` // "manual" | "interval"
	IntervalSeconds int            `json:"interval_seconds,omitempty"`
	StartsAt        int64          `json:"starts_at"`
	EndsAt          int64          `json:"ends_at,omitempty"`
	Params          map[string]any `json:"params,omitempty"`
	Status          string         `json:"status"` // "active" | "paused" | "archived" | "inactive_billing"
}

// Event is one entry from HeartbeatResponse.Events -- as of
// SpecVersion 2, only ever a "triggered" (manual/Quick Check run)
// entry; probe-assignment changes are carried by ProbeHash/Probes
// instead (see HeartbeatResponse). Seq is informational only now
// (which probe_events row this came from), not a sync cursor -- there
// is no more since_seq.
type Event struct {
	Seq       int           `json:"seq"`
	EventType string        `json:"event_type"` // "created" | "updated" | "removed" | "triggered"
	Probe     ProbeSnapshot `json:"probe"`
	// RunID is only set for a "triggered" event -- the server-issued id
	// every node executing this one trigger reports results under, so
	// they correlate into a single run across nodes instead of each
	// getting its own independent one (see agent's newRunID, which is
	// what a normal scheduled run still mints for itself).
	RunID string `json:"run_id,omitempty"`
}

// Result is probe.Result plus the correlation fields needed to route
// it back to the right probe/account server-side. RunID is minted by
// the node itself (see internal/agent's scheduler), not issued by
// the server -- there is no dispatch step to issue one from anymore.
type Result struct {
	RunID   string `json:"run_id"`
	ProbeID string `json:"probe_id"`
	probe.Result
	ObservedAt string `json:"observed_at"`
}

// ResultsRequest is the body of POST /v1/nodes/results.
type ResultsRequest struct {
	SpecVersion int      `json:"spec_version"`
	NodeID      string   `json:"node_id"`
	BatchID     string   `json:"batch_id"`
	SentAt      string   `json:"sent_at"`
	Results     []Result `json:"results"`
}

// ResultAck is one result's acceptance outcome. Reason is a closed,
// small enum (e.g. "account_inactive", "probe_inactive", "duplicate",
// "unknown_probe") the node only logs -- it carries no billing meaning
// on the node side.
type ResultAck struct {
	RunID    string `json:"run_id"`
	Seq      int    `json:"seq"`
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
}

// ResultsResponse is the body returned by POST /v1/nodes/results.
type ResultsResponse struct {
	SpecVersion int         `json:"spec_version"`
	Accepted    int         `json:"accepted"`
	Rejected    int         `json:"rejected"`
	Results     []ResultAck `json:"results"`
	NodeStatus  string      `json:"node_status"`
}

// HeartbeatRequest is the body of POST /v1/nodes/heartbeat. Probers
// is a compact inventory, one "prober_id:file_hash" entry per loaded
// module -- mirrors the "node_id:secret" token convention elsewhere
// in this protocol. file_hash is the sha256 of that module's raw
// YAML source; the server is content-addressed on this hash (see
// ModuleUpload below), so two nodes can report the same prober_id
// with two different hashes (each running a different version of
// "tcp", say) without conflict. There is deliberately no
// kind/engine/engine_version here anymore -- that metadata now lives
// server-side, attached to the hash itself, populated once via
// POST /v1/nodes/modules rather than repeated on every heartbeat.
type HeartbeatRequest struct {
	SpecVersion  int    `json:"spec_version"`
	NodeID       string `json:"node_id"`
	AgentVersion string `json:"agent_version"`
	// OS/Arch are this process's own runtime.GOOS/GOARCH -- how
	// radar-api knows whether a given bundled module (not every
	// engine has builds for every platform, e.g. openvpn/wireguard-go
	// are linux-only) can even be offered to this specific node at
	// all, not just whether one's already installed.
	OS      string   `json:"os,omitempty"`
	Arch    string   `json:"arch,omitempty"`
	Probers []string `json:"probers"`
	// Modules is every loaded module's own Version/URL (see
	// module.Module's own doc comment), keyed by prober_id -- entirely
	// separate from Probers' content-addressed prober_id:file_hash
	// pairs above, which exist for the module-sync handshake, not
	// human-readable version tracking. A module authored without
	// either (the embedded tcp/udp/dns/... defaults, or an
	// unmigrated custom module) is simply absent from this map, not
	// included with null fields.
	Modules map[string]ModuleVersion `json:"modules,omitempty"`
	// ProbeHash is this agent's own last-known content hash for its
	// compiled probe assignment (see probeCache.contentHash) -- absent
	// (empty string, omitted on the wire) on this node's very first-
	// ever heartbeat, since there's nothing cached yet. An absent/
	// empty hash naturally compares unequal to whatever the server has
	// compiled and triggers a full snapshot in response, the same "no
	// separate bootstrap path needed" shape SinceSeq=0 used to give.
	ProbeHash string `json:"probe_hash,omitempty"`
	SentAt    string `json:"sent_at"`
}

// ModuleVersion is one loaded module's own version/manifest-url, as
// reported per prober_id in HeartbeatRequest.Modules. Version/URL are
// both nil (not omitted) when a module was authored without them --
// "we checked, there is no version" is a real, reportable state,
// distinct from "this module isn't in the map at all".
type ModuleVersion struct {
	Version *string `json:"version"`
	URL     *string `json:"url"`
}

// HeartbeatResponse is the body returned by a successful
// POST /v1/nodes/heartbeat.
type HeartbeatResponse struct {
	SpecVersion           int    `json:"spec_version"`
	NodeStatus            string `json:"node_status"`
	HeartbeatIntervalSecs int    `json:"heartbeat_interval_seconds"`
	ServerTime            string `json:"server_time"`
	// ProbeHash/Probes are only present when this agent's own reported
	// ProbeHash (in the request) didn't match the server's current
	// compiled state -- absent entirely (not empty) whenever they
	// already agreed, meaning "you're caught up, nothing to change".
	// Probes, when present, is always this node's FULL current
	// assignment, never a delta -- apply it as a replace (see
	// probeCache.replaceAssignment), not a merge, though lastRunAt
	// still has to be carried forward per probe ID across that
	// replace, same as it always has been.
	ProbeHash string          `json:"probe_hash,omitempty"`
	Probes    []ProbeSnapshot `json:"probes,omitempty"`
	// Only ever "triggered" entries as of SpecVersion 2 -- unaffected
	// by ProbeHash/Probes above, still delivered inline on every
	// heartbeat regardless of hash match.
	Events []Event `json:"events,omitempty"`
	// Owner-triggered from the radar UI (see radar-api's
	// nodes.pending_command), delivered here rather than a separate
	// channel because heartbeat is this node's only poll. Empty/absent
	// means no command. "delete": this node was removed from radar --
	// stop running and tell the operator how to fully uninstall. Not
	// fire-once: kept coming until the agent uninstalls for good or an
	// owner restores the node.
	Command string `json:"command,omitempty"`
	// PendingAction is an "update" or bundled-prober module-actions
	// batch, still awaiting this agent's acknowledgement (see
	// radar-api's nodes.pending_action and POST /v1/nodes/ack) --
	// absent once acked, since there's nothing left to redeliver. Call
	// AckAction with PendingAction.ID *before* acting on it (before
	// reinstall/selfUpdate, which are about to kill this process) --
	// that's what lets the server tell "delivered" apart from
	// "received and being acted on" instead of clearing this the
	// instant it's handed back once, which used to make the dashboard
	// prematurely show the update button again while this node was
	// still mid-restart.
	PendingAction *PendingAction `json:"pending_action,omitempty"`
}

// PendingAction is the payload named in HeartbeatResponse.PendingAction
// -- see its doc comment.
type PendingAction struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`              // "update" or "module_actions"
	Actions []string `json:"actions,omitempty"` // only for kind == "module_actions"
}

// AckRequest is the body of POST /v1/nodes/ack.
type AckRequest struct {
	ID string `json:"id"`
}

// HeartbeatRejection is the body of a 409 response to
// POST /v1/nodes/heartbeat when one or more reported prober_id:
// file_hash pairs aren't in the server's prober_files store yet.
// MissingProberIDs names exactly which modules (by prober_id, not
// hash) the node should push via POST /v1/nodes/modules before
// retrying -- not the node's whole inventory, only what's actually
// unknown or changed. node_status/heartbeat_interval_seconds ride
// along so the agent doesn't lose that state while resyncing.
type HeartbeatRejection struct {
	Error                 string   `json:"error"`
	MissingProberIDs      []string `json:"missing_prober_ids"`
	NodeStatus            string   `json:"node_status"`
	HeartbeatIntervalSecs int      `json:"heartbeat_interval_seconds"`
}

const HeartbeatErrorModulesOutOfSync = "modules_out_of_sync"

// ModuleUpload is one module's full definition, pushed in response to
// a HeartbeatRejection. FileHash must match sha256(YAML) -- the
// server independently verifies this rather than trusting the claim.
// YAML is stored server-side purely as an opaque display string, never
// parsed there -- Manifest (plain JSON, no anchor/alias expansion
// mechanism the way a YAML parser has) is what radar-api actually
// parses to extract this module's request/response schema.
type ModuleUpload struct {
	ProberID string          `json:"prober_id"`
	FileHash string          `json:"file_hash"`
	YAML     string          `json:"yaml"`
	Manifest module.Manifest `json:"manifest"`
}

// ModulesUploadRequest is the body of POST /v1/nodes/modules.
type ModulesUploadRequest struct {
	SpecVersion int            `json:"spec_version"`
	NodeID      string         `json:"node_id"`
	Modules     []ModuleUpload `json:"modules"`
}

// ModulesUploadResponse is the body returned by POST /v1/nodes/modules.
type ModulesUploadResponse struct {
	SpecVersion int `json:"spec_version"`
	Stored      int `json:"stored"`
}

const (
	NodeStatusActive          = "active"
	NodeStatusSuspended       = "suspended"
	NodeStatusDeactivated     = "deactivated"
	NodeStatusInactiveBilling = "inactive_billing"
)

const (
	ProbeStatusActive          = "active"
	ProbeStatusPaused          = "paused"
	ProbeStatusArchived        = "archived"
	ProbeStatusInactiveBilling = "inactive_billing"
)
