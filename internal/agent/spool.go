package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mehrnet/radar-node/internal/journal"
	"github.com/mehrnet/radar-node/internal/wire"
)

const (
	defaultSpoolMaxBytes int64 = 16 << 20
	spoolDrainBudget           = 20 * time.Second
	spoolRetryInitial          = 5 * time.Second
	spoolRetryMax              = time.Minute
)

// spoolBatch is one results submission persisted before it is sent.
// SentAt is preserved so a replayed batch keeps its original observed
// timing; BatchID is deliberately minted fresh on every attempt.
type spoolBatch struct {
	Version int           `json:"version"`
	SentAt  string        `json:"sent_at"`
	Results []wire.Result `json:"results"`
}

type spoolEntry struct {
	path string
	size int64
}

type resultSpool struct {
	dir      string
	maxBytes int64
}

// newResultSpool creates the spool directory. A nil spool means the
// feature is disabled; an empty dir is the normal way to express that.
func newResultSpool(dir string, maxBytes int64) (*resultSpool, error) {
	if dir == "" {
		return nil, nil
	}
	if maxBytes <= 0 {
		maxBytes = defaultSpoolMaxBytes
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create result spool %s: %w", dir, err)
	}
	// MkdirAll leaves an existing directory's mode alone; a spool
	// path that predates this install must still be private.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure result spool %s: %w", dir, err)
	}
	return &resultSpool{dir: dir, maxBytes: maxBytes}, nil
}

// enqueue writes one batch atomically, then evicts the oldest files if
// the directory is over its byte budget. The newest file is always
// kept even when it alone exceeds the budget -- dropping the only copy
// of the results just produced would defeat the point of the spool.
func (s *resultSpool) enqueue(results []wire.Result, sentAt string) (string, error) {
	if len(results) == 0 {
		return "", nil
	}
	payload, err := json.Marshal(spoolBatch{Version: 1, SentAt: sentAt, Results: results})
	if err != nil {
		return "", fmt.Errorf("encode spool batch: %w", err)
	}
	name := fmt.Sprintf("batch-%020d-%s.json", time.Now().UnixNano(), randomHex(4))
	final := filepath.Join(s.dir, name)

	tmp, err := os.CreateTemp(s.dir, ".batch-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create spool file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(append(payload, '\n')); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("write spool file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("sync spool file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("close spool file: %w", err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		cleanup()
		return "", fmt.Errorf("publish spool file: %w", err)
	}
	if removed := s.prune(); removed > 0 {
		journal.Warn("agent", "result spool evicted oldest batches", "evicted", removed, "max_bytes", s.maxBytes)
	}
	return final, nil
}

// pending returns the spool's batches oldest-first. Only completed
// .json files count; temp files from an interrupted enqueue and
// anything else in the directory are ignored.
func (s *resultSpool) pending() ([]spoolEntry, error) {
	dirEntries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	entries := make([]spoolEntry, 0, len(dirEntries))
	for _, entry := range dirEntries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		entries = append(entries, spoolEntry{path: filepath.Join(s.dir, entry.Name()), size: info.Size()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	return entries, nil
}

func (s *resultSpool) read(path string) (spoolBatch, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return spoolBatch{}, err
	}
	var batch spoolBatch
	if err := json.Unmarshal(payload, &batch); err != nil {
		return spoolBatch{}, err
	}
	if batch.Version != 1 {
		return spoolBatch{}, fmt.Errorf("unsupported spool version %d", batch.Version)
	}
	if len(batch.Results) == 0 {
		return spoolBatch{}, fmt.Errorf("empty spool batch")
	}
	return batch, nil
}

func (s *resultSpool) remove(path string) {
	_ = os.Remove(path)
}

func (s *resultSpool) prune() int {
	entries, err := s.pending()
	if err != nil {
		return 0
	}
	var total int64
	for _, entry := range entries {
		total += entry.size
	}
	removed := 0
	for _, entry := range entries {
		if total <= s.maxBytes || len(entries)-removed <= 1 {
			break
		}
		if os.Remove(entry.path) == nil {
			total -= entry.size
			removed++
		}
	}
	return removed
}

// postResults sends one batch. It exists separately from drainSpool so
// the direct (spool-disabled) path and the replay path share the same
// timeout and request shape.
func (a *agent) postResults(ctx context.Context, results []wire.Result, sentAt string) (*wire.ResultsResponse, error) {
	reportCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if sentAt == "" {
		sentAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return a.client.PostResults(reportCtx, wire.ResultsRequest{
		NodeID:  a.nodeID,
		BatchID: newBatchID(),
		SentAt:  sentAt,
		Results: results,
	})
}

// drainSpool posts pending batches oldest-first until the queue is
// empty, the drain budget is exhausted, or one POST fails. A failure
// starts an exponential retry backoff so a down API does not get a
// request every scheduler tick; new results keep being enqueued while
// that backoff runs. Returns the number of results delivered and the
// server's accepted/rejected totals.
func (a *agent) drainSpool(ctx context.Context) (delivered, accepted, rejected int, err error) {
	if a.spool == nil {
		return 0, 0, 0, nil
	}
	if !a.spoolNextAttempt.IsZero() && time.Now().Before(a.spoolNextAttempt) {
		return 0, 0, 0, nil
	}
	entries, err := a.spool.pending()
	if err != nil {
		return 0, 0, 0, err
	}
	if len(entries) == 0 {
		return 0, 0, 0, nil
	}

	drainCtx, cancel := context.WithTimeout(ctx, spoolDrainBudget)
	defer cancel()
	for _, entry := range entries {
		if drainCtx.Err() != nil {
			break
		}
		batch, readErr := a.spool.read(entry.path)
		if readErr != nil {
			journal.Warn("agent", "discarding unreadable spooled results", "path", entry.path, "err", readErr)
			a.spool.remove(entry.path)
			continue
		}
		resp, postErr := a.postResults(drainCtx, batch.Results, batch.SentAt)
		if postErr != nil {
			a.spoolBackoff = nextSpoolBackoff(a.spoolBackoff)
			a.spoolNextAttempt = time.Now().Add(a.spoolBackoff)
			return delivered, accepted, rejected, postErr
		}
		a.spool.remove(entry.path)
		delivered += len(batch.Results)
		accepted += resp.Accepted
		rejected += resp.Rejected
		a.spoolBackoff = 0
		a.spoolNextAttempt = time.Time{}
	}
	return delivered, accepted, rejected, nil
}

func nextSpoolBackoff(previous time.Duration) time.Duration {
	if previous <= 0 {
		return spoolRetryInitial
	}
	next := previous * 2
	if next > spoolRetryMax {
		return spoolRetryMax
	}
	return next
}
