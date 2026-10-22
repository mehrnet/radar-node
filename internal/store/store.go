// Package store is radar-node's local durable state: a compact SQLite
// journal with materialized probe and trigger state on top. Results are
// stored once as typed columns (not duplicated as JSON) so a node on a
// small flash device can retain history without paying twice for it.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mehrnet/radar-node/internal/probe"
	"github.com/mehrnet/radar-node/internal/wire"
	_ "modernc.org/sqlite"
)

const (
	defaultBusyTimeoutMS = 5000
	defaultCacheKiB      = 2048
	// A checkpoint every 512 pages keeps the WAL around 2 MiB on a
	// flash-constrained node instead of the 4 MiB default.
	defaultWALCheckpointPages = 512
	defaultJournalSizeLimit   = 4 << 20
)

// Options configures one node-local database.
type Options struct {
	// Origin is the stable node identity that authored local rows.
	// Reopening the same file under a different origin is refused so
	// a copied database can never silently mix two nodes' histories.
	Origin string
	// Sync is "normal" (default) or "full". Normal survives process
	// crashes; full additionally fsyncs each transaction for power-loss
	// durability. Local-only deployments should use full.
	Sync string
}

type Store struct {
	db          *sql.DB
	path        string
	origin      string
	journalMode string
}

// Open creates or opens a database, applies the schema and pragmas, and
// binds the file to opts.Origin.
func Open(path string, opts Options) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("store: database path is required")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve database path %s: %w", path, err)
	}
	path = absPath
	if strings.TrimSpace(opts.Origin) == "" {
		// Diagnostics may open an existing database without knowing its
		// node identity; a new file still needs one to author events.
		if info, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) || (statErr == nil && info.Size() == 0) {
			return nil, errors.New("store: origin is required for a new database")
		}
	}
	syncMode := strings.ToLower(strings.TrimSpace(opts.Sync))
	if syncMode == "" {
		syncMode = "normal"
	}
	if syncMode != "normal" && syncMode != "full" {
		return nil, fmt.Errorf("store: unknown sync mode %q (want normal or full)", opts.Sync)
	}

	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: create directory %s: %w", dir, err)
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: create database %s: %w", path, err)
	}
	_ = file.Close()
	_ = os.Chmod(path, 0o600)

	db, err := sql.Open("sqlite", dsn(path, syncMode))
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	// One connection serializes writes and avoids SQLITE_BUSY between
	// this process's own goroutines. A separate read-only handle can be
	// added later if the local UI needs concurrent long reads.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	if err := initSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	s := &Store{db: db, path: path}
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&s.journalMode); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: read journal mode: %w", err)
	}
	if err := s.initMeta(ctx, opts.Origin); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func dsn(path, syncMode string) string {
	pragmas := []string{
		fmt.Sprintf("busy_timeout(%d)", defaultBusyTimeoutMS),
		"journal_mode(WAL)",
		"synchronous(" + strings.ToUpper(syncMode) + ")",
		"temp_store(MEMORY)",
		"auto_vacuum(INCREMENTAL)",
		fmt.Sprintf("wal_autocheckpoint(%d)", defaultWALCheckpointPages),
		fmt.Sprintf("journal_size_limit(%d)", defaultJournalSizeLimit),
		fmt.Sprintf("cache_size(-%d)", defaultCacheKiB),
		"secure_delete(OFF)",
	}
	parts := make([]string, 0, len(pragmas))
	for _, pragma := range pragmas {
		parts = append(parts, "_pragma="+pragma)
	}
	slashPath := filepath.ToSlash(path)
	if runtime.GOOS == "windows" && !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	u := url.URL{Scheme: "file", Path: slashPath}
	u.RawQuery = strings.Join(parts, "&")
	return u.String()
}

func initSchema(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	switch {
	case version == schemaVersion:
		return nil
	case version == 1:
		return migrateV1ToV2(ctx, db)
	case version == 0:
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: begin schema migration: %w", err)
		}
		if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: create schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: record schema version: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit schema: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("store: database schema %d is newer than this binary supports (%d)", version, schemaVersion)
	}
}

