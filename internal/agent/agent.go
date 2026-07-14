// Package agent implements the `radar-node agent` loop. Unlike
// the original design, the server never tells this agent what's due
// -- it syncs job *definitions* incrementally (folded into
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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mehrnet/radar-node/internal/apiclient"
	"github.com/mehrnet/radar-node/internal/probe"
	"github.com/mehrnet/radar-node/internal/registry"
	"github.com/mehrnet/radar-node/internal/wire"
)

const AgentVersion = "0.2.0-dev"

type Config struct {
	APIURL   string
	APIKey   string // "node_id:secret" -- also the bearer token as-is
	ProxyURL string
	// SchedulerTick is how often the local scheduler checks its
	// cached jobs for due-ness. This governs real-world scheduling
	// granularity (a 30s-interval job can fire up to one tick late),
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
// which also carries job-definition sync, see heartbeatLoop --
// and scheduler) share, so neither needs a long, overlapping
// positional parameter list just to thread the same handful of
// dependencies through -- client/nodeID/reg in particular were
// previously repeated across nearly every function signature in this
// package.
type agent struct {
	client      *apiclient.Client
	nodeID      string
	reg         registry.Registry
	cache       *jobCache
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

	a := &agent{
		client:      client,
		nodeID:      nodeID,
		reg:         reg,
		cache:       newJobCache(),
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

// heartbeatLoop also carries job-definition sync and clock
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
			AgentVersion: AgentVersion,
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
			log.Printf("agent: synced %d job event(s)", len(resp.Events))
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
			a.runDueJobs(ctx)
		}
	}
}

func (a *agent) runDueJobs(ctx context.Context) {
	now := a.clock.now()
	due := a.cache.dueJobs(now)
	if len(due) == 0 {
		return
	}

	// Claim immediately, before executing anything -- so a fast
	// subsequent tick can't re-select the same job while this run is
	// still in flight. If reporting later fails, this occurrence is
	// simply lost (an interval job is due again next interval); that
	// is the accepted failure mode, not silent double-execution.
	for _, job := range due {
		a.cache.markRun(job.ID, now)
	}

	results := a.executeJobs(ctx, due)
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
	log.Printf("agent: tick complete: %d job(s) run, %d results, %d accepted, %d rejected",
		len(due), len(results), resp.Accepted, resp.Rejected)
}

// executeJobs runs every probe of every due job concurrently, bounded
// by a semaphore sized to a.concurrency. Deliberately a single flat
// pool, no split between I/O-wait and CPU-bound stages -- see
// README.md's scheduler notes for the two-tier semaphore this
// should grow into once real load numbers justify it.
func (a *agent) executeJobs(ctx context.Context, due []wire.JobSnapshot) []wire.Result {
	sem := make(chan struct{}, a.concurrency)
	var mu sync.Mutex
	var results []wire.Result
	var wg sync.WaitGroup

	for _, job := range due {
		runID := newRunID()
		count := job.ProbeCount
		if count < 1 {
			count = 1
		}
		for seq := 1; seq <= count; seq++ {
			wg.Add(1)
			sem <- struct{}{}
			go func(job wire.JobSnapshot, runID string, seq int) {
				defer wg.Done()
				defer func() { <-sem }()
				r := a.runProbe(ctx, job, runID, seq)
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}(job, runID, seq)
		}
	}
	wg.Wait()
	return results
}

func (a *agent) runProbe(ctx context.Context, job wire.JobSnapshot, runID string, seq int) wire.Result {
	mode := probe.Mode(job.Mode)
	if mode == "" {
		mode = probe.ModeWarm
	}

	checker, ok := a.reg.Get(job.Prober)
	var r probe.Result
	if !ok {
		r = probe.Fail(job.Prober, job.Target, mode, seq, fmt.Errorf("unknown prober %q", job.Prober))
	} else {
		timeout := time.Duration(job.TimeoutMs) * time.Millisecond
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		r = checker.Check(ctx, probe.Options{
			Target:  job.Target,
			Timeout: timeout,
			Mode:    mode,
			Seq:     seq,
			Params:  job.Params,
		})
	}

	return wire.Result{
		RunID:      runID,
		JobID:      job.ID,
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
