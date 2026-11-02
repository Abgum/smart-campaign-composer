-- A campaign approved for a later moment: every delivery is queued with
-- next_attempt_at at that moment, so the worker claims none before it.
-- NULL means it was sent when approved. timestamptz keeps the instant in UTC.

-- +goose Up
ALTER TABLE campaigns ADD COLUMN scheduled_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE campaigns DROP COLUMN scheduled_at;
