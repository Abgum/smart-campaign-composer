-- The numeric primary key remains the internal relationship key. Public
-- URLs and API payloads use this unpredictable UUID instead.

-- +goose Up
ALTER TABLE campaigns
    ADD COLUMN public_id UUID NOT NULL DEFAULT gen_random_uuid(),
    ADD CONSTRAINT campaigns_public_id_key UNIQUE (public_id);

-- +goose Down
ALTER TABLE campaigns DROP CONSTRAINT campaigns_public_id_key, DROP COLUMN public_id;
