-- Multilingual campaigns. users.language is the language a recipient chose;
-- NULL now means none was recorded, and the country decides. No row is
-- rewritten: every stored value, the old default 'tr' included, stays a
-- preference.

-- +goose Up
ALTER TABLE users ALTER COLUMN language DROP NOT NULL, ALTER COLUMN language DROP DEFAULT;

-- A version's language setup (one content language, or one variant per
-- language with the country mapping and the decisions on unresolved groups)
-- and how its audience split by language when it was saved.
ALTER TABLE campaign_versions ADD COLUMN localization JSONB, ADD COLUMN language_summary JSONB;

-- The language of the variant each recipient got.
ALTER TABLE deliveries ADD COLUMN language TEXT;

-- Approval counts per language and channel, and who a language decision left
-- out. campaigns.approval keeps its per-channel shape.
ALTER TABLE campaigns ADD COLUMN approval_languages JSONB;

-- +goose Down
ALTER TABLE campaigns DROP COLUMN approval_languages;
ALTER TABLE deliveries DROP COLUMN language;
ALTER TABLE campaign_versions DROP COLUMN language_summary, DROP COLUMN localization;
-- The old schema needs a value for everyone; 'tr' was its default.
UPDATE users SET language = 'tr' WHERE language IS NULL;
ALTER TABLE users ALTER COLUMN language SET DEFAULT 'tr', ALTER COLUMN language SET NOT NULL;
