// Package agent implements the `radar-node agent` loop. Unlike
// the original design, the server never tells this agent what's due
// -- it syncs its own probe *assignment* via content-hash comparison
// (folded into POST /v1/nodes/heartbeat's probe_hash/probes, see
// heartbeatLoop) into a local cache, decides for itself when something
// is due using its own clock-corrected notion of "now" (see clock.go),
// runs it through the same Checkers the `probe` subcommand uses, and
// reports results back keyed by a locally-generated run id. See
// README.md for the wire contract this package implements.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mehrnet/radar-node/internal/apiclient"
	"github.com/mehrnet/radar-node/internal/destgate"
	"github.com/mehrnet/radar-node/internal/journal"
	"github.com/mehrnet/radar-node/internal/module"
	"github.com/mehrnet/radar-node/internal/probe"
	"github.com/mehrnet/radar-node/internal/registry"
	"github.com/mehrnet/radar-node/internal/store"
	"github.com/mehrnet/radar-node/internal/wire"
)

// installScriptURL is the same one-liner README.md documents for a
// fresh install -- self-update re-runs it verbatim (same node_id/
// api_key/api_url/proxy this process itself was started with), which
// is what lets it reuse install.sh's own stop-download-replace-
// restart sequence instead of this process trying to replace its own
// running binary directly. Served from radar's own origin (a plain
// copy of this exact file, kept in sync by hand -- see radar/install/
// node.sh) rather than raw.githubusercontent.com directly, so a node
// whose network policy allowlists radar.mehrnet.com but not GitHub
// can still self-update.
const installScriptURL = "https://radar.mehrnet.com/install/node.sh"

type Config struct {
	APIURL   string
	APIKey   string // "node_id:secret" -- also the bearer token as-is
	ProxyURL string
	// Version is the real build version (main.version, injected by
	// goreleaser's ldflags for a tagged release -- see cmd/radar-node/
	// main.go), reported in every heartbeat (see heartbeatLoop) and
	// compared server-side against the latest GitHub release to decide
	// whether to offer an update. Previously this was a hardcoded
	// constant here, permanently out of sync with what a build
	// actually was -- "dev" for an untagged local build, same as
	// main.version's own fallback.
	Version string
	// SchedulerTick is how often the local scheduler checks its
	// cached probes for due-ness. This governs real-world scheduling
	// granularity (a 30s-interval probe can fire up to one tick late),
	// not network traffic -- a tick with nothing due does no I/O at
	// all.
	SchedulerTick time.Duration
	// Concurrency is the check-execution parallelism. Zero auto-tunes
	// from available RAM (roughly 4-64); an explicit nonzero value always
	// wins.
	Concurrency int
	// Resource bounds for huge/public assignments. Zero uses defaults
	// (512 jobs per tick, 32 per probe, 16 triggers); per-probe is capped
	// to per-tick in Run. StartupJitter zero disables staggering; the CLI
	// sets a real default so tests and embedding callers aren't delayed.
	MaxJobsPerTick     int
	MaxJobsPerProbe    int
	MaxTriggersPerTick int
	StartupJitter      time.Duration
	// MemoryLimitBytes is a soft Go-runtime memory limit. Zero derives
	// 65% of available memory (32 MiB floor); an explicit GOMEMLIMIT
	// environment value is left alone.
	MemoryLimitBytes int64
	// PoolConcurrency and PoolMaxJobs lower a pooled module's own
	// test_concurrency / max_jobs_per_instance for resource-constrained
	// nodes. Zero keeps the module's value.
	PoolConcurrency int
	PoolMaxJobs     int
	// DomainStrategy is the node-level address-family choice exported
	// to module child processes as RADAR_DOMAIN_STRATEGY (e.g. UseIPv4
	// on a cellular vantage with a broken IPv6 path). Empty leaves each
	// outbound's own setting unchanged.
	DomainStrategy string
	// SpoolDir enables durable result delivery: every batch is written
	// here before it is POSTed and replayed after an API failure. Empty
	// keeps the old fire-and-forget behavior. SpoolMaxBytes bounds the
	// directory's total size; zero uses the default.
	SpoolDir      string
	SpoolMaxBytes int64
	// DBPath enables the SQLite journal: durable results plus retained
	// probe/result history. It supersedes SpoolDir when set. DBMaxBytes
	// and DBRetention bound the database; zero uses the store defaults.
	DBPath      string
	DBMaxBytes  int64
	DBRetention time.Duration
	// DBSync is "normal" (default) or "full"; see store.Options.Sync.
	DBSync string
	// DestinationInterval is the minimum spacing internal/destgate
	// enforces between two connection attempts aimed at the same real
	// destination, node-wide, regardless of which probe/group/
	// subscription/account asked for either -- see destgate's own doc
	// comment for why this exists. Zero disables it.
	DestinationInterval time.Duration
	// DestinationMaxWait caps how long a single check will ever wait
	// for its own destination to clear (see destgate.Configure's own
	// comment on why this is a separate budget from a check's own
	// timeout_ms) before giving up and failing that one check.
	DestinationMaxWait time.Duration
	// ModulesDir loads probers from *.yaml/*.yml files there, on top
	// of (and overriding by name) the embedded default fixtures
	// (tcp/udp/dns/icmp/http/https/system). Empty means defaults-only.
	ModulesDir string
	// ToolsDir is only used to resolve __TOOLS_DIR__ in a loaded
	// module's own prepare/run/teardown command argv (see
	// registry.LoadModules) -- this process never reads or writes
	// anything under it itself, unlike --fetch-module/--install-module
	// (see moduleinstall.Config's own ToolsDir), which actually places
	// binaries there.
	ToolsDir string
}

// agent bundles everything the concurrent loops (heartbeat -- which
// also carries probe-definition sync, see heartbeatLoop -- scheduler,
// and local-store maintenance) share, so none needs a long,
// overlapping positional parameter list just to thread the same
// handful of dependencies through -- client/nodeID/reg in particular
// were previously repeated across nearly every function signature in
// this package.
type agent struct {
	client      *apiclient.Client
	nodeID      string
	apiKey      string
	apiURL      string
	proxyURL    string
	version     string
	reg         registry.Registry
	cache       *probeCache
	clock       *clockSync
	concurrency int
	spool       *resultSpool
	store       *store.Store
	// Scheduler-owned retry state for a failed delivery drain. Only the
	// scheduler goroutine touches these.
	deliveryBackoff     time.Duration
	deliveryNextAttempt time.Time
	// Local database bounds; used by the maintenance loop.
	dbRetention time.Duration
	dbMaxBytes  int64
	// Lightweight, latest-only health telemetry and rolling path-health
	// counters. The heartbeat goroutine owns sampling; the scheduler
	// records check outcomes and deferred-job counts.
	health       *healthSampler
	path         *pathHealth
	deferredJobs atomic.Int64
	// Scheduler resource bounds, fixed at startup.
	maxJobsPerTick     int
	maxJobsPerProbe    int
	maxTriggersPerTick int
	startupJitter      time.Duration
	// ready flips after the first heartbeat that confirms this node's
	// current status. Restored probes must not run until then,
	// otherwise a node suspended while offline could race its first
	// heartbeat and execute a few checks.
	ready atomic.Bool
	// triggersRestored flips once the pending-trigger queue has been
	// read from disk. If that read fails at startup, the heartbeat
	// loop retries it instead of leaving already-persisted triggers
	// stranded behind RecordTrigger's dedup check.
	triggersRestored atomic.Bool
	// node_status starts optimistic; the first heartbeat/results
	// response corrects it. An atomic.Value rather than a mutex so the
	// scheduler can gate execution on it with no lock contention.
	status atomic.Value
}

