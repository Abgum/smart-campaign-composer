-- The interface language of the request that started a turn ("en"; NULL is
-- Turkish). Go's own notes in the transcript (a turn that failed after
-- saving, a question the router could not get from the model) are written in
-- it; the model's reply follows the language of the message itself.

-- +goose Up
ALTER TABLE turns ADD COLUMN lang TEXT;

-- +goose Down
ALTER TABLE turns DROP COLUMN lang;