// migrateV1ToV2 repoints the pending-trigger queue at journal rowids.
// v1 stored the per-origin sequence there, but maintenance protects
// pending trigger events by journal rowid; duplicate results create
// gaps between the two, so a live trigger's event could be pruned.
func migrateV1ToV2(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin schema migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		UPDATE pending_triggers
		   SET event_seq = (
		       SELECT j.seq FROM journal j
		        WHERE j.kind = 'trigger'
		          AND j.probe_id = pending_triggers.probe_id
		          AND j.run_id = pending_triggers.run_id
		        ORDER BY j.seq DESC
		        LIMIT 1
		   )
		 WHERE EXISTS (
		       SELECT 1 FROM journal j
		        WHERE j.kind = 'trigger'
		          AND j.probe_id = pending_triggers.probe_id
		          AND j.run_id = pending_triggers.run_id
		 )`); err != nil {
		return fmt.Errorf("store: migrate pending trigger seqs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("store: record schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit schema migration: %w", err)
	}
	return nil
}

func (s *Store) initMeta(ctx context.Context, requestedOrigin string) error {
	var origin string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'origin'").Scan(&origin)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if requestedOrigin == "" {
			return errors.New("store: database has no origin; pass an explicit origin")
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: begin metadata: %w", err)
		}
		now := time.Now().UnixMilli()
		if _, err := tx.ExecContext(ctx, "INSERT INTO meta (key, value) VALUES ('origin', ?), ('created_at_ms', ?)", requestedOrigin, fmt.Sprint(now)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: initialize metadata: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit metadata: %w", err)
		}
		s.origin = requestedOrigin
		return nil
	case err != nil:
		return fmt.Errorf("store: read origin: %w", err)
	case requestedOrigin != "" && origin != requestedOrigin:
		return fmt.Errorf("store: database belongs to origin %q, not %q", origin, requestedOrigin)
	default:
		s.origin = origin
		return nil
	}
}

func (s *Store) Origin() string      { return s.origin }
func (s *Store) Path() string        { return s.path }
func (s *Store) JournalMode() string { return s.journalMode }

// Close checkpoints the WAL so a clean shutdown does not leave a large
// -wal file on flash, then closes the database.
func (s *Store) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var busy, logPages, checkpointed int
	_ = s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logPages, &checkpointed)
	return s.db.Close()
}

// Event is one row of the append-only journal. Result is set only for
// kind == "result"; Payload carries control-event JSON.
type Event struct {
	Seq       int64
	Kind      string
	Origin    string
	OriginSeq int64
	AtMS      int64
	Result    *wire.Result
	Payload   []byte
}

// Replay walks the journal after afterSeq, oldest first, and calls fn
// for each event. It returns the last cursor successfully processed.
func (s *Store) Replay(ctx context.Context, afterSeq int64, limit int, fn func(Event) error) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	if limit > 10000 {
		limit = 10000
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, kind, origin, origin_seq, at_ms,
		       run_id, probe_id, prober, target, check_seq, ok, latency_us,
		       error_code, error, extra, payload
		  FROM journal
		 WHERE seq > ?
		 ORDER BY seq
		 LIMIT ?`, afterSeq, limit)
	if err != nil {
		return afterSeq, fmt.Errorf("store: replay: %w", err)
	}
	defer rows.Close()

	last := afterSeq
	for rows.Next() {
		var (
			event     Event
			runID     sql.NullString
			probeID   sql.NullString
			prober    sql.NullString
			target    sql.NullString
			checkSeq  sql.NullInt64
			ok        sql.NullInt64
			latencyUS sql.NullInt64
			errorCode sql.NullString
			errorText sql.NullString
			extra     []byte
			payload   []byte
		)
		if err := rows.Scan(
			&event.Seq, &event.Kind, &event.Origin, &event.OriginSeq, &event.AtMS,
			&runID, &probeID, &prober, &target, &checkSeq, &ok, &latencyUS,
			&errorCode, &errorText, &extra, &payload,
		); err != nil {
			return last, fmt.Errorf("store: scan replay row: %w", err)
		}
		event.Payload = append([]byte(nil), payload...)
		if event.Kind == "result" {
			result, err := resultFromRow(runID, probeID, prober, target, checkSeq, ok, latencyUS, errorCode, errorText, extra, event.AtMS)
			if err != nil {
				return last, err
			}
			event.Result = &result
		}
		if err := fn(event); err != nil {
			return last, err
		}
		last = event.Seq
	}
	if err := rows.Err(); err != nil {
		return last, fmt.Errorf("store: replay rows: %w", err)
	}
	return last, nil
}

