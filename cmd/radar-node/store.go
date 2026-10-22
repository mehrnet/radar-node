package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/mehrnet/radar-node/internal/store"
	"github.com/mehrnet/radar-node/internal/wire"
)

func runStore(args []string) error {
	if len(args) == 0 {
		return errors.New("store: expected stats, maintain, rebuild, or replay")
	}
	switch args[0] {
	case "stats":
		return runStoreStats(args[1:])
	case "maintain":
		return runStoreMaintain(args[1:])
	case "rebuild":
		return runStoreRebuild(args[1:])
	case "replay":
		return runStoreReplay(args[1:])
	default:
		return fmt.Errorf("store: unknown subcommand %q", args[0])
	}
}

type storeFlags struct {
	path   string
	origin string
}

func storeFlagSet(name string) (*flag.FlagSet, *storeFlags) {
	flags := &storeFlags{}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&flags.path, "db", "", "")
	fs.StringVar(&flags.origin, "origin", "", "")
	fs.SetOutput(os.Stderr)
	return fs, flags
}

func (f *storeFlags) open() (*store.Store, error) {
	if f.path == "" {
		return nil, errors.New("--db is required")
	}
	return store.Open(f.path, store.Options{Origin: f.origin})
}

func runStoreStats(args []string) error {
	fs, flags := storeFlagSet("store stats")
	if err := fs.Parse(args); err != nil {
		return err
	}
	local, err := flags.open()
	if err != nil {
		return err
	}
	defer local.Close()

	stats, err := local.Stats(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("path: %s\norigin: %s\njournal: %s\ndb_bytes: %d\nwal_bytes: %d\npages: %d (free %d)\nresults: %d (delivered %d, pending %d, rejected %d)\nprobes: %d\npending_triggers: %d\nrollups: %d\n",
		local.Path(), local.Origin(), stats.JournalMode, stats.DBBytes, stats.WALBytes,
		stats.PageCount, stats.FreePages, stats.Results, stats.DeliveredResults,
		stats.PendingResults, stats.RejectedResults, stats.Probes, stats.PendingTriggers, stats.Rollups)
	fmt.Printf("oldest: %s\nnewest: %s\n", formatMillis(stats.OldestAtMS), formatMillis(stats.NewestAtMS))
	return nil
}

func runStoreMaintain(args []string) error {
	fs, flags := storeFlagSet("store maintain")
	retention := fs.Duration("retention", store.DefaultRetention, "")
	maxBytes := fs.Int64("max-bytes", store.DefaultMaxBytes, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *retention < 0 {
		return errors.New("--retention must not be negative")
	}
	if *maxBytes < 0 {
		return errors.New("--max-bytes must not be negative")
	}
	local, err := flags.open()
	if err != nil {
		return err
	}
	defer local.Close()

	stats, err := local.Maintain(context.Background(), store.MaintainOptions{Retention: *retention, MaxBytes: *maxBytes})
	if err != nil {
		return err
	}
	fmt.Printf("deleted_rows: %d\ndeleted_rollups: %d\nrolled_up_results: %d\npruned_triggers: %d\nvacuumed_pages: %d\nlive_bytes: %d\ndb_bytes: %d\nwal_bytes: %d\nover_budget: %t\n",
		stats.DeletedRows, stats.DeletedRollups, stats.RolledUpResults, stats.PrunedTriggers,
		stats.VacuumedPages, stats.LiveBytes, stats.DBBytes, stats.WALBytes, stats.OverBudget)
	return nil
}

func runStoreRebuild(args []string) error {
	fs, flags := storeFlagSet("store rebuild")
	if err := fs.Parse(args); err != nil {
		return err
	}
	local, err := flags.open()
	if err != nil {
		return err
	}
	defer local.Close()
	if err := local.Rebuild(context.Background()); err != nil {
		return err
	}
	fmt.Printf("rebuilt: %s\n", local.Path())
	return nil
}

func runStoreReplay(args []string) error {
	fs, flags := storeFlagSet("store replay")
	after := fs.Int64("after", 0, "")
	limit := fs.Int("limit", 1000, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *limit < 1 {
		return errors.New("--limit must be positive")
	}
	local, err := flags.open()
	if err != nil {
		return err
	}
	defer local.Close()

	encoder := json.NewEncoder(os.Stdout)
	next, err := local.Replay(context.Background(), *after, *limit, func(event store.Event) error {
		line := struct {
			Seq       int64           `json:"seq"`
			Kind      string          `json:"kind"`
			Origin    string          `json:"origin"`
			OriginSeq int64           `json:"origin_seq"`
			AtMS      int64           `json:"at_ms"`
			Result    *wire.Result    `json:"result,omitempty"`
			Payload   json.RawMessage `json:"payload,omitempty"`
		}{
			Seq: event.Seq, Kind: event.Kind, Origin: event.Origin,
			OriginSeq: event.OriginSeq, AtMS: event.AtMS, Result: event.Result,
			Payload: json.RawMessage(event.Payload),
		}
		return encoder.Encode(line)
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "cursor: %d\n", next)
	return nil
}

func formatMillis(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}
