-- A/B test. campaign_versions.ab_test is a version's B side: one B content
-- for each A content (the version's own content), in the same language and
-- on the same channels; NULL means no A/B test. deliveries.ab_variant is
-- the arm a recipient was assigned to at approval (NULL: no A/B test); the
-- worker sends the stored text, so a retry or a restart never changes it.
-- campaigns.approval_variants keeps the approval's counts per arm and
-- channel, apart from approval (per channel) like approval_languages.

-- +goose Up
ALTER TABLE campaign_versions ADD COLUMN ab_test JSONB;
ALTER TABLE deliveries ADD COLUMN ab_variant TEXT CHECK (ab_variant IN ('A', 'B'));
ALTER TABLE campaigns ADD COLUMN approval_variants JSONB;

-- +goose Down
ALTER TABLE campaigns DROP COLUMN approval_variants;
ALTER TABLE deliveries DROP COLUMN ab_variant;
ALTER TABLE campaign_versions DROP COLUMN ab_test;