// Run blocks until ctx is cancelled, running the heartbeat, scheduler,
// and (when SQLite is enabled) maintenance loops concurrently.
func Run(ctx context.Context, cfg Config) error {
	nodeID, _, ok := strings.Cut(cfg.APIKey, ":")
	if !ok || nodeID == "" {
		return fmt.Errorf("--api-key must be in node_id:secret form")
	}
	if cfg.SchedulerTick <= 0 {
		return fmt.Errorf("--scheduler-tick must be positive")
	}
	if cfg.Concurrency < 0 {
		return fmt.Errorf("--concurrency must not be negative")
	}
	availableMemory := availableMemoryBytes()
	if cfg.Concurrency == 0 {
		cfg.Concurrency = autoConcurrency(availableMemory)
	}
	if cfg.Concurrency < 1 {
		return fmt.Errorf("--concurrency must be positive")
	}
	if cfg.MaxJobsPerTick < 0 || cfg.MaxJobsPerProbe < 0 || cfg.MaxTriggersPerTick < 0 {
		return fmt.Errorf("scheduler job limits must not be negative")
	}
	if cfg.StartupJitter < 0 {
		return fmt.Errorf("--startup-jitter must not be negative")
	}
	if cfg.MemoryLimitBytes < 0 {
		return fmt.Errorf("--mem-limit must not be negative")
	}
	maxJobsPerTick := cfg.MaxJobsPerTick
	if maxJobsPerTick == 0 {
		maxJobsPerTick = 512
	}
	maxJobsPerProbe := cfg.MaxJobsPerProbe
	if maxJobsPerProbe == 0 {
		maxJobsPerProbe = 32
	}
	if maxJobsPerProbe > maxJobsPerTick {
		maxJobsPerProbe = maxJobsPerTick
	}
	maxTriggersPerTick := cfg.MaxTriggersPerTick
	if maxTriggersPerTick == 0 {
		maxTriggersPerTick = 16
	}
	startupJitter := cfg.StartupJitter
	if cfg.MemoryLimitBytes == 0 && os.Getenv("GOMEMLIMIT") == "" {
		cfg.MemoryLimitBytes = autoMemoryLimit(availableMemory)
	}
	if cfg.MemoryLimitBytes > 0 {
		debug.SetMemoryLimit(cfg.MemoryLimitBytes)
	}
	if cfg.PoolConcurrency < 0 {
		return fmt.Errorf("--pool-concurrency must not be negative")
	}
	if cfg.PoolMaxJobs < 0 {
		return fmt.Errorf("--pool-max-jobs must not be negative")
	}
	if cfg.SpoolMaxBytes < 0 {
		return fmt.Errorf("--spool-max-bytes must not be negative")
	}
	if cfg.DBMaxBytes < 0 {
		return fmt.Errorf("--db-max-bytes must not be negative")
	}
	if cfg.DBRetention < 0 {
		return fmt.Errorf("--db-retention must not be negative")
	}
	var spool *resultSpool
	var localStore *store.Store
	var err error
	if cfg.DBPath != "" {
		opened, openErr := store.Open(cfg.DBPath, store.Options{Origin: nodeID, Sync: cfg.DBSync})
		if openErr != nil {
			journal.Error("agent", "local store unavailable, falling back", "path", cfg.DBPath, "err", openErr)
			if cfg.SpoolDir == "" {
				journal.Warn("agent", "continuing without durable result storage")
			}
		} else {
			localStore = opened
			if cfg.SpoolDir != "" {
				journal.Warn("agent", "--spool-dir is ignored while the local database is enabled", "db", cfg.DBPath)
			}
			defer func() {
				if err := localStore.Close(); err != nil {
					journal.Warn("agent", "close local store failed", "err", err)
				}
			}()
		}
	}
	if localStore == nil {
		spool, err = newResultSpool(cfg.SpoolDir, cfg.SpoolMaxBytes)
		if err != nil {
			return err
		}
	}
	domainStrategy, err := normalizeDomainStrategy(cfg.DomainStrategy)
	if err != nil {
		return err
	}
	if domainStrategy != "" {
		if err := os.Setenv("RADAR_DOMAIN_STRATEGY", domainStrategy); err != nil {
			return fmt.Errorf("set RADAR_DOMAIN_STRATEGY: %w", err)
		}
	}

	client, err := apiclient.New(cfg.APIURL, cfg.APIKey, cfg.ProxyURL)
	if err != nil {
		return err
	}
	limits := module.PoolLimits{TestConcurrency: cfg.PoolConcurrency, MaxJobsPerInstance: cfg.PoolMaxJobs}
	reg, err := registry.DefaultWithLimits(limits)
	if err != nil {
		return err
	}
	if err := reg.LoadModulesWithLimits(cfg.ModulesDir, cfg.ToolsDir, limits); err != nil {
		return err
	}
	destgate.Configure(cfg.DestinationInterval, cfg.DestinationMaxWait)

	version := cfg.Version
	if version == "" {
		version = "dev"
	}
	dbRetention := cfg.DBRetention
	if dbRetention == 0 {
		dbRetention = store.DefaultRetention
	}
	dbMaxBytes := cfg.DBMaxBytes
	if dbMaxBytes == 0 {
		dbMaxBytes = store.DefaultMaxBytes
	}
	a := &agent{
		client:             client,
		nodeID:             nodeID,
		apiKey:             cfg.APIKey,
		apiURL:             cfg.APIURL,
		proxyURL:           cfg.ProxyURL,
		version:            version,
		reg:                reg,
		cache:              newProbeCache(),
		clock:              &clockSync{},
		concurrency:        cfg.Concurrency,
		spool:              spool,
		store:              localStore,
		dbRetention:        dbRetention,
		dbMaxBytes:         dbMaxBytes,
		health:             newHealthSampler(),
		path:               newPathHealth(),
		maxJobsPerTick:     maxJobsPerTick,
		maxJobsPerProbe:    maxJobsPerProbe,
		maxTriggersPerTick: maxTriggersPerTick,
		startupJitter:      startupJitter,
	}
	journal.Info("agent", "scheduler resource limits",
		"concurrency", cfg.Concurrency, "max_jobs_per_tick", maxJobsPerTick,
		"max_jobs_per_probe", maxJobsPerProbe, "max_triggers_per_tick", maxTriggersPerTick,
		"startup_jitter", startupJitter, "mem_limit_bytes", cfg.MemoryLimitBytes)
	a.status.Store(wire.NodeStatusActive)

	if a.store != nil {
		hash, states, err := a.store.LoadProbeStates(ctx)
		if err != nil {
			journal.Error("agent", "restore local assignment failed", "err", err)
		} else if hash != "" {
			a.cache.restore(hash, cachedStates(states))
			journal.Info("agent", "restored local assignment", "probes", len(states))
		}
		a.restorePendingTriggers(ctx)
		if stats, err := a.store.Stats(ctx); err == nil {
			journal.Info("agent", "local store ready",
				"path", a.store.Path(), "journal", stats.JournalMode,
				"results", stats.Results, "pending", stats.PendingResults,
				"rollups", stats.Rollups, "db_bytes", stats.DBBytes)
		}
	}
	a.cache.staggerUnstarted(a.clock.now(), a.startupJitter)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		a.heartbeatLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		a.schedulerLoop(ctx, cfg.SchedulerTick)
	}()
	if a.store != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.maintenanceLoop(ctx)
		}()
	}
	wg.Wait()
	// All loops are done (ctx cancelled): stop every warm pooled
	// engine the scheduler was keeping alive -- the graceful half of
	// engine cleanup, Pdeathsig being the ungraceful half.
	reg.Shutdown()
	return nil
}

