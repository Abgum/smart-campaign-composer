package campaign

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CancelScheduled cancels an approved campaign while its send time is still
// in the future and no delivery has started. Pending deliveries are removed
// in the same transaction, releasing their content deduplication slots.
func (s *Store) CancelScheduled(ctx context.Context, campaignID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var status string
	var future bool
	err = tx.QueryRow(ctx, `
		SELECT status, coalesce(scheduled_at > clock_timestamp(), false)
		FROM campaigns WHERE id=$1 FOR UPDATE`, campaignID).Scan(&status, &future)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status == "cancelled" {
		return nil // a repeated request has the same result
	}
	if status != "approved" || !future {
		return ErrNotCancellable
	}

	var total int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE campaign_id=$1`, campaignID).Scan(&total); err != nil {
		return err
	}
	deleted, err := tx.Exec(ctx, `DELETE FROM deliveries WHERE campaign_id=$1 AND status='pending'`, campaignID)
	if err != nil {
		return fmt.Errorf("delete scheduled deliveries: %w", err)
	}
	if deleted.RowsAffected() != total {
		return ErrNotCancellable // a worker claimed a delivery; rollback restores deleted rows
	}
	updated, err := tx.Exec(ctx, `
		UPDATE campaigns SET status='cancelled', updated_at=now()
		WHERE id=$1 AND status='approved' AND scheduled_at > clock_timestamp()`, campaignID)
	if err != nil {
		return err
	}
	if updated.RowsAffected() != 1 {
		return ErrNotCancellable
	}
	return tx.Commit(ctx)
}
