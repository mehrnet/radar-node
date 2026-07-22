// Package agent implements the `radar-node agent` loop. Unlike
// the original design, the server never tells this agent what's due
// -- it syncs probe *definitions* incrementally (folded into
// POST /v1/nodes/heartbeat's since_seq/events, see heartbeatLoop)
// into a local cache, decides for itself when something is due using
// its own clock-corrected notion of "now" (see clock.go), runs it
// through the same Checkers the `probe` subcommand uses, and reports
// results back keyed by a locally-generated run id. See
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
	"github.com/mehrnet/radar-node/internal/probe"
	"github.com/mehrnet/radar-node/internal/registry"
	"github.com/mehrnet/radar-node/internal/wire"
)

// installScriptURL is the same one-liner README.md documents for a
// fresh install -- self-update re-runs it verbatim (same node_id/
// api_key/api_url/proxy this process itself was started with), which
// is what lets it reuse install.sh's own stop-download-replace-
// restart sequence instead of this process trying to replace its own
// running binary directly.
const installScriptURL = "https://raw.githubusercontent.com/mehrnet/radar-node/main/install.sh"

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
	// ModulesDir loads probers from *.yaml/*.yml files there, on top
	// of (and overriding by name) the embedded default fixtures
	// (tcp/udp/dns/icmp/http/https/system). Empty means defaults-only.
	ModulesDir string
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
	if err := reg.LoadModules(cfg.ModulesDir); err != nil {
		return err
	}

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

// heartbeatLoop also carries probe-definition sync and clock
// calibration -- folded in from what used to be a separate
// eventsSyncLoop polling GET /v1/nodes/events on its own timer. Both
// loops fired on a fixed interval regardless of activity and each
// paid its own request/auth round trip; since a heartbeat already
// happens this often, there's no freshness lost by piggybacking
// since_seq/events on it instead, and it halves the number of always-
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
			Probers:      proberHashes,
			SinceSeq:     a.cache.lastKnownSeq(),
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
		if len(resp.Events) > 0 {
			a.cache.applyEvents(resp.Events)
			log.Printf("agent: synced %d probe event(s)", len(resp.Events))
		}
		switch resp.Command {
		case "update":
			a.selfUpdate()
		case "delete":
			a.handleDeleteCommand()
		}
		if len(resp.ModuleActions) > 0 {
			a.applyModuleActions(resp.ModuleActions)
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

func (a *agent) selfUpdate() {
	a.reinstall()
}

// moduleActionFlags maps a wire-level "install_xray"/"remove_wireguard"
// style action name to install.sh's matching --install-xray/--remove-
// wireguard flag. Unrecognized entries are dropped rather than failing
// the whole batch -- radar-api validates this set already (see its
// nodeModuleActionsSchema), so an entry that doesn't map here can only
// mean a newer server introduced an action this older agent build
// doesn't know about yet, not a real error.
func moduleActionFlags(actions []string) []string {
	flags := make([]string, 0, len(actions))
	for _, action := range actions {
		flag, ok := strings.CutPrefix(action, "install_")
		if ok {
			flags = append(flags, "--install-"+flag)
			continue
		}
		if flag, ok := strings.CutPrefix(action, "remove_"); ok {
			flags = append(flags, "--remove-"+flag)
		}
	}
	return flags
}

// applyModuleActions re-execs install.sh once with every bundled-
// engine flag this heartbeat's batch named, e.g. installing xray and
// removing wireguard together becomes one
// "--install-xray --remove-wireguard" re-run instead of two separate
// fire-once commands one click (and one full install.sh re-run) apart.
func (a *agent) applyModuleActions(actions []string) {
	flags := moduleActionFlags(actions)
	if len(flags) == 0 {
		return
	}
	a.reinstall(flags...)
}

// reinstall re-execs install.sh with this node's own existing
// node_id/api_key/api_url/proxy (exactly like a plain "update" does),
// plus whatever extra flags the request carries when it's really about
// bundled engine modules rather than radar-node's own version -- e.g.
// "--install-xray" for an "install_xray" action, one or more at once
// (see applyModuleActions). install.sh itself does the actual fetch/
// verify/place and the service restart that picks up a module just
// dropped into modules.d; this is only ever "re-run that same script,
// with some extra arguments than usual."
func (a *agent) reinstall(extraFlags ...string) {
	args := []string{"--node_id=" + a.nodeID, "--api_key=" + strings.TrimPrefix(a.apiKey, a.nodeID+":"), "--api_url=" + a.apiURL}
	if a.proxyURL != "" {
		args = append(args, "--proxy="+a.proxyURL)
	}
	args = append(args, extraFlags...)
	installCmd := fmt.Sprintf("curl -fsSL %s | sh -s -- %s", installScriptURL, strings.Join(args, " "))
	reason := "update requested"
	if len(extraFlags) > 0 {
		reason = strings.Join(extraFlags, " ") + " requested"
	}
	log.Printf("agent: %s -- re-running install script: %s", reason, installCmd)

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
	os.Exit(0)
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
	if len(due) == 0 {
		return
	}

	// Claim immediately, before executing anything -- so a fast
	// subsequent tick can't re-select the same probe while this run is
	// still in flight. If reporting later fails, this occurrence is
	// simply lost (an interval probe is due again next interval); that
	// is the accepted failure mode, not silent double-execution.
	for _, pr := range due {
		a.cache.markRun(pr.ID, now)
	}

	results := a.executeProbes(ctx, due)
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
	log.Printf("agent: tick complete: %d probe(s) run, %d results, %d accepted, %d rejected",
		len(due), len(results), resp.Accepted, resp.Rejected)
}

// executeProbes runs every check of every due probe concurrently,
// bounded by a semaphore sized to a.concurrency. Deliberately a single
// flat pool, no split between I/O-wait and CPU-bound stages -- see
// README.md's scheduler notes for the two-tier semaphore this
// should grow into once real load numbers justify it.
func (a *agent) executeProbes(ctx context.Context, due []wire.ProbeSnapshot) []wire.Result {
	sem := make(chan struct{}, a.concurrency)
	var mu sync.Mutex
	var results []wire.Result
	var wg sync.WaitGroup

	for _, pr := range due {
		runID := newRunID()
		count := pr.ProbeCount
		if count < 1 {
			count = 1
		}
		for seq := 1; seq <= count; seq++ {
			wg.Add(1)
			sem <- struct{}{}
			go func(pr wire.ProbeSnapshot, runID string, seq int) {
				defer wg.Done()
				defer func() { <-sem }()
				r := a.runCheck(ctx, pr, runID, seq)
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}(pr, runID, seq)
		}
	}
	wg.Wait()
	return results
}

func (a *agent) runCheck(ctx context.Context, pr wire.ProbeSnapshot, runID string, seq int) wire.Result {
	mode := probe.Mode(pr.Mode)
	if mode == "" {
		mode = probe.ModeWarm
	}

	checker, ok := a.reg.Get(pr.Prober)
	var r probe.Result
	if !ok {
		r = probe.Fail(pr.Prober, pr.Target, mode, seq, fmt.Errorf("unknown prober %q", pr.Prober))
	} else {
		timeout := time.Duration(pr.TimeoutMs) * time.Millisecond
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		r = checker.Check(ctx, probe.Options{
			Target:  pr.Target,
			Timeout: timeout,
			Mode:    mode,
			Seq:     seq,
			Params:  pr.Params,
		})
	}

	return wire.Result{
		RunID:      runID,
		ProbeID:    pr.ID,
		Result:     r,
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
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