// heartbeatLoop also carries probe-assignment sync (content-hash
// compared, see cache.go's own replaceAssignment) and clock
// calibration -- folded in from what used to be a separate
// eventsSyncLoop polling GET /v1/nodes/events on its own timer. Both
// loops fired on a fixed interval regardless of activity and each
// paid its own request/auth round trip; since a heartbeat already
// happens this often, there's no freshness lost by piggybacking
// assignment sync on it instead, and it halves the number of always-
// on polling requests this agent makes.
func (a *agent) heartbeatLoop(ctx context.Context) {
	interval := 30 * time.Second // sane default until the server tells us otherwise
	proberHashes := a.reg.ProberHashes()

	send := func() (*wire.HeartbeatResponse, time.Time, time.Time, error) {
		hbCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		sentAt := time.Now()
		var health *wire.NodeHealth
		if a.health != nil {
			pathState := "ok"
			if a.path != nil {
				pathState = a.path.state(sentAt)
			}
			health = a.health.snapshot(sentAt, pathState, int(a.deferredJobs.Load()))
		}
		resp, err := a.client.Heartbeat(hbCtx, wire.HeartbeatRequest{
			NodeID:       a.nodeID,
			AgentVersion: a.version,
			OS:           runtime.GOOS,
			Arch:         runtime.GOARCH,
			Probers:      proberHashes,
			Modules:      a.reg.ModuleVersions(),
			ProbeHash:    a.cache.lastKnownHash(),
			Health:       health,
			SentAt:       sentAt.UTC().Format(time.RFC3339Nano),
		})
		if a.health != nil {
			a.health.noteHeartbeat(time.Since(sentAt), err == nil)
		}
		return resp, sentAt, time.Now(), err
	}

	// beat sends the heartbeat and, if radar-api rejects it because it
	// doesn't recognize one or more of this node's current module
	// hashes, uploads exactly those named modules and retries once --
	// the common case (nothing changed since last time) never touches
	// the upload path at all.
	beat := func() {
		if a.store != nil && !a.triggersRestored.Load() {
			a.restorePendingTriggers(ctx)
		}
		resp, sentAt, receivedAt, err := send()
		var rejected *apiclient.HeartbeatRejectedError
		if errors.As(err, &rejected) {
			if uploadErr := a.uploadMissingModules(ctx, rejected.Rejection.MissingProberIDs); uploadErr != nil {
				journal.Error("agent", "upload modules failed", "err", uploadErr)
				return
			}
			resp, sentAt, receivedAt, err = send()
		}
		if err != nil {
			journal.Error("agent", "heartbeat failed", "err", err)
			return
		}
		// A heartbeat without a status has not confirmed that this
		// node is still active; leave restored probes gated.
		confirmed := resp.NodeStatus != ""
		if confirmed {
			a.status.Store(resp.NodeStatus)
		}
		if resp.HeartbeatIntervalSecs > 0 {
			interval = time.Duration(resp.HeartbeatIntervalSecs) * time.Second
		}
		if serverTime, parseErr := time.Parse(time.RFC3339Nano, resp.ServerTime); parseErr == nil {
			a.clock.update(serverTime, sentAt, receivedAt)
		}
		// Present only on a hash mismatch (see radar-api's own
		// node-protocol.ts) -- an empty ProbeHash means "nothing
		// changed, you're already caught up", not "you have zero
		// probes" (a genuinely empty assignment still carries a real,
		// non-empty hash of its own).
		if resp.ProbeHash != "" {
			persisted := true
			if a.store != nil {
				if err := a.store.SaveAssignment(ctx, resp.ProbeHash, resp.Probes); err != nil {
					journal.Error("agent", "persist probe assignment failed; will retry on a later heartbeat", "err", err)
					persisted = false
				}
			}
			if persisted {
				a.cache.replaceAssignment(resp.ProbeHash, resp.Probes)
			} else {
				a.cache.replaceAssignmentKeepingHash(resp.Probes)
			}
			a.cache.staggerUnstarted(a.clock.now(), a.startupJitter)
			journal.Info("agent", "synced probe assignment", "probes", len(resp.Probes), "persisted", persisted)
		}
		if len(resp.Events) > 0 {
			a.applyTriggeredEvents(ctx, resp.Events)
		}
		if confirmed {
			a.ready.Store(true)
		}
		switch resp.Command {
		case "delete":
			a.handleDeleteCommand()
		}
		if resp.PendingAction != nil {
			a.handlePendingAction(ctx, resp.PendingAction)
		}
	}

	beat() // report in immediately on startup rather than waiting a full interval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			beat()
			ticker.Reset(interval)
		}
	}
}

// selfUpdate re-execs the public install script as a detached child
// process, then exits -- install.sh already stops the running
// service, downloads the latest release, replaces the binary, and
// restarts it (see its own "stop existing service before cp" step,
// added specifically so this exact re-run-to-upgrade path doesn't hit
// ETXTBSY). This process exiting is what lets that cp succeed; there
// is deliberately no attempt to replace this binary in-process.
// selfUpdateLogPattern is where the detached installer's own stdout/
// stderr goes -- deliberately NOT this process's inherited os.Stdout.
// A systemd service's stdout is a journal stream tied to that unit's
// own lifecycle; once this process exits and the unit is marked
// inactive (which happens within about a second, well before the
// installer finishes downloading/replacing/restarting), writes from a
// process that merely inherited that fd can vanish from the journal
// entirely -- observed in practice as install.sh silently appearing to
// do nothing, with zero output anywhere, on every single attempt. A
// plain, independent file survives regardless of what happens to the
// parent unit. CreateTemp gives it an unpredictable name and 0600
// permissions, unlike a fixed /tmp path that another local user could
// pre-create as a symlink (and whose existing mode O_CREATE alone would
// not tighten to 0600).
const selfUpdateLogPattern = "radar-node-selfupdate-*.log"