func resultFromRow(
	runID, probeID, prober, target sql.NullString,
	checkSeq, ok, latencyUS sql.NullInt64,
	errorCode, errorText sql.NullString,
	extra []byte,
	atMS int64,
) (wire.Result, error) {
	result := wire.Result{
		RunID:   runID.String,
		ProbeID: probeID.String,
		Result: probe.Result{
			Ok:        ok.Int64 != 0,
			Type:      prober.String,
			Target:    target.String,
			Seq:       int(checkSeq.Int64),
			Error:     errorText.String,
			ErrorCode: errorCode.String,
		},
		ObservedAt: time.UnixMilli(atMS).UTC().Format(time.RFC3339Nano),
	}
	if latencyUS.Valid {
		ms := float64(latencyUS.Int64) / 1000
		result.LatencyMs = &ms
	}
	if len(extra) > 0 {
		// A malformed extra field must not make one undelivered row
		// impossible to drain forever; the typed fields are still valid.
		_ = json.Unmarshal(extra, &result.Extra)
	}
	return result, nil
}

// Stats is a cheap snapshot for logs and the store CLI.
type Stats struct {
	JournalRows      int64
	Results          int64
	PendingResults   int64
	DeliveredResults int64
	RejectedResults  int64
	Probes           int64
	PendingTriggers  int64
	Rollups          int64
	OldestAtMS       int64
	NewestAtMS       int64
	PageSize         int64
	PageCount        int64
	FreePages        int64
	DBBytes          int64
	WALBytes         int64
	JournalMode      string
	AutoVacuum       int
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var stats Stats
	stats.JournalMode = s.journalMode
	var oldest, newest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*),
		       coalesce(sum(kind = 'result'), 0),
		       coalesce(sum(kind = 'result' AND delivery_state = 0), 0),
		       coalesce(sum(kind = 'result' AND delivery_state = 1), 0),
		       coalesce(sum(kind = 'result' AND delivery_state = 2), 0),
		       min(at_ms), max(at_ms)
		  FROM journal`).Scan(
		&stats.JournalRows, &stats.Results, &stats.PendingResults,
		&stats.DeliveredResults, &stats.RejectedResults, &oldest, &newest,
	); err != nil {
		return stats, fmt.Errorf("store: journal stats: %w", err)
	}
	stats.OldestAtMS = oldest.Int64
	stats.NewestAtMS = newest.Int64
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM probes").Scan(&stats.Probes); err != nil {
		return stats, fmt.Errorf("store: probe stats: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM pending_triggers").Scan(&stats.PendingTriggers); err != nil {
		return stats, fmt.Errorf("store: trigger stats: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM rollups").Scan(&stats.Rollups); err != nil {
		return stats, fmt.Errorf("store: rollup stats: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&stats.PageSize); err != nil {
		return stats, fmt.Errorf("store: page size: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&stats.PageCount); err != nil {
		return stats, fmt.Errorf("store: page count: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&stats.FreePages); err != nil {
		return stats, fmt.Errorf("store: free pages: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&stats.AutoVacuum); err != nil {
		return stats, fmt.Errorf("store: auto vacuum: %w", err)
	}
	if info, err := os.Stat(s.path); err == nil {
		stats.DBBytes = info.Size()
	}
	if info, err := os.Stat(s.path + "-wal"); err == nil {
		stats.WALBytes = info.Size()
	}
	return stats, nil
}
