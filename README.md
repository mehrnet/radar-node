# radar-node

`radar-node` is the agent binary for [radar](https://radar.mehrnet.com)
(mehrnet's network-monitoring service): a single Go binary that either runs a
one-shot probe from the CLI, or runs as a long-lived agent that syncs job
definitions from `radar-api`, executes them on schedule, and reports results
back.

Every prober -- including the six built-ins (`tcp`, `udp`, `dns`, `icmp`,
`http`/`https`, `system`) -- is a config file, not hardcoded Go. There is no
"native vs. custom module" distinction: a module either calls a built-in Go
implementation in-process (`action:`, zero subprocess overhead) or shells out
to an external binary (`run:`, e.g. `xray`/`sing-box`). The six built-ins are
embedded in the binary and load automatically; `radar-node init` writes
them out as real, editable files. In this sense radar-node isn't
fundamentally a network prober -- it's a generic scheduled data-transform
runner (request in, structured response out, on a schedule, billed and
stored) that ships with networking as its first set of fixtures.

## Build

```sh
make build      # -> ./radar-node
make test
make lint
make cross       # sanity build for linux/amd64 + linux/arm64
make install      # go install into $GOBIN
```

Requires Go 1.26+. Release builds (tagged) are handled by `.goreleaser.yaml`.

## CLI usage

```sh
radar-node probe <target> [flags]
radar-node agent [flags]
radar-node init [-C path]
```

### `probe` -- one-shot check runner

```sh
radar-node probe 1.1.1.1:443 --type tcp --param tls=true
radar-node probe https://example.com --type http --count 3 --format table
radar-node probe 8.8.8.8 --type icmp --count 5
radar-node probe self --type system
```

| Flag | Meaning |
|---|---|
| `--type` | `tcp` \| `udp` \| `dns` \| `icmp` \| `http` \| `system` \| any module name (default `tcp`) |
| `--probe` | `warm` \| `hard` (default `warm`) |
| `--count` | number of probes to run (default `1`) |
| `--timeout` | per-probe timeout (default `5s`) |
| `--format` | `json` \| `csv` \| `table` (default `json`) |
| `--param k=v` | module-specific parameter, repeatable (`tcp`: `tls`,`sni`,`insecure` -- `dns`: `record`,`server` -- `http`: `method`) |
| `--modules-dir` | load/override modules from `*.yaml`/`*.yml` here, on top of the embedded defaults |

### `agent` -- long-lived worker

```sh
radar-node agent --api-url https://radar-api.mehrnet.com --api-key node_01J...:s3cr3t
```

| Flag | Meaning |
|---|---|
| `--api-url` | `radar-api` base URL (required) |
| `--api-key` | `"node_id:secret"` bearer token (required) |
| `--api-proxy` | proxy for the agent's own `radar-api` traffic (`http://`, `https://`, `socks5://`, `socks5h://`) |
| `--events-interval` | how often to sync job definitions (default `30s`) |
| `--scheduler-tick` | how often to check cached jobs for due-ness (default `2s`) |
| `--concurrency` | max probes running at once (default `64`) |
| `--modules-dir` | load/override modules from `*.yaml`/`*.yml` here, on top of the embedded defaults |

The agent has no server-computed dispatch: it syncs job definitions
incrementally into a local cache and decides for itself when something is
due. See [Wire protocol](#wire-protocol--radar-api) below for the full
mechanics.

### `init` -- scaffold editable module files

```sh
radar-node init -C /etc/radar-node/modules.d
```

Writes the six embedded default module files (`tcp.yaml`, `udp.yaml`,
`dns.yaml`, `icmp.yaml`, `http.yaml`, `https.yaml`, `system.yaml`) to `-C`
(default `.`) as real files, so there's something to actually edit -- until
`init` is run, or a directory is pointed at with `--modules-dir`, they only
exist embedded inside the binary. `--force` overwrites files that already
exist there.

## Modules

A module is a YAML file describing one prober: what data form it expects as
a request, what it returns as a response, and how it does the work. Exactly
one of two execution modes is set per module:

- **`action:`** -- calls a built-in Go implementation directly, in-process,
  no subprocess. This is how the six defaults work (`tcp` -> `tcp_connect`,
  `system` -> `system_stats`, ...). Action names are an internal
  implementation library, decoupled from prober identity: any number of
  differently-configured modules can reference the same action (e.g. a
  `tcp-strict.yaml` with a tighter request schema, still `action:
  tcp_connect`).
- **`run:`** (+ optional `prepare:`/`teardown:`) -- the subprocess
  lifecycle for anything that needs a real external binary and doesn't
  fit the two generic proxy actions below. See `examples/modules` for
  working `xray-vless`/`singbox-trojan` reference implementations of
  this path (still valid for a genuinely bespoke external-tool
  integration, just superseded for xray/sing-box specifically by the
  generic actions below).

```yaml
name: tcp
action: tcp_connect
request:
  - name: tls
    type: bool
    required: false
  - name: sni
    type: string
    required: false
response:
  - name: tls_version
    type: string
```

`request`/`response` declare the module's data form -- a small, bespoke
shape (`name`, `type: string|number|bool|object|array`, `required`,
default `false`), not full JSON Schema. `object`/`array` exist for params
that are inherently structured (a full xray/sing-box config, say) rather
than scalar; unlike the scalar types, they get no string-coercion leniency
-- there's no sensible "string that looks like an object." `request` is
enforced before every run, for every execution mode: a job whose params
don't match (missing required field, wrong type) never reaches the real
probe/action at all. It comes back as a normal failed result with
`"error_code": "invalid_params"` (distinct from the free-text `error`) so a
UI can show "this is misconfigured" instead of "the target is down" -- see
[Result](#result) below. `response` is declarative/documentation only,
validated for well-formedness at load time but not checked against actual
output, since a check's own failure modes (partial data, a tool's varying
output shape on error) shouldn't be conflated with a request-validation
rejection.

### Generic xray/sing-box proxying

`xray_proxy_test`/`singbox_proxy_test` are native actions (see
`internal/checks/proxytest`) that take a full engine config plus which
port in it is the local test entry point, and are entirely
protocol-agnostic -- neither reads anything protocol-specific out of the
config, so any protocol either engine supports (vless, vmess, trojan,
shadowsocks, hysteria2, ...) works with zero radar-node code changes.
Building the config from a share link (`vless://...`, `vmess://...`, ...)
is a client-side/UI concern, the same way 3x-ui converts share links to
JSON in the browser -- radar-node only ever receives the finished config.

```yaml
name: xray
action: xray_proxy_test
request:
  - name: config
    type: object
    required: true
  - name: socks_port
    type: number
    required: true
```

`socks_port` is **never bound as-is**. A node's environment isn't fully
known in advance -- another process could already hold that port, or two
probes could legitimately want to test through "the same" port
concurrently -- so the node always silently reallocates the matching
inbound (whichever one's `port`/`listen_port` equals `socks_port`) to a
real free local port before launching the engine, and reports results
against whichever port it actually used. `socks_port` only ever serves to
identify *which* inbound to remap; the account/job never sees or needs to
know remapping happened. A `socks_port` that doesn't match any inbound in
`config` comes back as `error_code: "invalid_params"`, same as any other
request-schema mismatch.

For `run:`-based modules, the following placeholders resolve in every
`prepare`/`run`/`teardown` command, sourced only from the fixed set below
or a `param.<name>` reference -- an unrecognized placeholder fails config
loading (and therefore process start), not a running check, and a job can
never introduce a new one:

| Placeholder | Source |
|---|---|
| `{{target}}` | the job's `target` |
| `{{timeout_ms}}` | the job's `timeout_ms` |
| `{{param.<name>}}` | the job's `params.<name>` (string values only) |
| `{{params_json}}` | path to a temp file containing the full `params` object |
| `{{alloc_port}}` | a locally-allocated free TCP port, for a `prepare` step that starts a local proxy inbound |

Trust boundary: module *definitions* (which action or command a name maps
to) come only from local YAML files, read once at process start -- a
remote job can only *invoke* an already-loaded module by name with typed
parameters, never introduce a new command, placeholder, or action.

Each loaded module's raw YAML source and its `file_hash` (`sha256` of that
source, hex-encoded) are what a heartbeat reports and what
`POST /v1/nodes/modules` uploads on demand -- see
[`POST /v1/nodes/heartbeat`](#post-v1nodesheartbeat) below. This is how
`radar-api` learns each node's request/response schema well enough to
drive a job-creation form dynamically, without radar-node ever pushing
full module bodies on every heartbeat.

## Wire protocol -- radar-node <-> radar-api

This is the contract between a `radar-node agent` process and `radar-api`.
Both sides build against it; changing a shape here is a breaking change and
must bump `spec_version`.

Nothing in this protocol carries billing information. A node knows exactly
two things about its own standing: whether the API accepted its results,
and whether the API still wants it to keep working. Balances, prices, and
plans are entirely `radar-api`'s concern.

There is no server-computed dispatch. A node pulls an incremental,
seq-ordered log of job-definition changes and keeps its own local cache of
what to run and when; the server never computes per-tick "what's due"
itself. This makes the API side stateless per request (append an event on
write, hand back everything after a cursor on read) and pushes all
scheduling compute to the node, where it's cheap and doesn't compete for D1
write capacity.

### Conventions

- **IDs** are ULIDs (lexicographically sortable, timestamp-prefixed),
  rendered as strings with a type prefix for readability: `job_01J...`,
  `node_01J...`, `batch_01J...`, `acct_01J...`. A `run_id` is the exception
  -- see [Result](#result).
- **Timestamps** are RFC 3339, UTC, millisecond precision:
  `"2026-07-12T10:00:03.123Z"`. `starts_at`/`ends_at` on a `JobSnapshot`
  are Unix milliseconds instead, since they're compared against the node's
  own clock-corrected `now()` in a tight scheduling loop -- see
  [Clock sync](#clock-sync).
- **Durations** are always explicit about their unit in the field name
  (`timeout_ms`, `interval_seconds`) -- never a bare number. This mirrors
  `probe.Options`/`probe.Result`'s own convention (`latency_ms`, not
  `latency`).
- **Optional/absent fields are omitted**, not sent as `null` -- matches
  the `omitempty` json tags already on `probe.Result`. A field's absence
  and its zero value are always distinguishable this way (e.g. a missing
  `error` key means no error).
- Every top-level request/response body carries `"spec_version": 1`. A
  node or API build that doesn't understand a future version rejects the
  call with `spec_version_unsupported` rather than guessing.
- All node-facing endpoints require the node bearer token
  (`Authorization: Bearer <node_id>:<node_secret>`), issued once at node
  registration -- no JWT, no request signing.
- `seq` (on both `Event` and `Result`) is a plain, not-necessarily-
  contiguous monotonic cursor, never reused or reordered. A node never
  interprets gaps -- it only ever asks for "everything after the highest
  seq I've already applied."

### Entities

#### Job

Created by a user through `radar-api` (never by a node). Describes *what*
to check, *how often*, and *which nodes* should do it. A node never sees
this shape directly -- what it receives is a [JobSnapshot](#jobsnapshot), a
flattened, self-contained view of the same job.

```jsonc
{
  "job_id": "job_01J8Z3K7QK6H1S8YB6WQXQABCD",
  "account_id": "acct_01J8Z2Q1N9R4F0T3X8Y6Z1WXYZ",
  "target": "1.2.3.4:443",
  "prober": "tcp",                 // native check name or a custom module name
  "mode": "warm",                  // "warm" | "hard" -- ignored by probers that don't use it
  "probe_count": 5,
  "timeout_ms": 5000,
  "schedule": {
    "type": "interval",             // "once" | "interval"
    "interval_seconds": 3600,       // required when type == "interval"
    "starts_at": "2026-07-12T10:00:00.000Z",
    "ends_at": null                 // null = runs indefinitely (until paused/archived)
  },
  "nodes": [
    "node_01J8Z1A2B3C4D5E6F7G8H9J0KL",
    "node_01J8Z1A2B3C4D5E6F7G8H9J0MN"
  ],
  "params": {
    "sni": "example.com",
    "insecure": "true"
  },
  "status": "active",               // "active" | "paused" | "archived" | "inactive_billing"
  "created_at": "2026-07-10T08:00:00.000Z"
}
```

`nodes` is always an explicit list of node IDs chosen by the account --
there is no pool/tag-based auto-selection. Every node has its own
admin-set price, so a user is always knowingly picking which specific
nodes a job runs on, not delegating that choice to the platform.

`params` is a flat JSON object of strings for the common case. For a
custom prober that needs structured input beyond flat key/value pairs,
`params` may contain nested JSON of any shape; native checks ignore keys
they don't recognize, and a custom module's command template can reference
`{{params_json}}` to receive the *entire* params object as a temp file.

`status: "inactive_billing"` is a billing-driven cascade, distinct from
user-set `"paused"`/`"archived"`: when an account's balance runs out,
every job it owns (and, for BYO nodes, every node it owns) flips to
`inactive_billing` automatically, and flips back to `active` on top-up. A
node treats it exactly like `"paused"` -- not due, ever -- and never needs
to know *why*.

#### JobSnapshot

The full current definition of a job, as carried by every
[Event](#event). Flattened and self-contained -- a node applies this
directly to its local cache with no follow-up lookup of any kind.

```jsonc
{
  "id": "job_01J8Z3K7QK6H1S8YB6WQXQABCD",
  "target": "1.2.3.4:443",
  "prober": "tcp",
  "mode": "warm",
  "probe_count": 5,
  "timeout_ms": 5000,
  "schedule_type": "interval",        // "once" | "interval"
  "interval_seconds": 3600,           // present when schedule_type == "interval"
  "starts_at": 1783929600000,         // unix ms
  "ends_at": 1783933200000,           // unix ms, omitted = runs indefinitely
  "params": { "sni": "example.com", "insecure": "true" },
  "status": "active"                  // "active" | "paused" | "archived" | "inactive_billing"
}
```

#### Event

One entry from the job-definition change log a node syncs incrementally
via `GET /v1/nodes/events`.

```jsonc
{
  "seq": 42,
  "event_type": "updated",          // "created" | "updated" | "removed"
  "job": { /* JobSnapshot, see above */ }
}
```

- `created` -- a job now applies to this node. The node adds it to its
  local cache.
- `updated` -- any field of a job this node already has changed. The node
  replaces its cached copy wholesale with the new snapshot, but must
  never reset its own bookkeeping of when it last ran the job -- only a
  fresh `starts_at`/interval computation against that preserved history
  decides due-ness going forward.
- `removed` -- the job no longer applies to this node. The node drops it
  from its cache entirely.

A node that has never synced (or is resyncing from scratch) requests from
`since_seq=0` and applies every event in order; the log is a complete
history, so replaying it from zero always converges on the correct
current state.

#### Result

One probe's outcome. This is `probe.Result` (the Go type already
implemented) plus the correlation fields needed to route it back to the
right job/account server-side.

`run_id` is minted by the node itself (a ULID, generated fresh whenever a
cached job is found due) -- there is no dispatch step for the server to
issue one from anymore.

```jsonc
{
  "run_id": "run_01J8Z4M5N6P7Q8R9S0T1U2V3WX",
  "job_id": "job_01J8Z3K7QK6H1S8YB6WQXQABCD",
  "seq": 1,
  "ok": true,
  "type": "tcp",
  "target": "1.2.3.4:443",
  "mode": "warm",
  "latency_ms": 12.4,
  "extra": { "tls_version": "1.3" },
  "observed_at": "2026-07-12T10:00:05.001Z"
}
```

A failed probe omits `latency_ms` and `extra` and includes `error`
instead:

```jsonc
{
  "run_id": "run_01J8Z4M5N6P7Q8R9S0T1U2V3WX",
  "job_id": "job_01J8Z3K7QK6H1S8YB6WQXQABCD",
  "seq": 2,
  "ok": false,
  "type": "tcp",
  "target": "1.2.3.4:443",
  "mode": "warm",
  "error": "dial tcp 1.2.3.4:443: connect: connection refused",
  "observed_at": "2026-07-12T10:00:05.812Z"
}
```

A failure caused by a job's params not matching its module's declared
`request` schema (see [Modules](#modules)) additionally carries
`error_code`, distinguishing "this job is misconfigured" from a real probe
attempt that failed -- no probe/action was ever attempted in this case:

```jsonc
{
  "run_id": "run_01J8Z4M5N6P7Q8R9S0T1U2V3WX",
  "job_id": "job_01J8Z3K7QK6H1S8YB6WQXQABCD",
  "seq": 1,
  "ok": false,
  "type": "xray-vless",
  "target": "1.2.3.4:6300",
  "mode": "warm",
  "error": "missing required param \"uuid\"",
  "error_code": "invalid_params",
  "observed_at": "2026-07-12T10:00:05.812Z"
}
```

### Clock sync

A node's local scheduling decisions must agree with the server's notion of
time, not the node's own possibly-drifted wall clock. Every
`GET /v1/nodes/events` response includes `server_time`; the node measures
its own request/response round trip around that call and derives an
offset using a standard NTP-style midpoint correction:

```
midpoint  = sent_at + (received_at - sent_at) / 2
offset    = server_time - midpoint
node_now  = time.Now() + offset
```

All due-ness comparisons (`starts_at`, `ends_at`, last-run +
`interval_seconds`) use `node_now()`, never a direct `time.Now()`. If the
server's clock reads an hour behind this node's, the node's schedule
should behave exactly as if its own clock also read an hour behind --
there is no negotiation, the server's clock always wins. The offset is
refreshed on every events-sync call, so it self-corrects if server or
node clock drifts over a long-running process.

### Local scheduling

There is no `window_seconds` fetch parameter and no server-side "due"
computation. Instead, a node runs three independent loops:

1. **Events sync** -- periodically (and once eagerly at startup) calls
   `GET /v1/nodes/events?since_seq=<cursor>`, applies every event to its
   local job cache in order, advances its cursor, and updates its clock
   offset from `server_time`.
2. **Scheduler tick** -- on a short, fixed local interval, scans the
   cached jobs and executes whichever are due per `node_now()`. A job is
   marked as run (its last-run timestamp updated) *before* the probes
   actually execute, not after -- this trades a narrow, benign failure
   mode ("a slow probe run causes the next tick to skip an occurrence it
   technically could have started") for a much worse one it avoids
   entirely ("a job with a short interval or a `once` schedule fires
   twice because two ticks raced to claim it while it was still
   running").
3. **Heartbeat** -- unchanged in shape from the previous protocol
   version, see below.

A job that's never resulted because a node crashed or lost network
mid-run is not explicitly retried or reconciled by the server -- for
`interval` jobs, the next due tick after the node recovers naturally
produces a fresh run; for `once` jobs, the node's local "already ran
this" bookkeeping is only in memory, so a crash before completion means
it's picked up again once the process restarts and resyncs.

### Endpoints

#### `POST /v1/nodes/register`

One-time. Used at node provisioning (root-operated boxes and
account-added BYO nodes alike) to mint credentials. Called with an
account bearer token (not a node token, since the node doesn't exist
yet), typically by whatever provisioning flow/UI adds the node.

Request:
```jsonc
{ "spec_version": 1, "name": "eu-west-3" }
```

Response:
```jsonc
{
  "spec_version": 1,
  "node_id": "node_01J8Z1A2B3C4D5E6F7G8H9J0KL",
  "node_secret": "9f2c...redacted...a41d"   // shown once, never retrievable again
}
```

#### `GET /v1/nodes/events?since_seq=0`

Polled by the node on its own cadence (see
[Local scheduling](#local-scheduling)). Returns every job-definition
change for this node with `seq > since_seq`, oldest first, capped at 1000
events per call -- a node with a large backlog simply calls again with
the new cursor until it catches up. Node auth identifies which node is
asking; no account context needed in the request.

Response:
```jsonc
{
  "spec_version": 1,
  "server_time": "2026-07-12T10:00:03.123Z",
  "events": [
    { "seq": 41, "event_type": "created", "job": { /* JobSnapshot */ } },
    { "seq": 42, "event_type": "updated", "job": { /* JobSnapshot */ } }
  ]
}
```

An empty `events` array is a normal, common response -- it just means
nothing changed since `since_seq`. The node still uses `server_time` from
every response (empty or not) to refresh its clock offset.

#### `POST /v1/nodes/results`

Batched, one call per scheduler-tick's worth of completed runs rather
than one call per probe.

Request:
```jsonc
{
  "spec_version": 1,
  "node_id": "node_01J8Z1A2B3C4D5E6F7G8H9J0KL",
  "batch_id": "batch_01J8Z5X6Y7Z8A9B0C1D2E3FGH",
  "sent_at": "2026-07-12T10:00:08.900Z",
  "results": [ /* Result objects, see above */ ]
}
```

`batch_id` is the idempotency key for the whole POST; the API also
dedupes on `(node_id, run_id, seq)` underneath in case a node retries a
partially-acknowledged batch after a network error. Since billing charges
on response, a duplicate delivery of the same result must never be
charged twice.

Response -- per-result acceptance, since a batch can straddle a balance
running out or a job/account going `inactive_billing` mid-way:
```jsonc
{
  "spec_version": 1,
  "accepted": 4,
  "rejected": 1,
  "results": [
    { "run_id": "run_01J...", "seq": 1, "accepted": true },
    { "run_id": "run_01J...", "seq": 2, "accepted": false, "reason": "account_inactive" }
  ],
  "node_status": "active"
}
```

`reason` is a small closed enum (`unknown_job`, `job_inactive`,
`account_inactive`, `duplicate`) -- a node doesn't interpret these beyond
logging them; it has no billing model of its own. `job_inactive`/
`account_inactive` should be rare in practice: the node's own cache
already stops scheduling a job the moment its `status` flips via the
events log, so these mostly cover the narrow race where a run was
already in flight when the status changed.

#### `POST /v1/nodes/heartbeat`

Periodic (interval suggested by the API, see response), also the
content-addressed module sync handshake. `probers` is a compact
inventory, one `"prober_id:file_hash"` entry per loaded module --
mirrors the `"node_id:secret"` token convention elsewhere in this
protocol. There is no `kind`/`engine`/`engine_version` here anymore;
that metadata is attached to the hash itself server-side (see
`POST /v1/nodes/modules`), populated once rather than repeated on
every heartbeat.

Request:
```jsonc
{
  "spec_version": 1,
  "node_id": "node_01J8Z1A2B3C4D5E6F7G8H9J0KL",
  "agent_version": "0.3.1",
  "probers": [
    "tcp:6985e90a888a115f28bbc83ae985f30a399e9ca9ab162a3af734bbd1ac2e64f",
    "xray-vless:a1b2c3d4e5f6..."
  ],
  "sent_at": "2026-07-12T10:00:00.000Z"
}
```

Response (200 -- every reported hash is already known):
```jsonc
{
  "spec_version": 1,
  "node_status": "active",           // "active" | "suspended" | "deactivated" | "inactive_billing"
  "heartbeat_interval_seconds": 30
}
```

Response (409 -- one or more hashes aren't recognized yet):
```jsonc
{
  "error": "modules_out_of_sync",
  "missing_prober_ids": ["xray-vless"],
  "node_status": "active",
  "heartbeat_interval_seconds": 30
}
```

`missing_prober_ids` names exactly which modules (by `prober_id`, not
hash) to push via `POST /v1/nodes/modules` -- never this node's whole
inventory, only what's actually new or changed since the last
successful heartbeat. `node_status`/`heartbeat_interval_seconds` ride
along on the rejection too, so the agent doesn't lose that state while
resyncing. The agent's own loop for this: send heartbeat -> on 409,
upload exactly `missing_prober_ids` -> retry the same heartbeat once,
which should now succeed. See [Modules](#modules) for how a module's
`file_hash` (`sha256` of its raw YAML source, hex-encoded) is computed.

A node reads `node_status` after *every* call that returns it (both this
endpoint and the results endpoint), not just the heartbeat --
`deactivated`/`suspended`/`inactive_billing` all mean stop scheduling new
runs (existing cached jobs are left in place, simply not acted on) until
a future heartbeat says `active` again. This, plus `--api-proxy` and
reading `node_status`/per-result `reason`, is the entire extent of what a
node needs to understand about its own standing -- everything else (why
it's inactive, what plan the account is on, what the balance is) is
`radar-api`/dashboard-only information a node never sees.

#### `POST /v1/nodes/modules`

Uploads the full definition of one or more modules named in a
heartbeat's `missing_prober_ids`. `file_hash` must equal
`sha256(yaml)`; the server independently verifies this rather than
trusting the claim.

Request:
```jsonc
{
  "spec_version": 1,
  "node_id": "node_01J8Z1A2B3C4D5E6F7G8H9J0KL",
  "modules": [
    {
      "prober_id": "xray-vless",
      "file_hash": "a1b2c3d4e5f6...",
      "yaml": "name: xray-vless\nengine: xray\n..."
    }
  ]
}
```

Response:
```jsonc
{ "spec_version": 1, "stored": 1 }
```

`stored` counts only genuinely new content -- radar-api is
content-addressed on `file_hash` server-side, so uploading a hash it
has already seen (from this node or any other) is a no-op parse-wise;
the upload still updates which hash *this* node currently points at
for that `prober_id`.