// selfUpdateExitCode is what reinstall() exits with to hand off to
// the detached installer -- install.sh's own systemd unit template
// sets RestartPreventExitStatus=<this value> specifically so this one
// deliberate exit doesn't trigger the unit's normal Restart=always.
// Without that, systemd has no way to tell "this process is handing
// off to an installer that's about to replace/restart it anyway" apart
// from any other exit, and (observed in production) auto-restarts the
// still-old binary within RestartSec -- which then races install.sh's
// own later `systemctl stop radar-node`, landing a SIGTERM on
// whichever instance happens to be running at that moment instead of
// a cleanly-already-stopped service. Not 0: handleDeleteCommand also
// exits 0, deliberately relying on Restart=always to relaunch it (see
// its own comment) -- these two "exit on purpose" paths need opposite
// systemd behavior, so they can't share a code.
//
// launchd (install.sh's macOS service manager) has no equivalent of
// RestartPreventExitStatus -- KeepAlive has no way to except one
// specific exit code the way this does, only "restart on any exit" or
// "restart only on unsuccessful exit," neither of which fits both
// selfUpdateExitCode and handleDeleteCommand's 0 at once. The same
// class of race is plausible there too, just not something reproduced
// or fixed here -- Linux/systemd is where this was actually observed
// in production.
//
// procd (OpenWrt) also has no exit-code exception; it respawns the old
// agent after its five-second delay. The detached updater waits past
// that window, stops the service itself, and only then replaces the
// binary -- see selfUpdateCommandFor.
const selfUpdateExitCode = 42

func (a *agent) selfUpdate() {
	a.reinstall()
}

// handlePendingAction acks the given action *before* acting on it --
// reinstall/selfUpdate are about to kill this process (re-exec
// install.sh, stop the service, replace the binary), so there'd be no
// later point at which this process could still confirm receipt. If
// the server rejects the ack (410: it already gave up on this id
// after too many un-acked heartbeats, or something else superseded
// it), this bails without touching install.sh at all -- proceeding
// anyway would just race whatever the server has already moved on to.
func (a *agent) handlePendingAction(ctx context.Context, action *wire.PendingAction) {
	ackCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := a.client.AckAction(ackCtx, action.ID); err != nil {
		journal.Warn("agent", "could not ack pending action, not acting on it this heartbeat", "action_id", action.ID, "err", err)
		return
	}
	switch action.Kind {
	case "update":
		a.selfUpdate()
	case "module_actions":
		a.applyModuleActions(action.Actions)
	}
}

// moduleActionFlags maps a wire-level "install_xray"/"remove_wireguard"
// style action name to install.sh's matching --install-module=xray/
// --remove-module=wireguard flag (install.sh's own generic flag pair,
// not one literal flag per module -- see its own module_requested/
// module_dispatch). Unrecognized entries are dropped rather than
// failing the whole batch -- radar-api validates this set already
// (see its nodeModuleActionsSchema), so an entry that doesn't map here
// can only mean a newer server introduced an action this older agent
// build doesn't know about yet, not a real error.
func moduleActionFlags(actions []string) []string {
	flags := make([]string, 0, len(actions))
	for _, action := range actions {
		flag, ok := strings.CutPrefix(action, "install_")
		if ok {
			flags = append(flags, "--install-module="+flag)
			continue
		}
		if flag, ok := strings.CutPrefix(action, "remove_"); ok {
			flags = append(flags, "--remove-module="+flag)
		}
	}
	return flags
}

// applyModuleActions re-execs install.sh once with every bundled-
// engine flag this heartbeat's batch named, e.g. installing xray and
// removing wireguard together becomes one
// "--install-module=xray --remove-module=wireguard" re-run instead of
// two separate fire-once commands one click (and one full install.sh
// re-run) apart.
func (a *agent) applyModuleActions(actions []string) {
	flags := moduleActionFlags(actions)
	if len(flags) == 0 {
		return
	}
	a.reinstall(flags...)
}

// installFetchMaxAttempts/installFetchBackoff mirror node.sh's own
// fetch_with_retry convention (5 attempts, sleep attempt*2s) -- see
// buildInstallCommand's own doc comment for why this exists at all.
const installFetchMaxAttempts = 5

// shellQuote single-quotes a value for POSIX sh. Every value in the
// generated installer command that is not a fixed literal (including
// server-supplied module action suffixes) goes through this: the
// command is ultimately executed by sh, so leaving it unquoted would
// turn e.g. an action name into arbitrary shell code.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// buildInstallCommand is reinstall's own command-string logic, pulled
// out into a pure function so the proxy handling is unit-testable
// without spawning a real subprocess. proxyURL, if set, is threaded
// through twice, for two different reasons: as curl's own --proxy
// flag on the *outer* fetch of install.sh (a node whose only route to
// the internet at all is through this proxy could never even
// downloaded the script otherwise -- raw.githubusercontent.com would
// simply be unreachable), and again as a plain "--proxy=" argument
// install.sh itself only gets to see -- and act on, for its own
// internal downloads and to thread --api-proxy into the agent's own
// systemd service -- after it's already running.
//
// The outer fetch is downloaded to a real temp file and retried up to
// installFetchMaxAttempts times before install.sh ever runs, instead
// of the original "curl | sh" straight pipe -- piping a failed curl
// (e.g. the TLS EOF observed in production on a node with a flaky
// proxied path) fed `sh` an *empty* script, which is not an error to
// sh at all: it exits 0 having done nothing, so the whole self-update
// silently no-ops with zero indication anywhere that it didn't happen.
// This process has already committed to exiting (see reinstall) by
// the time that would be discovered, so there's no later point to
// detect and report it from -- the fetch has to succeed, retry, or
// fail loudly *before* control ever reaches "hand off and exit".
// Failing loudly here means the wrapping systemd unit (or plain child
// on non-systemd hosts) exits non-zero and is visible as failed in
// journalctl, and radar-api's own reconcile loop -- seeing this node's
// reported version never changed on a later heartbeat -- re-issues the
// update as a fresh pending action up to its own retry limit, the same
// way it would for any other kind of update failure.
func buildInstallCommand(nodeID, apiKey, apiURL, proxyURL string, extraFlags []string) string {
	args := []string{"--node_id=" + shellQuote(nodeID), "--api_key=" + shellQuote(apiKey), "--api_url=" + shellQuote(apiURL)}
	curlProxyFlag := ""
	if proxyURL != "" {
		curlProxyFlag = "--proxy " + shellQuote(proxyURL) + " "
		args = append(args, "--proxy="+shellQuote(proxyURL))
	}
	for _, flag := range extraFlags {
		args = append(args, shellQuote(flag))
	}
	return fmt.Sprintf(`_recover_service() {
  if command -v pidof >/dev/null 2>&1 && pidof radar-node >/dev/null 2>&1; then
    return 0
  fi
  if [ -x /etc/init.d/radar-node ]; then
    /etc/init.d/radar-node start >/dev/null 2>&1 && return 0
  fi
  if command -v systemctl >/dev/null 2>&1; then
    systemctl start radar-node >/dev/null 2>&1 && return 0
    systemctl --user start radar-node >/dev/null 2>&1 && return 0
  fi
  if command -v launchctl >/dev/null 2>&1; then
    launchctl load -w /Library/LaunchDaemons/com.mehrnet.radar-node.plist >/dev/null 2>&1 && return 0
    launchctl load -w "$HOME/Library/LaunchAgents/com.mehrnet.radar-node.plist" >/dev/null 2>&1 && return 0
  fi
  return 1
}
_script=""
trap 'status=$?; if [ "$status" -ne 0 ]; then _recover_service || true; fi; if [ -n "$_script" ]; then rm -f "$_script"; fi; exit "$status"' EXIT
set -e
_script=$(mktemp)
_attempt=1
while :; do
  if curl -fsSL --connect-timeout 10 --max-time 120 %s%s -o "$_script"; then
    break
  fi
  if [ "$_attempt" -ge %d ]; then
    echo "radar-node: failed to fetch install.sh after %d attempts" >&2
    exit 1
  fi
  echo "radar-node: install.sh fetch failed (attempt $_attempt/%d) -- retrying in $((_attempt * 2))s..." >&2
  sleep $((_attempt * 2))
  _attempt=$((_attempt + 1))
done
sh "$_script" %s
_status=$?
rm -f "$_script"
_script=""
exit $_status`, curlProxyFlag, installScriptURL, installFetchMaxAttempts, installFetchMaxAttempts, installFetchMaxAttempts, strings.Join(args, " "))
}

