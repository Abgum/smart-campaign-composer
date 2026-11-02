-- What the overview counts by (plan §19).
--
-- deliveries.city_geoname_id: the recipient's city when the campaign was
-- approved (rendered in the same transaction), so history does not move
-- when a person moves, and the overview never reads the users table. NULL
-- means the city is unknown (e.g. only the province is known).
-- deliveries.finished_at: when a delivery ended (sent, failed, unknown,
-- skipped_no_consent); sent_at already marks sends.
-- campaigns.approved_at: when the campaign was approved.

-- +goose Up
ALTER TABLE deliveries
    ADD COLUMN city_geoname_id BIGINT,
    ADD COLUMN finished_at     TIMESTAMPTZ;
ALTER TABLE campaigns ADD COLUMN approved_at TIMESTAMPTZ;

-- Rows from before this migration: the best the data tells.
UPDATE deliveries d SET city_geoname_id = u.city_geoname_id FROM users u WHERE u.id = d.user_id;
UPDATE deliveries SET finished_at = coalesce(sent_at, updated_at)
WHERE status IN ('sent', 'failed', 'unknown', 'skipped_no_consent');
UPDATE campaigns c SET approved_at = coalesce(
    (SELECT min(d.created_at) FROM deliveries d WHERE d.campaign_id = c.id), c.updated_at)
WHERE c.status IN ('approved', 'sending', 'completed');

CREATE INDEX deliveries_sent_at ON deliveries (sent_at) WHERE status = 'sent';
CREATE INDEX deliveries_finished_at ON deliveries (finished_at) WHERE status IN ('failed', 'unknown', 'skipped_no_consent');

-- +goose Down
DROP INDEX deliveries_finished_at;
DROP INDEX deliveries_sent_at;
ALTER TABLE campaigns DROP COLUMN approved_at;
ALTER TABLE deliveries DROP COLUMN finished_at, DROP COLUMN city_geoname_id;
