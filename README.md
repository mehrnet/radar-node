# radar-node

`radar-node` is the agent binary for [radar](https://radar.mehrnet.com)
(mehrnet's network-monitoring service): a single Go binary that either runs a
one-shot probe from the CLI, or runs as a long-lived agent that syncs probe
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

## Install (Linux/macOS)

Register a node in the [radar UI](https://radar.mehrnet.com) first -- it
gives you a one-time `node_id` and `api_key`. Then, on the machine that
should run the node:

```sh
curl -fsSL https://radar.mehrnet.com/install/node.sh \
  | sh -s -- --node_id=<node_id> --api_key=<api_key>
```

This downloads the right release asset for your OS/arch, verifies its
checksum, and installs `radar-node` as a persistent service -- systemd on
Linux, launchd on macOS -- so it survives reboots with no further steps. Run
as root for a system-wide service, or as a regular user for a user-scoped
one (see `--help` below).

Behind a proxy? Add `--proxy=<url>` (`http://`, `https://`, `socks5://`, or
`socks5h://`) -- it's used both for the installer's own downloads and for
the running agent's ongoing `radar-api` traffic.

```
Usage: install.sh --node_id=<id> --api_key=<secret> [options]

Required (shown once when you register a node in the radar UI):
  --node_id=ID       the node id from registration
  --api_key=SECRET   the node secret from registration

Options:
  --api_url=URL      radar-api base URL (default: https://radar-api.mehrnet.com)
  --proxy=URL        proxy for both this installer's downloads and the running
                     agent's radar-api traffic (http://, https://, socks5://, socks5h://)
  --uninstall        stop and fully remove radar-node from this machine (no
                      other flag is needed -- this ignores --node_id/--api_key)
  -h, --help         show this help
```

There's no `--version=` pin -- the installer (and everything it downloads:
this binary, and the bundled xray/wireguard-go/openvpn engine modules
below) is mirrored at [radar.mehrnet.com](https://radar.mehrnet.com) as
only ever the single latest build of each, never a version history (see
that repo's own `releases-sync.sh`). The real installed version is read
back from the extracted binary itself (`radar-node version`) once it's on
disk, not guessed beforehand.

For Windows, grab a release asset manually from
[radar.mehrnet.com/releases/radar-node/](https://radar.mehrnet.com/releases/radar-node/).

### Remote update / delete

Deleting a node from the radar UI stops it (via its next heartbeat) but does
*not* remove it from the machine -- to fully clean up, run:

```sh
curl -fsSL https://radar.mehrnet.com/install/node.sh \
  | sh -s -- --uninstall
```

An "Update" button in the UI (shown when a newer release exists) re-runs the
install script on the node's own machine automatically -- no action needed
there. This agent acknowledges the update request before acting on it (see
[`POST /v1/nodes/ack`](#post-v1nodesack) below), so the dashboard can tell
"delivered" apart from "actually received and applying it" instead of the
update button reappearing the instant one heartbeat handed the request
back, with the node still mid-restart.

## Build

```sh
make build      # -> ./radar-node
make test
make lint       # gofmt -l check + go vet
make cross      # sanity build across the full release matrix: linux/darwin amd64+arm64, windows amd64+arm64
make install    # go install into $GOBIN
make clean      # rm ./radar-node
```

Requires Go 1.26+. Release builds (tagged) are handled by `.goreleaser.yaml`.

## CLI usage

```sh
radar-node probe <target> [flags]
radar-node agent [flags]
radar-node init [-C path]
radar-node fetch-module <url> [flags]
radar-node install-module <name> [flags]
radar-node remove-module <name> [flags]
radar-node version
```

### `probe` -- one-shot check runner

```sh
radar-node probe 1.1.1.1:443 --type tcp --param tls=true
radar-node probe https://example.com --type http --count 3 --format table
radar-node probe 8.8.8.8 --type icmp --count 5
radar-node probe self --type system
radar-node probe cloudflare.com --type dns --param record=mx
```

| Flag | Meaning |
|---|---|
| `--type` | `tcp` \| `udp` \| `dns` \| `icmp` \| `http` \| `system` \| any module name (default `tcp`) |
| `--count` | number of probes to run (default `1`) |
| `--timeout` | per-probe timeout (default `5s`) |
| `--format` | `json` \| `csv` \| `table` (default `json`) |
| `--param k=v` | module-specific parameter, repeatable (`tcp`: `tls`,`sni`,`insecure` -- `dns`: `record` (`a`\|`aaaa`\|`ns`\|`mx`\|`txt`\|`cname`\|`ptr`\|`srv`, default `a`), `server`, and (`srv` only) `service`,`proto` -- `http`: `method`) |
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
| `--scheduler-tick` | how often to check cached probes for due-ness (default `2s`) |
| `--concurrency` | max probes running at once (default `64`) |
| `--destination-interval` | min spacing between any two checks against the same real destination, node-wide, regardless of which probe/group/subscription/account asked for either -- overrides a probe's own `interval_seconds` when its destination is shared and busy (default `10s`, `0` disables) |
| `--destination-max-wait` | longest a single check will wait for its own destination to clear before giving up and failing that one check -- its own budget, separate from the check's own `timeout_ms`, so a heavily-shared destination (many probes resolving to one host) doesn't leave every check but the first permanently failing before it ever gets a real attempt (default `30s`) |
| `--modules-dir` | load/override modules from `*.yaml`/`*.yml` here, on top of the embedded defaults |

The agent has no server-computed dispatch: it syncs probe definitions
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

### `fetch-module` / `install-module` / `remove-module` -- Go-native module install

```sh
radar-node fetch-module https://radar.mehrnet.com/install/modules/xray.yaml
radar-node install-module xray   # re-fetch an already locally-known module, using its own recorded url
radar-node remove-module xray
```

| Flag | Meaning |
|---|---|
| `--modules-dir` | where a module's own YAML + file-kind dependencies go (default: `/etc/radar-node/modules.d` as root, `~/.config/radar-node/modules.d` otherwise) |
| `--tools-dir` | where a module's binary-kind dependencies go (default: `/etc/radar-node/tools` as root, `~/.config/radar-node/tools` otherwise) |
| `--proxy` | proxy for these downloads (`http://`, `https://`, `socks5://`, `socks5h://`) |

The Go-native counterpart to the shell-based `install.sh --install-module=
xray/wireguard/openvpn` flags described below -- same target directories,
same checksum verification, same module YAML schema,
different entry point. `fetch-module` downloads a module's own YAML from a
URL, checks this node's platform against its declared `os`/`arch` (if any),
downloads+verifies+installs every dependency in its `install:` list, then
writes the module YAML itself into `--modules-dir` -- what makes it
"locally known" for a later `install-module`/`remove-module` by name alone.
No separate state is kept beside that YAML: its own `url:` field is exactly
what `install-module <name>` re-fetches from, so re-running it later is how
to pick up an update. `remove-module <name>` deletes everything that
module's own `install:` list named plus the YAML itself.

A module's `install:` list is a flat set of remote artifacts, each with an
explicit source `url` and destination `path`:

```yaml
name: xray
os: [linux, darwin, windows]     # platforms this module can even be installed on; omit for "any"
arch: [amd64, arm64]
install:
  - name: xray
    kind: binary                 # "binary" (default) or "file"
    version: "26.3.27-1"
    url: https://radar.mehrnet.com/releases/xray/xray_latest_{os}_{arch}.{ext}
    path: __TOOLS_DIR__/xray
  - name: xray-prepare.sh
    kind: file
    url: https://radar.mehrnet.com/install/modules/xray-prepare.sh
    path: __MODULES_DIR__/xray-prepare.sh
```

- `kind: binary` (the default) is fetched as a goreleaser-style archive
  (`{os}`/`{arch}`/`{ext}` placeholders in `url` resolve per this node's own
  platform, `{ext}` is `zip` on windows, `tar.gz` elsewhere), checksum-
  verified against the asset URL's own `.checksum.txt` sidecar, extracted,
  and written to `path` as an executable.
- `kind: file` is fetched as-is -- no archive, no checksum sidecar -- and
  written to `path` directly, e.g. a `prepare`/`run` wrapper script.
- `path` must start with the literal `__TOOLS_DIR__/` or `__MODULES_DIR__/`
  placeholder in a module's *canonical source* (what `fetch-module`
  actually resolves against); a module already sitting in `--modules-dir`
  may have this already resolved to a real path (see install.sh's own
  substitution below) -- `fetch-module`/`install-module` always re-fetch
  the canonical source fresh rather than trusting whatever's on disk, so
  that distinction never matters in practice.

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
enforced before every run, for every execution mode: a probe whose params
don't match (missing required field, wrong type) never reaches the real
probe/action at all. It comes back as a normal failed result with
`"error_code": "invalid_params"` (distinct from the free-text `error`) so a
UI can show "this is misconfigured" instead of "the target is down" -- see
[Result](#result) below. `response` is declarative/documentation only,
validated for well-formedness at load time but not checked against actual
output, since a check's own failure modes (partial data, a tool's varying
output shape on error) shouldn't be conflated with a request-validation
rejection.

### Bundled engine modules (xray, WireGuard, OpenVPN)

There is no built-in Go action for any proxy/VPN engine -- `xray_proxy_test`/
`singbox_proxy_test` used to be native actions here, removed in favor of
ordinary `run:`-based modules driving real, independently-versioned engine
binaries. install.sh's `--install-module=xray`/`wireguard`/`openvpn` flags
fetch those binaries (statically built, tracked daily against upstream) from
[mehrnet/static-builds](https://github.com/mehrnet/static-builds), and drop
the matching module YAML + wrapper script into `modules.d` -- see that
repo's own README for exactly what gets installed where, and see
[`fetch-module`/`install-module`/`remove-module`](#fetch-module--install-module--remove-module----go-native-module-install)
above for the same install: schema these modules declare, and the
Go-native (rather than shell-flag-driven) way to fetch/update/remove one.
This is a managed install concern, not something this binary reaches for
itself; a node with none of these flags used at install time simply never
has those probers in its inventory at all.

install.sh writes each module's own YAML to disk *verbatim* -- its
`__TOOLS_DIR__`/`__MODULES_DIR__` placeholders (inside `install:` entries'
`path` fields) are left unresolved, exactly as fetched from
`radar.mehrnet.com`. Only the wrapper scripts alongside it (e.g.
`xray-prepare.sh`) get those placeholders substituted for this node's real,
resolved directories, since their references are shell command-line
arguments meant to be resolved once, at install time. (Before this was
fixed, install.sh ran the same substitution over the module YAML too --
harmless while `install:`/`path` didn't exist yet, but the day they were
added, a resolved-instead-of-placeholder `path` value failed this binary's
own stricter parsing on every subsequent boot, crash-looping every node
whose modules got refreshed by that release. `LoadDir`'s own parsing is
permissive about this on purpose now -- see `module.InstallDependency.Path`'s
doc comment -- but install.sh writing the placeholder form in the first
place is what keeps a local module's YAML byte-identical to its canonical
source.)

`version:`/`url:` on a loaded module (and each of its `install:` entries)
are what a heartbeat reports back to `radar-api` (see `registry.
ModuleVersions` and [`POST /v1/nodes/heartbeat`](#post-v1nodesheartbeat)
below) -- how the dashboard knows a bundled module has a newer version
available, the same way it already knows about `radar-node` itself.

For `run:`-based modules, the following placeholders resolve in every
`prepare`/`run`/`teardown` command, sourced only from the fixed set below
or a `param.<name>` reference -- an unrecognized placeholder fails config
loading (and therefore process start), not a running check, and a probe can
never introduce a new one:

| Placeholder | Source |
|---|---|
| `{{target}}` | the probe's `target` |
| `{{timeout_ms}}` | the probe's `timeout_ms` |
| `{{param.<name>}}` | the probe's `params.<name>` (string values only) |
| `{{params_json}}` | path to a temp file containing the full `params` object |
| `{{alloc_port}}` | a locally-allocated free TCP port, for a `prepare` step that starts a local proxy inbound |

Trust boundary: module *definitions* (which action or command a name maps
to) come only from local YAML files, read once at process start -- a
remote probe can only *invoke* an already-loaded module by name with typed
parameters, never introduce a new command, placeholder, or action.

Each loaded module's raw YAML source and its `file_hash` (`sha256` of that
source, hex-encoded) are what a heartbeat reports and what
`POST /v1/nodes/modules` uploads on demand -- see
[`POST /v1/nodes/heartbeat`](#post-v1nodesheartbeat) below. This is how
`radar-api` learns each node's request/response schema well enough to
drive a probe-creation form dynamically, without radar-node ever pushing
full module bodies on every heartbeat.

### Subscription-link parsing (`internal/checks/subscriptionfetch`)

The `subscription_fetch` prober turns a subscription URL's own content
(a base64/raw VLESS/VMess/Trojan/Shadowsocks URI list, a full xray-style
JSON config, or a plain host:port list) into xray `outbound`/
`streamSettings` configs, hand-rolled per protocol/transport rather than
delegated to a library -- there is no CGO-free, cross-platform-buildable
Go package that does this for us today. That hand-rolling is exactly
what's produced several real gaps and bugs this project has hit in
production (missing `xhttpSettings`, missing `tcpSettings.header`
obfuscation, `grpcSettings.serviceName` read from the wrong query param,
several TLS fields never wired up at all) -- each fixed as found, not by
design.

[kutovoys/xray-checker](https://github.com/kutovoys/xray-checker) solves
the same problem (subscription proxies -> xray config -> connectivity
check) and is the reference this package's own field coverage is checked
against when in doubt. It gets its own parsing correctness from
[xtls/libxray](https://github.com/xtls/libxray) -- a share-link/config
conversion library published by the same `xtls` org that maintains
`xray-core` itself, so it tracks xray-core's own config-format changes
directly rather than reactively. Adopting `libxray` here directly (in
place of this package's own hand-rolled parsing) hasn't been done -- it's
a real architecture change (a new, sizeable dependency; unclear
cross-platform build story for this binary's own `CGO_ENABLED=0`/6-target
release matrix) -- but if this package's own gaps keep recurring, that's
the more durable fix to reach for before patching another individual
field.

## Wire protocol -- radar-node <-> radar-api

This is the contract between a `radar-node agent` process and `radar-api`.
Both sides build against it; changing a shape here is a breaking change and
must bump `spec_version`.

Nothing in this protocol carries billing information. A node knows exactly
two things about its own standing: whether the API accepted its results,
and whether the API still wants it to keep working. Balances, prices, and
plans are entirely `radar-api`'s concern.

There is no server-computed dispatch. A node syncs its own current probe
assignment via content-hash comparison and keeps its own local cache of
what to run and when; the server never computes per-tick "what's due"
itself. A node sends its own last-known hash on every heartbeat; the server
compares it against what it has compiled for that node and replies either
"you're caught up" (match, nothing sent back) or the full current
assignment (mismatch) -- never a delta to merge in. This pushes all
scheduling compute to the node, where it's cheap and doesn't compete for
central-database write capacity, the same reasoning the old incremental-log
design had, just without that design's own unbounded-growth problem (every
single probe-definition change used to leave a permanent row behind,
regardless of whether any node still needed it).

### Conventions

- **IDs** are ULIDs (lexicographically sortable, timestamp-prefixed),
  rendered as strings with a type prefix for readability: `probe_01J...`,
  `node_01J...`, `batch_01J...`, `acct_01J...`. A `run_id` is the exception
  -- see [Result](#result).
- **Timestamps** are RFC 3339, UTC, millisecond precision:
  `"2026-07-12T10:00:03.123Z"`. `starts_at`/`ends_at` on a `ProbeSnapshot`
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
- Every top-level request/response body carries `"spec_version": 2`.
  `radar-api` deliberately never hard-rejects an older `spec_version` --
  a BYO node can't be force-upgraded, and every other part of a heartbeat
  (liveness, module sync, the pending-action/auto-update handshake) still
  has to work regardless, since a stuck node's *only* path off an old
  version is receiving an auto-update action on this same response. An
  older agent (`spec_version` 1, `since_seq`-based) simply keeps getting
  the old delta-log response shape it already understands; only a request
  that actually carries `probe_hash` gets the new hash-compare shape back.
- All node-facing endpoints require the node bearer token
  (`Authorization: Bearer <node_id>:<node_secret>`), issued once at node
  registration -- no JWT, no request signing.
- `seq` (on `Result`, and on `Event` when it's a `"triggered"` entry) is a
  plain, informational identifier -- not a sync cursor. A pre-2.0 agent's
  `since_seq` was the one exception (a monotonic, not-necessarily-
  contiguous cursor it never interpreted gaps in, only ever asking for
  "everything after the highest seq I've already applied") -- spec_version
  2 has no cursor at all, see [`probe_hash`/`probes`](#post-v1nodesheartbeat)
  below.

### Entities

#### Probe

Created by a user through `radar-api` (never by a node). Describes *what*
to check, *how often*, and *which nodes* should do it. A node never sees
this shape directly -- what it receives is a [ProbeSnapshot](#probesnapshot), a
flattened, self-contained view of the same probe.

```jsonc
{
  "probe_id": "probe_01J8Z3K7QK6H1S8YB6WQXQABCD",
  "account_id": "acct_01J8Z2Q1N9R4F0T3X8Y6Z1WXYZ",
  "target": "1.2.3.4:443",
  "prober": "tcp",                 // native check name or a custom module name
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
nodes a probe runs on, not delegating that choice to the platform.

`params` is a flat JSON object of strings for the common case. For a
custom prober that needs structured input beyond flat key/value pairs,
`params` may contain nested JSON of any shape; native checks ignore keys
they don't recognize, and a custom module's command template can reference
`{{params_json}}` to receive the *entire* params object as a temp file.

`status: "inactive_billing"` is a billing-driven cascade, distinct from
user-set `"paused"`/`"archived"`: when an account's balance runs out,
every probe it owns (and, for BYO nodes, every node it owns) flips to
`inactive_billing` automatically, and flips back to `active` on top-up. A
node treats it exactly like `"paused"` -- not due, ever -- and never needs
to know *why*.

#### ProbeSnapshot

The full current definition of a probe, as carried by every
[Event](#event). Flattened and self-contained -- a node applies this
directly to its local cache with no follow-up lookup of any kind.

```jsonc
{
  "id": "probe_01J8Z3K7QK6H1S8YB6WQXQABCD",
  "target": "1.2.3.4:443",
  "prober": "tcp",
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

#### Probe assignment sync (`probe_hash`/`probes`)

As of `spec_version` 2, a node's own probe assignment (which probes it
should be running at all) syncs via content-hash comparison, folded into
`POST /v1/nodes/heartbeat`'s `probe_hash` request field and `probe_hash`/
`probes` response fields -- not an incremental event log. There is no
cursor: a node sends its own last-known `probe_hash` (empty on its very
first-ever heartbeat, since nothing's cached yet); the server compares it
against what it has compiled for that node and replies with:

- **A hash match** -- `probe_hash`/`probes` are both *absent* from the
  response entirely (not empty). Nothing changed; the node's cache is
  already correct, there's nothing to do.
- **A hash mismatch** -- both fields are present. `probes` is this node's
  **full current assignment** (every [ProbeSnapshot](#probesnapshot) it
  should now be running), never a delta. The node replaces its whole local
  cache with it and adopts the accompanying `probe_hash` as its own new
  last-known value to send on the next heartbeat.

Applying a full replacement is not a bare cache swap, though: a probe
present in both the old and new assignment must keep this node's own
memory of when it last ran it, or every probe on this node would look
simultaneously overdue the instant any single mismatch resolves --
causing a check-burst. Only a fresh `starts_at`/interval computation
against that *preserved* history decides due-ness going forward, same as
it always has. A probe absent from the new assignment (removed, paused,
deactivated, ...) is simply dropped from the cache.

A node that has never synced (or has gone stale enough that its own
locally-cached probe set no longer matches anything) just sends an empty
`probe_hash` -- that alone guarantees a mismatch against whatever the
server has, so it always converges to the correct current state in one
round trip, the same "no separate bootstrap path" guarantee `since_seq=0`
used to give under the old protocol.

#### Event

As of `spec_version` 2, the only entries a node still receives here are
`"triggered"` ones (a manual/Quick Check run) -- probe-assignment changes
are carried by `probe_hash`/`probes` (see "Probe assignment sync" above)
instead. Still delivered inline on `POST /v1/nodes/heartbeat`'s own
`events` response field, on every heartbeat, regardless of whether
`probe_hash` also matched or mismatched that same request.

```jsonc
{
  "seq": 42,
  "event_type": "triggered",
  "probe": { /* ProbeSnapshot, see above */ },
  "run_id": "run_01J8Z4M5N6P7Q8R9S0T1U2V3WX"
}
```

The node queues an immediate one-off run of `probe` under `run_id` --
entirely independent of that probe's own normal schedule/due-ness
bookkeeping (a triggered run never touches when the probe is next due on
its own schedule), and every node executing the same trigger reports
results back under that one shared `run_id` so they correlate into a
single run server-side instead of each node minting its own independent
one the way a normal scheduled execution still does. `seq` here is a
plain informational identifier now, not a sync cursor -- there's nothing
to resume from it.

#### Result

One probe's outcome. This is `probe.Result` (the Go type already
implemented) plus the correlation fields needed to route it back to the
right probe/account server-side.

`run_id` is minted by the node itself (a ULID, generated fresh whenever a
cached probe is found due) -- there is no dispatch step for the server to
issue one from anymore.

```jsonc
{
  "run_id": "run_01J8Z4M5N6P7Q8R9S0T1U2V3WX",
  "probe_id": "probe_01J8Z3K7QK6H1S8YB6WQXQABCD",
  "seq": 1,
  "ok": true,
  "type": "tcp",
  "target": "1.2.3.4:443",
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
  "probe_id": "probe_01J8Z3K7QK6H1S8YB6WQXQABCD",
  "seq": 2,
  "ok": false,
  "type": "tcp",
  "target": "1.2.3.4:443",
  "error": "dial tcp 1.2.3.4:443: connect: connection refused",
  "observed_at": "2026-07-12T10:00:05.812Z"
}
```

A failure caused by a probe's params not matching its module's declared
`request` schema (see [Modules](#modules)) additionally carries
`error_code`, distinguishing "this probe is misconfigured" from a real probe
attempt that failed -- no probe/action was ever attempted in this case:

```jsonc
{
  "run_id": "run_01J8Z4M5N6P7Q8R9S0T1U2V3WX",
  "probe_id": "probe_01J8Z3K7QK6H1S8YB6WQXQABCD",
  "seq": 1,
  "ok": false,
  "type": "xray-vless",
  "target": "1.2.3.4:6300",
  "error": "missing required param \"uuid\"",
  "error_code": "invalid_params",
  "observed_at": "2026-07-12T10:00:05.812Z"
}
```

### Clock sync

A node's local scheduling decisions must agree with the server's notion of
time, not the node's own possibly-drifted wall clock. Every
`POST /v1/nodes/heartbeat` response includes `server_time`; the node
measures its own request/response round trip around that call and derives
an offset using a standard NTP-style midpoint correction:

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
refreshed on every heartbeat, so it self-corrects if server or node clock
drifts over a long-running process.

### Local scheduling

There is no `window_seconds` fetch parameter and no server-side "due"
computation. Instead, a node runs two independent loops:

1. **Heartbeat** -- periodic (interval suggested by the server, see
   below), and also where probe-assignment sync happens: each call sends
   this node's own last-known `probe_hash`, and the response either
   confirms it's still current (nothing to apply) or hands back the full
   current assignment on a mismatch, which replaces the local probe
   cache wholesale (see "Probe assignment sync" above) -- there's no
   cursor to advance. A `"triggered"` entry in the response's `events`
   array is applied separately, queuing an immediate one-off run. This
   used to be two independent loops on their own fixed timers (a
   heartbeat and an events poll), each paying its own request/auth round
   trip for what's almost always zero new information -- merged into one
   since there's no freshness lost by piggybacking probe-sync on a call
   that already happens this often. Also updates the clock offset from
   `server_time` on every call.
2. **Scheduler tick** -- on a short, fixed local interval, scans the
   cached probes and executes whichever are due per `node_now()`. A probe is
   marked as run (its last-run timestamp updated) *before* the probes
   actually execute, not after -- this trades a narrow, benign failure
   mode ("a slow probe run causes the next tick to skip an occurrence it
   technically could have started") for a much worse one it avoids
   entirely ("a probe with a short interval or a `once` schedule fires
   twice because two ticks raced to claim it while it was still
   running").

A probe that's never resulted because a node crashed or lost network
mid-run is not explicitly retried or reconciled by the server -- for
`interval` probes, the next due tick after the node recovers naturally
produces a fresh run; for `once` probes, the node's local "already ran
this" bookkeeping is only in memory, so a crash before completion means
it's picked up again once the process restarts and resyncs.

### Endpoints

#### `POST /v1/nodes`

One-time. Used at node provisioning (root-operated boxes and
account-added BYO nodes alike) to mint credentials. Called with an
account bearer token (not a node token, since the node doesn't exist
yet), typically by whatever provisioning flow/UI adds the node -- an
ordinary account-authed REST call, not part of the `spec_version`-
carrying wire protocol the rest of this document covers, so it carries
no `spec_version` field itself.

Request:
```jsonc
{ "name": "eu-west-3" }
```

Response:
```jsonc
{
  "node_id": "node_01J8Z1A2B3C4D5E6F7G8H9J0KL",
  "node_secret": "9f2c...redacted...a41d"   // shown once, never retrievable again
}
```

#### `POST /v1/nodes/results`

Batched, one call per scheduler-tick's worth of completed runs rather
than one call per probe.

Request:
```jsonc
{
  "spec_version": 2,
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
running out or a probe/account going `inactive_billing` mid-way:
```jsonc
{
  "spec_version": 2,
  "accepted": 4,
  "rejected": 1,
  "results": [
    { "run_id": "run_01J...", "seq": 1, "accepted": true },
    { "run_id": "run_01J...", "seq": 2, "accepted": false, "reason": "account_inactive" }
  ],
  "node_status": "active"
}
```

`reason` is a small closed enum (`unknown_probe`, `probe_inactive`,
`account_inactive`, `duplicate`) -- a node doesn't interpret these beyond
logging them; it has no billing model of its own. `probe_inactive`/
`account_inactive` should be rare in practice: the node's own cache
already stops scheduling a probe the moment its `status` flips via the
events log, so these mostly cover the narrow race where a run was
already in flight when the status changed.

#### `POST /v1/nodes/heartbeat`

Periodic (interval suggested by the API, see response) -- the content-
addressed module sync handshake, probe-assignment sync, and clock
calibration are all folded into this single call rather than three
separate polls. `probers` is a compact inventory, one
`"prober_id:file_hash"` entry per loaded module -- mirrors the
`"node_id:secret"` token convention elsewhere in this protocol. There is
no `kind`/`engine`/`engine_version` here anymore; that metadata is
attached to the hash itself server-side (see `POST /v1/nodes/modules`),
populated once rather than repeated on every heartbeat. `probe_hash` is
this node's own last-known content hash for its compiled probe
assignment (empty for a full resync) -- see "Probe assignment sync"
above and [Local scheduling](#local-scheduling).

`os`/`arch` are this process's own `runtime.GOOS`/`runtime.GOARCH` --
how `radar-api` knows whether a given bundled module (not every engine
has builds for every platform) can even be offered to this specific
node at all. `modules` is every *loaded* module's own `version`/`url`
(both `null`, not omitted, for a module authored without them -- the
embedded tcp/udp/dns/... defaults, or an unmigrated custom module),
keyed by `prober_id` -- entirely separate from `probers`' own
`prober_id:file_hash` pairs above, which exist for the module-sync
handshake, not human-readable version tracking. This is how the
dashboard shows a bundled module's own version under a node's title,
and flags one as outdated (`outdated_modules` on `GET /v1/nodes`) the
same way it already does for `agent_version` itself -- both share the
same remediation, since re-running install.sh/node.sh refreshes every
already-opted-into module in the same pass as the binary itself.

Request:
```jsonc
{
  "spec_version": 2,
  "node_id": "node_01J8Z1A2B3C4D5E6F7G8H9J0KL",
  "agent_version": "0.3.1",
  "os": "linux",
  "arch": "amd64",
  "probers": [
    "tcp:6985e90a888a115f28bbc83ae985f30a399e9ca9ab162a3af734bbd1ac2e64f",
    "xray-vless:a1b2c3d4e5f6..."
  ],
  "modules": {
    "xray-vless": { "version": "26.3.27-1", "url": "https://radar.mehrnet.com/install/modules/xray.yaml" }
  },
  "probe_hash": "b17c...redacted...9f02",
  "sent_at": "2026-07-12T10:00:00.000Z"
}
```

Response (200 -- `probe_hash` still matches, nothing to sync):
```jsonc
{
  "spec_version": 2,
  "node_status": "active",           // "active" | "suspended" | "deactivated" | "inactive_billing"
  "heartbeat_interval_seconds": 30,
  "server_time": "2026-07-12T10:00:00.123Z"
}
```

Response (200 -- `probe_hash` mismatched; full current assignment, plus an
unrelated pending trigger):
```jsonc
{
  "spec_version": 2,
  "node_status": "active",
  "heartbeat_interval_seconds": 30,
  "server_time": "2026-07-12T10:00:00.123Z",
  "probe_hash": "a94e...redacted...c30b",
  "probes": [ /* ProbeSnapshot, one per currently-assigned probe */ ],
  "events": [
    { "seq": 43, "event_type": "triggered", "probe": { /* ProbeSnapshot */ }, "run_id": "run_01J..." }
  ]
}
```

`probe_hash`/`probes` are both entirely absent (not empty) whenever this
node's own reported hash already matched -- that's the normal, common
case on most heartbeats. An absent (or empty) `events` array is equally
normal -- it just means no trigger is currently pending for this node.
The node uses `server_time` from every response to refresh its clock
offset regardless of whether anything else changed.

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
runs (existing cached probes are left in place, simply not acted on) until
a future heartbeat says `active` again. This, plus `--api-proxy` and
reading `node_status`/per-result `reason`, is the entire extent of what a
node needs to understand about its own standing -- everything else (why
it's inactive, what plan the account is on, what the balance is) is
`radar-api`/dashboard-only information a node never sees.

Two more fields ride along on this same response, both owner-triggered
from the radar UI:

- `command` -- `"delete"` (this node was removed from radar; stop running
  and tell the operator how to fully uninstall) or, only for an agent
  older than the ack protocol below, `"update"` as a backward-compatible
  fallback. `"delete"` is not fire-once: it keeps coming until the agent
  uninstalls for good or an owner restores the node. A current agent has
  no `"update"` case in its own dispatch at all -- see `pending_action`
  instead.
- `pending_action` -- an `{"id", "kind", "actions"}` object (`kind` is
  `"update"` or `"module_actions"`; `actions` is only present for the
  latter, the same `install_xray`/`remove_wireguard`/... strings the edit
  modal's Save button batches) still awaiting this agent's
  acknowledgement. Absent once acked -- see
  [`POST /v1/nodes/ack`](#post-v1nodesack) directly below for why acking
  matters and what this agent does with `kind`/`actions` once it has.
  Redelivered on every heartbeat while un-acked, up to a small fixed
  number of attempts, then dropped server-side if never acknowledged (an
  older, pre-ack agent that only understands `command` above falls into
  exactly this path for `"update"`, resolved instead by the server simply
  noticing this node's own `agent_version` changed on a later heartbeat).

#### `POST /v1/nodes/ack`

Confirms receipt of a `pending_action` a heartbeat just handed back --
call this the instant this agent decides to act on it (before re-execing
`install.sh`, which is about to kill this process), never after. This is
what lets `radar-api` tell "delivered in a heartbeat response" apart from
"actually received and being acted on", instead of clearing the pending
state the moment it's handed back once -- which used to make the
dashboard's update button reappear, with this node still mid-restart,
after nothing more than one heartbeat round trip.

Request:
```jsonc
{ "id": "action_9f2c...redacted...a41d" }
```

Response (200 -- acknowledged):
```jsonc
{ "ok": true }
```

A `410` means `radar-api` no longer recognizes this id -- either it was
never truly pending, or the server already gave up on it (too many
un-acked heartbeats, or something else superseded it) before this ack
arrived. Either way, this agent must not proceed with whatever it was
about to do: an ack that loses this race is the signal to bail, not to
barrel ahead assuming the server still agrees the action is live.

#### `POST /v1/nodes/modules`

Uploads the full definition of one or more modules named in a
heartbeat's `missing_prober_ids`. `file_hash` must equal
`sha256(yaml)`; the server independently verifies this rather than
trusting the claim. `manifest` is the same manifest `yaml` itself
parses to (name/engine/engine_version/request/response), sent
alongside the raw YAML as plain already-validated JSON -- the server
derives `kind`/`engine`/`engine_version` and the request/response
schema it later serves from `GET /v1/probers/:proberId/schema` from
`manifest` directly, never by parsing YAML itself; `yaml` itself is
only ever stored verbatim, never parsed server-side.

Request:
```jsonc
{
  "spec_version": 2,
  "node_id": "node_01J8Z1A2B3C4D5E6F7G8H9J0KL",
  "modules": [
    {
      "prober_id": "xray-vless",
      "file_hash": "a1b2c3d4e5f6...",
      "yaml": "name: xray-vless\nengine: xray\n...",
      "manifest": {
        "name": "xray-vless",
        "engine": "xray",
        "engine_version": "26.3.27",
        "request": [],
        "response": [
          { "name": "latency_ms", "type": "number", "primary": true }
        ]
      }
    }
  ]
}
```

Response:
```jsonc
{ "spec_version": 1, "stored": 1 }
```

(`spec_version` here is currently always `1`, regardless of the
request's own `spec_version` -- this response body's shape hasn't
changed since the original protocol version, worth confirming with
radar-api directly if that ever looks surprising rather than assuming
it's a typo.)

`stored` counts only genuinely new content -- radar-api is
content-addressed on `file_hash` server-side, so uploading a hash it
has already seen (from this node or any other) is a no-op parse-wise;
the upload still updates which hash *this* node currently points at
for that `prober_id`.