// reinstall re-execs install.sh with this node's own existing
// node_id/api_key/api_url/proxy (exactly like a plain "update" does),
// plus whatever extra flags the request carries when it's really about
// bundled engine modules rather than radar-node's own version -- e.g.
// "--install-module=xray" for an "install_xray" action, one or more at once
// (see applyModuleActions). install.sh itself does the actual fetch/
// verify/place and the service restart that picks up a module just
// dropped into modules.d; this is only ever "re-run that same script,
// with some extra arguments than usual."
func (a *agent) reinstall(extraFlags ...string) {
	installCmd := buildInstallCommand(a.nodeID, strings.TrimPrefix(a.apiKey, a.nodeID+":"), a.apiURL, a.proxyURL, extraFlags)
	reason := "update requested"
	if len(extraFlags) > 0 {
		reason = strings.Join(extraFlags, " ") + " requested"
	}
	// Logs the reason only, not installCmd itself -- installCmd is now a
	// multi-line generated shell script (see buildInstallCommand's own
	// retry loop), and log.Printf-ing it wholesale used to flood
	// journalctl with one entry per line of the script's own source
	// (journald splits a single write on newlines), making the actual
	// per-attempt fetch/run output below harder to find, not easier.
	journal.Info("agent", "re-running install script", "reason", reason)

	launch := selfUpdateCommand(installCmd)
	cmd := launch.cmd
	logPath := "stdout"
	logFile, err := os.CreateTemp("", selfUpdateLogPattern)
	if err != nil {
		journal.Warn("agent", "self-update: could not open log file, falling back to this process's own stdout, which may not survive the restart that follows", "err", err)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		logPath = logFile.Name()
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	if launch.detached {
		// procd has no transient-unit equivalent. This child is in its
		// own session and stops the service only after procd's respawn
		// delay has elapsed, so stopping the service cannot take the
		// updater down with it.
		if err := cmd.Start(); err != nil {
			if logFile != nil {
				_ = logFile.Close()
			}
			journal.Error("agent", "self-update: launching the detached installer failed", "err", err, "log", logPath)
			return
		}
		if logFile != nil {
			_ = logFile.Close()
		}
		journal.Info("agent", "detached installer handed off, exiting so it can replace this process", "log", logPath)
		os.Exit(selfUpdateExitCode)
	}
	// Run, not Start -- deliberately blocks here, but only on the
	// *wrapper* (systemd-run confirming the transient unit actually
	// started, a sub-second round trip through PID1's D-Bus API), never
	// on install.sh's own full download/replace/restart. Exiting via
	// Start()+immediate os.Exit(0) raced this confirmation:
	// `systemd-run --scope` needs a moment to register the new scope
	// and migrate into it, and this process's own exit (tearing down
	// its cgroup, per selfUpdateCommand's own doc comment) could kill
	// systemd-run before that finished -- observed in practice as
	// install.sh never producing a single line of output, on every
	// attempt. Waiting for confirmation first removes that race
	// instead of shrinking it.
	runErr := cmd.Run()
	if logFile != nil {
		_ = logFile.Close()
	}
	if runErr != nil {
		journal.Error("agent", "self-update: launching the installer failed", "err", runErr, "log", logPath)
		return
	}
	journal.Info("agent", "installer handed off, exiting so it can replace this process", "log", logPath)
	os.Exit(selfUpdateExitCode)
}

const (
	// procd respawn is configured by install.sh as
	// `respawn 3600 5 5`, i.e. five seconds after the agent's deliberate
	// exit. Waiting past that window lets the old binary come back, then
	// stops it, so the installer's cp cannot hit ETXTBSY.
	procdRespawnWindowSeconds  = 6
	procdSettleSeconds         = 1
	procdUpdateWatchdogSeconds = 600
	procdInitScript            = "/etc/init.d/radar-node"
)

type selfUpdateLaunch struct {
	cmd *exec.Cmd
	// detached means the child must be Start()ed and this process must
	// exit immediately; it is already isolated from the service's
	// process group and from procd's stop.
	detached bool
}

// selfUpdateCommand makes the installer survive the service being
// stopped underneath it. On systemd this means a transient unit; on
// procd (OpenWrt) it means a detached session that waits out procd's
// respawn delay and then stops the service itself. A plain child on
// macOS is already safe because launchd does not tear down orphaned
// children the same way.
func selfUpdateCommand(installCmd string) selfUpdateLaunch {
	return selfUpdateCommandFor(runtime.GOOS, os.Geteuid(), os.Getpid(), exec.LookPath, procdManaged(), installCmd)
}

// procdManaged reports whether this node is running under OpenWrt's
// procd with our own init script. /sbin/procd by itself is not enough:
// the init script is what the update wrapper stops and install.sh
// rewrites.
func procdManaged() bool {
	if _, err := os.Stat("/sbin/procd"); err != nil {
		return false
	}
	info, err := os.Stat(procdInitScript)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// selfUpdateCommandFor is selfUpdateCommand's decision logic, factored
// out for testability -- goos/euid/pid/lookPath/procd are the only
// real-world inputs it needs, so a test can exercise every branch
// without depending on the actual host it runs on.
func selfUpdateCommandFor(goos string, euid int, pid int, lookPath func(string) (string, error), procd bool, installCmd string) selfUpdateLaunch {
	if goos == "linux" {
		if procd {
			script := fmt.Sprintf(
				"( sleep %d; if ! command -v pidof >/dev/null 2>&1 || ! pidof radar-node >/dev/null 2>&1; then %s start >/dev/null 2>&1 || true; fi ) &\n_watchdog=$!\nsleep %d\n%s stop >/dev/null 2>&1 || true\nsleep %d\n( %s )\n_status=$?\nkill $_watchdog >/dev/null 2>&1 || true\nexit $_status",
				procdUpdateWatchdogSeconds, procdInitScript,
				procdRespawnWindowSeconds, procdInitScript, procdSettleSeconds, installCmd,
			)
			cmd := exec.Command("sh", "-c", script)
			cmd.SysProcAttr = detachedSysProcAttr()
			return selfUpdateLaunch{cmd: cmd, detached: true}
		}
		if path, err := lookPath("systemd-run"); err == nil {
			unitName := fmt.Sprintf("radar-node-selfupdate-%d", pid)
			runArgs := []string{"--unit=" + unitName, "--quiet", "--collect"}
			if euid != 0 {
				runArgs = append(runArgs, "--user")
			}
			runArgs = append(runArgs, "sh", "-c", installCmd)
			return selfUpdateLaunch{cmd: exec.Command(path, runArgs...)}
		}
	}
	return selfUpdateLaunch{cmd: exec.Command("sh", "-c", installCmd)}
}

// handleDeleteCommand runs when this node has been deleted from radar
// (see routes/nodes.ts's "deactivated" status transition). It does
// NOT attempt to self-uninstall a systemd/launchd service -- that
// needs privileges and unit-file knowledge this process shouldn't
// assume it has. Instead it stops running (a systemd Restart=always
// unit will just relaunch it, heartbeat once, see "delete" again, and
// exit again -- a harmless low-frequency loop, not a resource concern)
// and tells the operator exactly how to finish the job for real.
func (a *agent) handleDeleteCommand() {
	journal.Warn("agent", "this node was deleted from radar, stopping; to remove it from this machine run the uninstall command", "uninstall", fmt.Sprintf("curl -fsSL %s | sh -s -- --uninstall", installScriptURL))
	os.Exit(0)
}

// uploadMissingModules pushes exactly the modules radar-api named as
// unrecognized -- not this node's whole inventory -- via
// POST /v1/nodes/modules.
func (a *agent) uploadMissingModules(ctx context.Context, proberIDs []string) error {
	if len(proberIDs) == 0 {
		return nil
	}
	modules := make([]wire.ModuleUpload, 0, len(proberIDs))
	for _, id := range proberIDs {
		yamlSrc, fileHash, manifest, ok := a.reg.RawYAML(id)
		if !ok {
			continue // server named a prober_id this node no longer has loaded; nothing to push
		}
		modules = append(modules, wire.ModuleUpload{
			ProberID: id,
			FileHash: fileHash,
			YAML:     yamlSrc,
			Manifest: manifest,
		})
	}
	if len(modules) == 0 {
		return nil
	}
	uploadCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := a.client.UploadModules(uploadCtx, wire.ModulesUploadRequest{NodeID: a.nodeID, Modules: modules})
	return err
}

func (a *agent) schedulerLoop(ctx context.Context, tick time.Duration) {
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !a.ready.Load() {
				continue
			}
			if s, _ := a.status.Load().(string); s != wire.NodeStatusActive {
				continue
			}
			a.runDueProbes(ctx)
		}
	}
}

