// Package wire defines the JSON shapes exchanged between radar-node
// and radar-api, per README.md. Changing a field here is a
// breaking change to that spec and must be reflected in SpecVersion.
//
// There is no more server-computed dispatch. A node syncs job
// definitions incrementally (POST /v1/nodes/heartbeat's since_seq/
// events) into its own local cache and decides for itself when
// something is due -- see internal/agent's scheduler. Results are
// keyed by a node-generated RunID instead of a server-issued
// assignment id.
package wire

import (
	"github.com/mehrnet/radar-node/internal/module"
	"github.com/mehrnet/radar-node/internal/probe"
)

const SpecVersion = 1

// JobSnapshot is the full current definition of a job, as carried by
// every Event -- a node applies this directly to its local cache
// with no follow-up lookup of any kind.
type JobSnapshot struct {
	ID              string         `json:"id"`
	Target          string         `json:"target"`
	Prober          string         `json:"prober"`
	Mode            string         `json:"mode,omitempty"`
	ProbeCount      int            `json:"probe_count"`
	TimeoutMs       int            `json:"timeout_ms"`
	ScheduleType    string         `json:"schedule_type"` // "once" | "interval"
	IntervalSeconds int            `json:"interval_seconds,omitempty"`
	StartsAt        int64          `json:"starts_at"`
	EndsAt          int64          `json:"ends_at,omitempty"`
	Params          map[string]any `json:"params,omitempty"`
	Status          string         `json:"status"` // "active" | "paused" | "archived" | "inactive_billing"
}

// Event is one entry from the job-definition change log a node syncs
// incrementally via POST /v1/nodes/heartbeat's since_seq/events
// fields. Seq is a plain, not-necessarily-contiguous cursor: a node's
// next heartbeat always sends since_seq = the highest one it has
// already applied.
type Event struct {
	Seq       int         `json:"seq"`
	EventType string      `json:"event_type"` // "created" | "updated" | "removed"
	Job       JobSnapshot `json:"job"`
}

// Result is probe.Result plus the correlation fields needed to route
// it back to the right job/account server-side. RunID is minted by
// the node itself (see internal/agent's scheduler), not issued by
// the server -- there is no dispatch step to issue one from anymore.
type Result struct {
	RunID string `json:"run_id"`
	JobID string `json:"job_id"`
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
// small enum (e.g. "account_inactive", "job_inactive", "duplicate",
// "unknown_job") the node only logs -- it carries no billing meaning
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
//
// SinceSeq folds what used to be a separate GET /v1/nodes/events poll
// into the heartbeat itself -- both fired on their own fixed timer
// regardless of activity, each paying its own request/auth overhead
// for (almost always) zero new information. 0 means a full resync,
// same meaning the old standalone endpoint gave a fresh cache.
type HeartbeatRequest struct {
	SpecVersion  int      `json:"spec_version"`
	NodeID       string   `json:"node_id"`
	AgentVersion string   `json:"agent_version"`
	Probers      []string `json:"probers"`
	SinceSeq     int      `json:"since_seq"`
	SentAt       string   `json:"sent_at"`
}

// HeartbeatResponse is the body returned by a successful
// POST /v1/nodes/heartbeat. ServerTime/Events are what used to be
// GET /v1/nodes/events's whole response -- see SinceSeq above.
type HeartbeatResponse struct {
	SpecVersion           int     `json:"spec_version"`
	NodeStatus            string  `json:"node_status"`
	HeartbeatIntervalSecs int     `json:"heartbeat_interval_seconds"`
	ServerTime            string  `json:"server_time"`
	Events                []Event `json:"events,omitempty"`
	// Owner-triggered from the radar UI (see radar-api's
	// nodes.pending_command), delivered here rather than a separate
	// channel because heartbeat is this node's only poll. Empty/absent
	// means no command. "update": re-run the install script to fetch
	// the latest release and replace this process. "delete": this
	// node was removed from radar -- stop running and tell the
	// operator how to fully uninstall. Sent at most once per command
	// (the server clears it after handing it back), so a request that
	// never reaches a running agent (offline, crashed) is simply
	// retried the next time it does heartbeat, no worse than any other
	// heartbeat-carried state.
	Command string `json:"command,omitempty"`
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
	JobStatusActive          = "active"
	JobStatusPaused          = "paused"
	JobStatusArchived        = "archived"
	JobStatusInactiveBilling = "inactive_billing"
)
