package store

const schemaVersion = 1

const schemaV1 = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS sequences (
	origin   TEXT PRIMARY KEY,
	next_seq INTEGER NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS journal (
	seq             INTEGER PRIMARY KEY AUTOINCREMENT,
	kind            TEXT    NOT NULL,
	origin          TEXT    NOT NULL,
	origin_seq      INTEGER NOT NULL,
	at_ms           INTEGER NOT NULL,

	run_id          TEXT,
	probe_id        TEXT,
	prober          TEXT,
	target          TEXT,
	check_seq       INTEGER,
	ok              INTEGER,
	latency_us      INTEGER,
	error_code      TEXT,
	error           TEXT,
	extra           BLOB,

	payload         BLOB,

	delivery_state  INTEGER NOT NULL DEFAULT 0 CHECK (delivery_state IN (0, 1, 2)),
	delivery_reason TEXT,
	delivered_at_ms INTEGER,
	attempts        INTEGER NOT NULL DEFAULT 0,

	CHECK (kind <> 'result' OR (
		run_id IS NOT NULL AND probe_id IS NOT NULL AND check_seq IS NOT NULL AND ok IS NOT NULL
	)),
	UNIQUE (origin, origin_seq)
) STRICT;

CREATE UNIQUE INDEX IF NOT EXISTS journal_result_dedup
	ON journal(origin, run_id, probe_id, check_seq) WHERE kind = 'result';

CREATE INDEX IF NOT EXISTS journal_result_probe_time
	ON journal(probe_id, at_ms) WHERE kind = 'result';

CREATE INDEX IF NOT EXISTS journal_pending
	ON journal(seq) WHERE kind = 'result' AND delivery_state = 0;

CREATE INDEX IF NOT EXISTS journal_at
	ON journal(at_ms);

CREATE TABLE IF NOT EXISTS probes (
	id                  TEXT PRIMARY KEY,
	target              TEXT    NOT NULL,
	prober              TEXT    NOT NULL,
	probe_count         INTEGER NOT NULL,
	timeout_ms          INTEGER NOT NULL,
	schedule_type       TEXT    NOT NULL,
	interval_seconds    INTEGER NOT NULL,
	starts_at_ms        INTEGER NOT NULL,
	ends_at_ms          INTEGER NOT NULL,
	params_json         BLOB,
	status              TEXT    NOT NULL,
	last_run_at_ms      INTEGER,
	last_trigger_run_id TEXT
) STRICT;

CREATE INDEX IF NOT EXISTS probes_status_schedule
	ON probes(status, schedule_type);

CREATE TABLE IF NOT EXISTS pending_triggers (
	probe_id      TEXT    NOT NULL,
	run_id        TEXT    NOT NULL,
	event_seq     INTEGER NOT NULL,
	created_at_ms INTEGER NOT NULL,
	snapshot_json BLOB    NOT NULL,
	PRIMARY KEY (probe_id, run_id)
) STRICT;

CREATE TABLE IF NOT EXISTS completed_triggers (
	probe_id        TEXT    NOT NULL,
	run_id          TEXT    NOT NULL,
	completed_at_ms INTEGER NOT NULL,
	PRIMARY KEY (probe_id, run_id)
) STRICT;

CREATE INDEX IF NOT EXISTS completed_triggers_at
	ON completed_triggers(completed_at_ms);

CREATE TABLE IF NOT EXISTS rollups (
	origin          TEXT    NOT NULL,
	probe_id        TEXT    NOT NULL,
	bucket_ms       INTEGER NOT NULL,
	checks          INTEGER NOT NULL,
	ok              INTEGER NOT NULL,
	errors          INTEGER NOT NULL,
	latency_sum_us  INTEGER NOT NULL,
	latency_count   INTEGER NOT NULL,
	latency_min_us  INTEGER,
	latency_max_us  INTEGER,
	PRIMARY KEY (origin, probe_id, bucket_ms)
) STRICT, WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS rollups_bucket
	ON rollups(bucket_ms);
`