func (a *agent) runDueProbes(ctx context.Context) {
	now := a.clock.now()
	maxJobs := a.maxJobsPerTick
	if maxJobs < 1 {
		maxJobs = 1 << 30
	}
	perProbe := a.maxJobsPerProbe
	if perProbe < 1 {
		perProbe = 1
	}
	triggerSlots := a.maxTriggersPerTick
	if triggerSlots < 1 {
		triggerSlots = 16
	}
	// Triggers are priority work, but reserve at most the whole tick
	// budget: a trigger with a large probe_count is clamped, and
	// scheduled probes get whatever budget remains. A trigger flood is
	// bounded by maxTriggersPerTick and the durable queue cap.
	if triggerSlots*perProbe > maxJobs {
		triggerSlots = maxJobs / perProbe
		if triggerSlots < 1 {
			triggerSlots = 1
		}
	}
	triggers := a.cache.drainPendingTriggersLimited(triggerSlots)
	triggerBudget := len(triggers) * perProbe
	if triggerBudget > maxJobs {
		triggerBudget = maxJobs
	}
	probeBudget := maxJobs - triggerBudget
	due, deferred := a.cache.takeDueProbes(now, probeBudget, a.maxJobsPerProbe)
	a.deferredJobs.Store(int64(deferred + a.cache.pendingTriggerCount()))

	if len(due) == 0 && len(triggers) == 0 {
		if _, _, _, err := a.drainDelivery(ctx); err != nil {
			journal.Error("agent", "delivery replay failed", "err", err)
		}
		return
	}

	// takeDueProbes marks only the selected probes, before this tick
	// executes them, so a fast subsequent tick cannot re-select them
	// while the run is in flight. Deferred probes stay due rather than
	// being marked run and silently skipped. Triggers don't touch
	// lastRunAt at all -- they're independent of due bookkeeping.
	results := a.executeProbes(ctx, due)
	results = append(results, a.executeTriggers(ctx, triggers)...)
	if a.path != nil {
		recordedAt := time.Now()
		for _, result := range results {
			a.path.record(result.Target, result.Error, result.ErrorCode, recordedAt)
		}
	}

	if len(due) > 0 && a.store != nil {
		ids := make([]string, len(due))
		for i, pr := range due {
			ids[i] = pr.ID
		}
		if err := a.store.MarkProbesRun(ctx, ids, now); err != nil {
			journal.Error("agent", "persist probe run times failed", "err", err)
		}
	}
	if len(results) == 0 {
		a.finishTriggers(ctx, triggers, true)
		return
	}
	durable := a.reportResults(ctx, due, triggers, results)
	a.finishTriggers(ctx, triggers, durable)
}

// reportResults makes one tick's results durable, then drains pending
// deliveries. It returns whether the results are safely stored (or were
// posted directly), which decides whether their triggers may be retired.
func (a *agent) reportResults(ctx context.Context, due []wire.ProbeSnapshot, triggers []pendingTrigger, results []wire.Result) bool {
	sentAt := time.Now().UTC().Format(time.RFC3339Nano)
	if a.store != nil {
		summary, err := a.store.RecordResults(ctx, results)
		if err != nil {
			journal.Error("agent", "store results failed; falling back", "err", err)
			return a.reportFallback(ctx, due, triggers, results, sentAt)
		}
		delivered, accepted, rejected, err := a.drainStore(ctx)
		if err != nil {
			journal.Error("agent", "deliver results failed (left in local store)", "err", err,
				"probes_run", len(due), "triggered", len(triggers), "results", len(results),
				"inserted", summary.Inserted, "duplicates", summary.Duplicates,
				"invalid_latency", summary.InvalidLatency, "invalid_extra", summary.InvalidExtra,
				"delivered", delivered)
			return true
		}
		journal.Info("agent", "tick complete",
			"probes_run", len(due), "triggered", len(triggers), "results", len(results),
			"inserted", summary.Inserted, "duplicates", summary.Duplicates,
			"invalid_latency", summary.InvalidLatency, "invalid_extra", summary.InvalidExtra,
			"delivered", delivered, "accepted", accepted, "rejected", rejected)
		return true
	}
	return a.reportFallback(ctx, due, triggers, results, sentAt)
}

