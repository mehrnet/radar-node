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
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mehrnet/radar-node/internal/apiclient"
	"github.com/mehrnet/radar-node/internal/destgate"
	"github.com/mehrnet/radar-node/internal/probe"
	"github.com/mehrnet/radar-node/internal/registry"
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
	Concurrency   int
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

// agent bundles everything the two concurrent loops (heartbeat --
// which also carries probe-definition sync, see heartbeatLoop --
// and scheduler) share, so neither needs a long, overlapping
// positional parameter list just to thread the same handful of
// dependencies through -- client/nodeID/reg in particular were
// previously repeated across nearly every function signature in this
// package.
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
	// node_status starts optimistic; the first heartbeat/results
	// response corrects it. An atomic.Value rather than a mutex so the
	// scheduler can gate execution on it with no lock contention.
	status atomic.Value
}

// Run blocks until ctx is cancelled, running the heartbeat and
// scheduler loops concurrently.
func Run(ctx context.Context, cfg Config) error {
	nodeID, _, ok := strings.Cut(cfg.APIKey, ":")
	if !ok || nodeID == "" {
		return fmt.Errorf("--api-key must be in node_id:secret form")
	}
	if cfg.SchedulerTick <= 0 {
		return fmt.Errorf("--scheduler-tick must be positive")
	}
	if cfg.Concurrency <= 0 {
		return fmt.Errorf("--concurrency must be positive")
	}

	client, err := apiclient.New(cfg.APIURL, cfg.APIKey, cfg.ProxyURL)
	if err != nil {
		return err
	}
	reg, err := registry.Default()
	if err != nil {
		return err
	}
	if err := reg.LoadModules(cfg.ModulesDir, cfg.ToolsDir); err != nil {
		return err
	}
	destgate.Configure(cfg.DestinationInterval, cfg.DestinationMaxWait)

	version := cfg.Version
	if version == "" {
		version = "dev"
	}
	a := &agent{
		client:      client,
		nodeID:      nodeID,
		apiKey:      cfg.APIKey,
		apiURL:      cfg.APIURL,
		proxyURL:    cfg.ProxyURL,
		version:     version,
		reg:         reg,
		cache:       newProbeCache(),
		clock:       &clockSync{},
		concurrency: cfg.Concurrency,
	}
	a.status.Store(wire.NodeStatusActive)

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
	wg.Wait()
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
		resp, err := a.client.Heartbeat(hbCtx, wire.HeartbeatRequest{
			NodeID:       a.nodeID,
			AgentVersion: a.version,
			OS:           runtime.GOOS,
			Arch:         runtime.GOARCH,
			Probers:      proberHashes,
			Modules:      a.reg.ModuleVersions(),
			ProbeHash:    a.cache.lastKnownHash(),
			SentAt:       sentAt.UTC().Format(time.RFC3339Nano),
		})
		return resp, sentAt, time.Now(), err
	}

	// beat sends the heartbeat and, if radar-api rejects it because it
	// doesn't recognize one or more of this node's current module
	// hashes, uploads exactly those named modules and retries once --
	// the common case (nothing changed since last time) never touches
	// the upload path at all.
	beat := func() {
		resp, sentAt, receivedAt, err := send()
		var rejected *apiclient.HeartbeatRejectedError
		if errors.As(err, &rejected) {
			if uploadErr := a.uploadMissingModules(ctx, rejected.Rejection.MissingProberIDs); uploadErr != nil {
				log.Printf("agent: upload modules: %v", uploadErr)
				return
			}
			resp, sentAt, receivedAt, err = send()
		}
		if err != nil {
			log.Printf("agent: heartbeat failed: %v", err)
			return
		}
		if resp.NodeStatus != "" {
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
			a.cache.replaceAssignment(resp.ProbeHash, resp.Probes)
			log.Printf("agent: synced probe assignment (%d probe(s))", len(resp.Probes))
		}
		if len(resp.Events) > 0 {
			a.cache.applyTriggeredEvents(resp.Events)
			log.Printf("agent: synced %d triggered event(s)", len(resp.Events))
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
// selfUpdateLogPath is where the detached installer's own stdout/
// stderr goes -- deliberately NOT this process's inherited os.Stdout.
// A systemd service's stdout is a journal stream tied to that unit's
// own lifecycle; once this process exits and the unit is marked
// inactive (which happens within about a second, well before the
// installer finishes downloading/replacing/restarting), writes from a
// process that merely inherited that fd can vanish from the journal
// entirely -- observed in practice as install.sh silently appearing to
// do nothing, with zero output anywhere, on every single attempt. A
// plain, independent file survives regardless of what happens to the
// parent unit.
const selfUpdateLogPath = "/tmp/radar-node-selfupdate.log"

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
		log.Printf("agent: could not ack pending action %s (%v) -- not acting on it this heartbeat", action.ID, err)
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
	args := []string{"--node_id=" + nodeID, "--api_key=" + apiKey, "--api_url=" + apiURL}
	curlProxyFlag := ""
	if proxyURL != "" {
		curlProxyFlag = "--proxy " + proxyURL + " "
		args = append(args, "--proxy="+proxyURL)
	}
	args = append(args, extraFlags...)
	return fmt.Sprintf(`set -e
_script=$(mktemp)
_attempt=1
while :; do
  if curl -fsSL %s%s -o "$_script"; then
    break
  fi
  if [ "$_attempt" -ge %d ]; then
    echo "radar-node: failed to fetch install.sh after %d attempts" >&2
    rm -f "$_script"
    exit 1
  fi
  echo "radar-node: install.sh fetch failed (attempt $_attempt/%d) -- retrying in $((_attempt * 2))s..." >&2
  sleep $((_attempt * 2))
  _attempt=$((_attempt + 1))
done
sh "$_script" %s
_status=$?
rm -f "$_script"
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
	log.Printf("agent: %s -- re-running install script", reason)

	cmd := selfUpdateCommand(installCmd)
	logFile, err := os.OpenFile(selfUpdateLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		log.Printf("agent: self-update: could not open %s (%v) -- falling back to this process's own stdout, which may not survive the restart that follows", selfUpdateLogPath, err)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	// Run, not Start -- deliberately blocks here, but only on the
	// *wrapper* (systemd-run confirming the transient unit actually
	// started, a sub-second round trip through PID1's D-Bus API, or a
	// plain child's own near-instant fork+exec on the non-systemd
	// fallback), never on install.sh's own full download/replace/
	// restart. Exiting via Start()+immediate os.Exit(0) raced this
	// confirmation: `systemd-run --scope` needs a moment to register
	// the new scope and migrate into it, and this process's own exit
	// (tearing down its cgroup, per selfUpdateCommand's own doc
	// comment) could kill systemd-run before that finished -- observed
	// in practice as install.sh never producing a single line of
	// output, on every attempt. Waiting for confirmation first removes
	// that race instead of shrinking it.
	if err := cmd.Run(); err != nil {
		log.Printf("agent: self-update: launching the installer failed: %v (see %s)", err, selfUpdateLogPath)
		return
	}
	log.Printf("agent: installer handed off successfully, logging to %s -- exiting so it can replace this process", selfUpdateLogPath)
	os.Exit(selfUpdateExitCode)
}

// selfUpdateCommand wraps installCmd in `systemd-run --unit=...` when
// available, instead of just running it as a plain child process.
// This matters specifically because this process is (usually) itself
// a systemd service: os/exec never moves a child into a new cgroup, so
// a plain child stays in *this* unit's cgroup, and systemd's default
// KillMode=control-group kills every process in that cgroup -- not
// just the main one -- the moment the unit is stopped/restarted, which
// is exactly what this function's own exit (via Restart=always)
// triggers immediately afterward. The result without this wrapper:
// the installer gets killed mid-download/replace before it ever
// upgrades the binary, and the service just restarts on the same old
// version it started with -- silently, since nothing here observes
// the installer's fate after Start().
//
// A real transient *unit* (`--unit=name`), not a `--scope`: a scope
// becomes a direct child of the invoking process and needs a moment to
// register itself and migrate into it over D-Bus, which raced this
// process's own exit in practice -- if this process's cgroup got torn
// down before that handshake finished, systemd-run itself was killed
// before it ever got to exec install.sh, with zero output anywhere to
// explain why. `--unit=` instead asks PID1 to create and start an
// independent unit directly; by default systemd-run blocks only until
// that start is *confirmed* (a fast round trip, not install.sh's full
// runtime -- see selfUpdate's use of Run() instead of Start()), and
// once confirmed the unit has no remaining relationship to this
// process or its cgroup at all, so there's no window left to race.
//
// `--user` is added when this process isn't running as root, mirroring
// install.sh's own root-vs-per-user service split. Falls back to a
// plain child on non-Linux (macOS/launchd doesn't tear down orphaned
// children this way) or if systemd-run isn't on PATH.
func selfUpdateCommand(installCmd string) *exec.Cmd {
	return selfUpdateCommandFor(runtime.GOOS, os.Geteuid(), os.Getpid(), exec.LookPath, installCmd)
}

// selfUpdateCommandFor is selfUpdateCommand's decision logic, factored
// out for testability -- goos/euid/pid/lookPath are the only real-
// world inputs it needs, so a test can exercise every branch (root vs
// user, systemd-run present vs absent, Linux vs not) without depending
// on the actual host it runs on.
func selfUpdateCommandFor(goos string, euid int, pid int, lookPath func(string) (string, error), installCmd string) *exec.Cmd {
	if goos == "linux" {
		if path, err := lookPath("systemd-run"); err == nil {
			unitName := fmt.Sprintf("radar-node-selfupdate-%d", pid)
			runArgs := []string{"--unit=" + unitName, "--quiet", "--collect"}
			if euid != 0 {
				runArgs = append(runArgs, "--user")
			}
			runArgs = append(runArgs, "sh", "-c", installCmd)
			return exec.Command(path, runArgs...)
		}
	}
	return exec.Command("sh", "-c", installCmd)
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
	log.Printf("agent: this node was deleted from radar -- stopping. To fully remove it from this machine, run:")
	log.Printf("  curl -fsSL %s | sh -s -- --uninstall", installScriptURL)
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
			if s, _ := a.status.Load().(string); s != wire.NodeStatusActive {
				continue
			}
			a.runDueProbes(ctx)
		}
	}
}

func (a *agent) runDueProbes(ctx context.Context) {
	now := a.clock.now()
	due := a.cache.dueProbes(now)
	triggers := a.cache.drainPendingTriggers()
	if len(due) == 0 && len(triggers) == 0 {
		return
	}

	// Claim immediately, before executing anything -- so a fast
	// subsequent tick can't re-select the same probe while this run is
	// still in flight. If reporting later fails, this occurrence is
	// simply lost (an interval probe is due again next interval); that
	// is the accepted failure mode, not silent double-execution.
	// Triggers don't touch lastRunAt at all -- they're independent of
	// due-ness bookkeeping, see applyTriggeredEvents.
	for _, pr := range due {
		a.cache.markRun(pr.ID, now)
	}

	results := a.executeProbes(ctx, due)
	results = append(results, a.executeTriggers(ctx, triggers)...)
	if len(results) == 0 {
		return
	}

	reportCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := a.client.PostResults(reportCtx, wire.ResultsRequest{
		NodeID:  a.nodeID,
		BatchID: newBatchID(),
		SentAt:  time.Now().UTC().Format(time.RFC3339Nano),
		Results: results,
	})
	if err != nil {
		log.Printf("agent: post results: %v", err)
		return
	}
	log.Printf("agent: tick complete: %d probe(s) run, %d triggered, %d results, %d accepted, %d rejected",
		len(due), len(triggers), len(results), resp.Accepted, resp.Rejected)
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
// same trigger reports back correlated under that one id. A trigger
// for a probe this node no longer has cached (removed between firing
// and this tick draining it) is silently skipped -- nothing to run.
func (a *agent) executeTriggers(ctx context.Context, triggers []pendingTrigger) []wire.Result {
	if len(triggers) == 0 {
		return nil
	}
	var jobs []checkJob
	for _, t := range triggers {
		pr, ok := a.cache.get(t.ProbeID)
		if !ok {
			continue
		}
		count := pr.ProbeCount
		if count < 1 {
			count = 1
		}
		for seq := 1; seq <= count; seq++ {
			jobs = append(jobs, checkJob{pr: pr, runID: t.RunID, seq: seq})
		}
	}
	return a.runChecks(ctx, jobs)
}

// executeProbes runs every check of every due probe, bounded by a
// semaphore sized to a.concurrency -- see runChecks for how a prober
// backed by a pooled module (probe.BatchChecker) is grouped into
// CheckBatch calls instead of one goroutine per check.
func (a *agent) executeProbes(ctx context.Context, due []wire.ProbeSnapshot) []wire.Result {
	var jobs []checkJob
	for _, pr := range due {
		runID := newRunID()
		count := pr.ProbeCount
		if count < 1 {
			count = 1
		}
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
		log.Printf("agent: %s check for probe %s (seq %d): destination %q never cleared, not reporting: %v", pr.Prober, pr.ID, seq, opts.Destination, err)
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
			log.Printf("agent: %s check for probe %s (seq %d): destination never cleared, not reporting", j.pr.Prober, j.pr.ID, j.seq)
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
