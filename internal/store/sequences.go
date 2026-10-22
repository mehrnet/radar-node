package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// nextOriginSeq reserves count consecutive sequence numbers for one
// origin. The read/update runs inside the caller's transaction; a
// competing process gets a stale-snapshot write failure rather than a
// duplicate sequence.
func nextOriginSeq(ctx context.Context, tx *sql.Tx, origin string, count int) (int64, error) {
	if count < 1 {
		return 0, errors.New("store: sequence count must be positive")
	}
	var next int64
	err := tx.QueryRowContext(ctx, "SELECT next_seq FROM sequences WHERE origin = ?", origin).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO sequences (origin, next_seq) VALUES (?, ?)", origin, int64(count)+1); err != nil {
			return 0, fmt.Errorf("store: initialize origin sequence: %w", err)
		}
		return 1, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: read origin sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE sequences SET next_seq = ? WHERE origin = ?", next+int64(count), origin); err != nil {
		return 0, fmt.Errorf("store: advance origin sequence: %w", err)
	}
	return next, nil
}