// reportFallback is the spool/direct path, also used when the local
// database write fails. A spool is written before the network attempt,
// so an API outage or process exit cannot lose the batch; replay is
// deduplicated server-side.
func (a *agent) reportFallback(ctx context.Context, due []wire.ProbeSnapshot, triggers []pendingTrigger, results []wire.Result, sentAt string) bool {
	if a.spool == nil {
		resp, err := a.postResults(ctx, results, sentAt)
		if err != nil {
			journal.Error("agent", "post results failed", "err", err)
			return false
		}
		journal.Info("agent", "tick complete",
			"probes_run", len(due), "triggered", len(triggers), "results", len(results),
			"accepted", resp.Accepted, "rejected", resp.Rejected)
		return true
	}
	if _, err := a.spool.enqueue(results, sentAt); err != nil {
		// A broken spool must not stop a working network path: try the
		// direct post, but make the durability failure loud.
		journal.Error("agent", "spool results failed; posting directly", "err", err)
		resp, postErr := a.postResults(ctx, results, sentAt)
		if postErr != nil {
			journal.Error("agent", "post results failed", "err", postErr)
			return false
		}
		journal.Info("agent", "tick complete",
			"probes_run", len(due), "triggered", len(triggers), "results", len(results),
			"accepted", resp.Accepted, "rejected", resp.Rejected)
		return true
	}
	delivered, accepted, rejected, err := a.drainSpool(ctx)
	if err != nil {
		journal.Error("agent", "deliver results failed (left in spool)", "err", err,
			"probes_run", len(due), "triggered", len(triggers), "results", len(results), "delivered", delivered)
		return true
	}
	if delivered == 0 {
		journal.Warn("agent", "results queued in spool; delivery deferred",
			"probes_run", len(due), "triggered", len(triggers), "results", len(results))
		return true
	}
	journal.Info("agent", "tick complete",
		"probes_run", len(due), "triggered", len(triggers), "results", len(results),
		"delivered", delivered, "accepted", accepted, "rejected", rejected)
	return true
}

// checkJob is one (probe, seq) check due to run this tick, before
// runChecks has split it between per-check (Checker) and batched
// (BatchChecker) dispatch -- the common shape executeProbes and
// executeTriggers both reduce down to, since batching only cares
// about which prober each check belongs to, not why it's due.
type checkJob struct {
	pr    wire.ProbeSnapshot
	runID string
	seq   int
}

// executeTriggers runs each drained pendingTrigger's probe immediately,
// under the server-issued RunID it carries (not a freshly minted one --
// see wire.Event's own comment on RunID) so every node acting on the
// same trigger reports back correlated under that one id. Triggers
// carry their own snapshot, so one fired just before a probe was
// removed still has a runnable definition.
func (a *agent) executeTriggers(ctx context.Context, triggers []pendingTrigger) []wire.Result {
	if len(triggers) == 0 {
		return nil
	}
	var jobs []checkJob
	for _, t := range triggers {
		pr := t.Probe
		if pr.ID == "" {
			var ok bool
			pr, ok = a.cache.get(t.ProbeID)
			if !ok {
				continue
			}
		}
		count := a.probeJobCount(pr.ProbeCount)
		for seq := 1; seq <= count; seq++ {
			jobs = append(jobs, checkJob{pr: pr, runID: t.RunID, seq: seq})
		}
	}
	return a.runChecks(ctx, jobs)
}

func (a *agent) probeJobCount(raw int) int {
	if raw < 1 {
		return 1
	}
	if a.maxJobsPerProbe > 0 && raw > a.maxJobsPerProbe {
		return a.maxJobsPerProbe
	}
	return raw
}

// executeProbes runs every check of every due probe, bounded by a
// semaphore sized to a.concurrency -- see runChecks for how a prober
// backed by a pooled module (probe.BatchChecker) is grouped into
// CheckBatch calls instead of one goroutine per check.
func (a *agent) executeProbes(ctx context.Context, due []wire.ProbeSnapshot) []wire.Result {
	var jobs []checkJob
	for _, pr := range due {
		runID := newRunID()
		count := a.probeJobCount(pr.ProbeCount)
		for seq := 1; seq <= count; seq++ {
			jobs = append(jobs, checkJob{pr: pr, runID: runID, seq: seq})
		}
	}
	return a.runChecks(ctx, jobs)
}

// runChecks dispatches jobs, bounded by a semaphore sized to
// a.concurrency. Deliberately a single flat pool, no split between
// I/O-wait and CPU-bound stages -- see README.md's scheduler notes for
// the two-tier semaphore this should grow into once real load numbers
// justify it.
//
// Jobs are first grouped by prober name wherever that prober's Checker
// implements probe.BatchChecker (a pooled module, see internal/module's
// PoolChecker) -- each such group becomes a single CheckBatch call,
// occupying one semaphore slot for however many jobs it covers, rather
// than one slot (and one subprocess) per job. This is the only place
// pooling changes anything for the scheduler: everything else about a
// batched prober's jobs (how they got marked due, how their results
// get reported) is identical to any other check. A prober whose
// Checker doesn't implement it -- every native/action module, and any
// run.command module without a pool: block -- still gets exactly one
// goroutine per job, unchanged from before pooling existed.
func (a *agent) runChecks(ctx context.Context, jobs []checkJob) []wire.Result {
	if len(jobs) == 0 {
		return nil
	}
	sem := make(chan struct{}, a.concurrency)
	var mu sync.Mutex
	var results []wire.Result
	var wg sync.WaitGroup

	batches := map[string][]checkJob{}
	var singles []checkJob
	for _, j := range jobs {
		if checker, ok := a.reg.Get(j.pr.Prober); ok {
			if _, ok := checker.(probe.BatchChecker); ok {
				batches[j.pr.Prober] = append(batches[j.pr.Prober], j)
				continue
			}
		}
		singles = append(singles, j)
	}

	for prober, batchJobs := range batches {
		wg.Add(1)
		sem <- struct{}{}
		go func(prober string, batchJobs []checkJob) {
			defer wg.Done()
			defer func() { <-sem }()
			r := a.runCheckBatch(ctx, prober, batchJobs)
			mu.Lock()
			results = append(results, r...)
			mu.Unlock()
		}(prober, batchJobs)
	}

	for _, j := range singles {
		wg.Add(1)
		// Deliberately spawned unconditionally, without taking sem
		// first -- runCheck's own destgate.Wait can block a while if
		// this job's destination is busy, and a goroutine parked there
		// is cheap, but a goroutine parked there *while holding a
		// semaphore slot* would starve every other check waiting on a
		// totally unrelated destination for that same slot. See
		// runCheck's own comment.
		go func(j checkJob) {
			defer wg.Done()
			r, ok := a.runCheck(ctx, sem, j.pr, j.runID, j.seq)
			if !ok {
				return // never actually ran -- see runCheck's own comment
			}
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		}(j)
	}
	wg.Wait()
	return results
}

// runCheck waits for this check's own destination to be clear
// (destgate.Wait), *then* takes a concurrency slot from sem, then
// actually runs the check -- in that order. Taking sem before waiting
// would let one busy destination's queue of blocked checks exhaust
// the whole fleet's concurrency budget, starving checks against
// destinations that aren't busy at all; see runChecks' own comment on
// its singles loop.
//
// The wait is deliberately its own budget (destgate's own configured
// maxWait), not this check's own timeout_ms -- confirmed in
// production that a heavily-shared destination (dozens of probes
// resolving to one physical host) with a short timeout_ms meant only
// the very first probe per floor window could ever get a real
// attempt in, and every other one failed waiting, every cycle,
// permanently, not as an occasional/transient thing. Once the wait
// clears, the check gets a full, fresh timeout_ms-scoped budget of
// its own to actually run in -- ctx is only wrapped with
// context.WithTimeout after Wait returns, not before.
//
// Returns ok=false when the wait itself never cleared -- deliberately
// not reported as a failure at all (radar-node only reports when it
// has real data), not just tagged specially, so this never shows up
// as a misleading down-tick for a proxy that was simply never asked
// to answer this cycle. Logged locally so it's still visible for our
// own operational debugging, just never sent on.
func (a *agent) runCheck(ctx context.Context, sem chan struct{}, pr wire.ProbeSnapshot, runID string, seq int) (wire.Result, bool) {
	opts := optionsFor(pr, seq)

	checker, ok := a.reg.Get(pr.Prober)
	var r probe.Result
	if !ok {
		r = probe.Fail(pr.Prober, pr.Target, seq, fmt.Errorf("unknown prober %q", pr.Prober))
	} else if err := destgate.Wait(ctx, opts.Destination); err != nil {
		journal.Warn("agent", "destination never cleared, not reporting", "prober", pr.Prober, "probe_id", pr.ID, "seq", seq, "destination", opts.Destination, "err", err)
		return wire.Result{}, false
	} else {
		checkCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
		sem <- struct{}{}
		r = checker.Check(checkCtx, opts)
		<-sem
		cancel()
	}

	return wire.Result{
		RunID:      runID,
		ProbeID:    pr.ID,
		Result:     r,
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}, true
}

// runCheckBatch is runCheck's batched counterpart: every job in
// batchJobs shares prober (guaranteed by runChecks' own grouping), so
// they're built into one probe.Options slice and run through a single
// CheckBatch call rather than one Check call apiece. a.reg.Get is
// guaranteed to succeed and to implement probe.BatchChecker here --
// runChecks only ever forms a group after confirming both.
func (a *agent) runCheckBatch(ctx context.Context, prober string, batchJobs []checkJob) []wire.Result {
	checker, _ := a.reg.Get(prober)
	batchChecker := checker.(probe.BatchChecker)

	opts := make([]probe.Options, len(batchJobs))
	for i, j := range batchJobs {
		opts[i] = optionsFor(j.pr, j.seq)
	}
	checkResults := batchChecker.CheckBatch(ctx, opts)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	out := make([]wire.Result, 0, len(batchJobs))
	for i, j := range batchJobs {
		if checkResults[i].Skip {
			// Never actually ran (see probe.Result.Skip's own doc
			// comment, and internal/module/pool.go's testJob, the only
			// place inside a pooled Checker that sets it). internal/
			// module never logs anything itself (see testJob's own
			// comment), so this is where that gets surfaced -- same
			// message shape as runCheck's own non-pooled counterpart,
			// and equally not reported on at all.
			journal.Warn("agent", "destination never cleared, not reporting", "prober", j.pr.Prober, "probe_id", j.pr.ID, "seq", j.seq)
			continue
		}
		out = append(out, wire.Result{
			RunID:      j.runID,
			ProbeID:    j.pr.ID,
			Result:     checkResults[i],
			ObservedAt: now,
		})
	}
	return out
}

// optionsFor builds the probe.Options a due probe's seq'th check runs
// with -- shared by runCheck and runCheckBatch so a batched and
// unbatched check of the same prober can never see a different
// timeout default.
func optionsFor(pr wire.ProbeSnapshot, seq int) probe.Options {
	timeout := time.Duration(pr.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return probe.Options{
		Target:      pr.Target,
		Timeout:     timeout,
		Seq:         seq,
		Params:      pr.Params,
		Destination: destinationFor(pr),
	}
}

// destinationFor is destgate's rate-limiting key for pr -- see
// probe.Options.Destination's own doc comment. Target is already
// exactly right for a direct tcp/http/dns/icmp check (no indirection
// -- confirmed by reading each one's own Checker), and is used as-is
// for every prober this doesn't special-case, including wireguard/
// openvpn: their own Target is a through-tunnel probe value too, not
// their real endpoint, but that endpoint lives in raw wg-quick/.ovpn
// text inside Params with no JSON path to it -- a real text parser,
// not attempted here (see the plan this shipped under).
func destinationFor(pr wire.ProbeSnapshot) string {
	if pr.Prober == "xray" {
		if d, ok := xrayDestination(pr.Params); ok {
			return d
		}
	}
	return pr.Target
}

// xrayDestination pulls the real proxy server address:port out of an
// xray probe's own Params -- unlike Target (always the fixed
// connectivity-test constant a discovered proxy is created with, see
// radar-api's lib/subscriptionParse.ts), this is the thing actually
// being dialed. Mirrors
// xray-pool-build-config.sh's own vnext-or-servers fallback (vless/
// vmess vs. trojan-shaped configs). Returns ok=false on any shape
// mismatch rather than panicking or erroring -- an unrecognized/
// malformed config just falls back to destinationFor's own Target
// default, no worse than not having this at all.
func xrayDestination(params map[string]any) (string, bool) {
	config, _ := params["config"].(map[string]any)
	outbounds, _ := config["outbounds"].([]any)
	if len(outbounds) == 0 {
		return "", false
	}
	outbound, _ := outbounds[0].(map[string]any)
	settings, _ := outbound["settings"].(map[string]any)
	endpoints, _ := settings["vnext"].([]any)
	if len(endpoints) == 0 {
		endpoints, _ = settings["servers"].([]any)
	}
	if len(endpoints) == 0 {
		return "", false
	}
	endpoint, _ := endpoints[0].(map[string]any)
	address, ok := endpoint["address"].(string)
	if !ok || address == "" {
		return "", false
	}
	port := endpoint["port"]
	return fmt.Sprintf("%s:%v", address, port), true
}

func newBatchID() string {
	return "batch_" + randomHex(12)
}

func newRunID() string {
	return "run_" + randomHex(12)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
