-- +goose Up

-- Seeded recipients. Contacts are unique so one person cannot exist twice.
CREATE TABLE users (
    id            BIGSERIAL PRIMARY KEY,
    first_name    TEXT NOT NULL,
    last_name     TEXT NOT NULL,
    email         TEXT UNIQUE,
    phone         TEXT UNIQUE,
    -- Location: country ISO 3166-1 alpha-2, first-level region ISO 3166-2
    -- (Turkish province, e.g. TR-34), settlement GeoNames geonameId. The
    -- province and the city are different targeting levels; a user may have
    -- a province but no known city.
    country_code    TEXT NOT NULL CHECK (country_code ~ '^[A-Z]{2}$'),
    admin1_code     TEXT CHECK (admin1_code ~ '^[A-Z]{2}-[A-Z0-9]{1,3}$'),
    city_geoname_id BIGINT,
    language      TEXT NOT NULL DEFAULT 'tr',
    birth_date    DATE NOT NULL,
    total_orders  INT NOT NULL DEFAULT 0,
    total_spent   NUMERIC(12, 2) NOT NULL DEFAULT 0,
    consent_sms   BOOLEAN NOT NULL DEFAULT false,
    consent_email BOOLEAN NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE user_events (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    type        TEXT NOT NULL CHECK (type IN ('cart_add', 'purchase', 'product_view')),
    occurred_at TIMESTAMPTZ NOT NULL,
    amount      NUMERIC(12, 2)
);
CREATE INDEX users_location ON users (country_code, admin1_code);
CREATE INDEX users_city ON users (city_geoname_id);

CREATE INDEX user_events_user_type_time ON user_events (user_id, type, occurred_at);
CREATE INDEX user_events_type_time ON user_events (type, occurred_at);

CREATE TABLE chats (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title      TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per chat request. (chat_id, client_turn_id) is the dedupe key;
-- at most one running turn per chat keeps context building race-free.
CREATE TABLE turns (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chat_id        UUID NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    client_turn_id UUID NOT NULL,
    request_hash   TEXT NOT NULL,
    focus_campaign_id BIGINT, -- UI tab at send time; FK added after campaigns exists
    status         TEXT NOT NULL CHECK (status IN ('running', 'completed', 'failed')),
    outcome        TEXT CHECK (outcome IN ('ok', 'partial', 'failed')),
    error_code     TEXT,
    route          JSONB, -- verified routing decision (intent, basis, source, reject_reason)
    heartbeat_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at    TIMESTAMPTZ,
    UNIQUE (chat_id, client_turn_id)
);
CREATE UNIQUE INDEX ux_turns_one_running_per_chat ON turns (chat_id) WHERE status = 'running';

-- Visible transcript: one user and one assistant row per turn.
CREATE TABLE messages (
    id           BIGSERIAL PRIMARY KEY,
    chat_id      UUID NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    turn_id      UUID NOT NULL REFERENCES turns (id) ON DELETE CASCADE,
    role         TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
    text         TEXT NOT NULL,
    outcome      TEXT CHECK (outcome IN ('ok', 'partial', 'failed')),
    llm_provider TEXT,
    llm_model    TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (turn_id, role)
);
CREATE INDEX messages_chat_id ON messages (chat_id, id);

CREATE TABLE campaigns (
    id                  BIGSERIAL PRIMARY KEY,
    chat_id             UUID NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    ordinal             INT NOT NULL,
    name                TEXT NOT NULL,
    status              TEXT NOT NULL CHECK (status IN ('draft', 'approved', 'sending', 'completed', 'cancelled')),
    current_version_id  BIGINT,
    approved_version_id BIGINT,
    approval            JSONB, -- snapshot counts per channel, written at approval
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (chat_id, ordinal)
);

-- Single source of truth for campaign content; every change is a new row.
CREATE TABLE campaign_versions (
    id                 BIGSERIAL PRIMARY KEY,
    campaign_id        BIGINT NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    version            INT NOT NULL,
    source             TEXT NOT NULL CHECK (source IN ('llm', 'user')),
    llm_provider       TEXT,
    llm_model          TEXT,
    name               TEXT NOT NULL,
    segment_rule       JSONB NOT NULL,
    sms                JSONB,
    email              JSONB,
    audience           JSONB NOT NULL,
    change_summary     TEXT NOT NULL DEFAULT '',
    created_by_turn_id UUID REFERENCES turns (id) ON DELETE SET NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (campaign_id, version)
);

ALTER TABLE turns
    ADD CONSTRAINT turns_focus_campaign_fk
        FOREIGN KEY (focus_campaign_id) REFERENCES campaigns (id) ON DELETE SET NULL;

ALTER TABLE campaigns
    ADD CONSTRAINT campaigns_current_version_fk
        FOREIGN KEY (current_version_id) REFERENCES campaign_versions (id) DEFERRABLE INITIALLY DEFERRED,
    ADD CONSTRAINT campaigns_approved_version_fk
        FOREIGN KEY (approved_version_id) REFERENCES campaign_versions (id) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE turn_campaign_refs (
    turn_id     UUID NOT NULL REFERENCES turns (id) ON DELETE CASCADE,
    campaign_id BIGINT NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    version_id  BIGINT NOT NULL REFERENCES campaign_versions (id) ON DELETE CASCADE,
    action      TEXT NOT NULL CHECK (action IN ('created', 'revised')),
    PRIMARY KEY (turn_id, version_id)
);

-- Audit of every model call; never replayed into later prompts.
CREATE TABLE llm_calls (
    id            BIGSERIAL PRIMARY KEY,
    turn_id       UUID NOT NULL REFERENCES turns (id) ON DELETE CASCADE,
    kind          TEXT NOT NULL DEFAULT 'main' CHECK (kind IN ('router', 'main')),
    iteration     INT NOT NULL,
    provider      TEXT NOT NULL,
    model         TEXT NOT NULL DEFAULT '',
    request       JSONB,
    response      JSONB,
    stop_reason   TEXT,
    input_tokens  INT,
    output_tokens INT,
    latency_ms    INT,
    error_code    TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Queue and send log. Content is rendered once at approval and sent as is.
CREATE TABLE deliveries (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id         BIGINT NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    user_id             BIGINT NOT NULL REFERENCES users (id),
    channel             TEXT NOT NULL CHECK (channel IN ('sms', 'email')),
    subject             TEXT,
    body                TEXT NOT NULL,
    content_hash        TEXT NOT NULL,
    status              TEXT NOT NULL CHECK (status IN
                            ('pending', 'in_flight', 'retry', 'sent', 'failed', 'unknown', 'skipped_no_consent')),
    attempts            INT NOT NULL DEFAULT 0,
    ambiguous_attempts  INT NOT NULL DEFAULT 0,
    next_attempt_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_until        TIMESTAMPTZ,
    last_error          TEXT,
    provider_message_id TEXT,
    sent_at             TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (campaign_id, user_id, channel)
);

-- "Aynı kişiye aynı mesaj iki kez gitmez": a message is (user, channel,
-- rendered content). The slot is held by every row except definite
-- non-delivery; 'unknown' (provider may have accepted) keeps it.
CREATE UNIQUE INDEX ux_deliveries_message ON deliveries (user_id, channel, content_hash)
    WHERE status NOT IN ('failed', 'skipped_no_consent');
CREATE INDEX deliveries_claim ON deliveries (channel, next_attempt_at) WHERE status IN ('pending', 'retry');
CREATE INDEX deliveries_campaign_status ON deliveries (campaign_id, status);
-- The reaper's scan for leases that ran out mid-attempt.
CREATE INDEX deliveries_leases ON deliveries (locked_until) WHERE status = 'in_flight';

CREATE TABLE worker_heartbeats (
    name    TEXT PRIMARY KEY,
    beat_at TIMESTAMPTZ NOT NULL
);

-- Send limits shared by every worker and kept across restarts. Per channel
-- the last `calls` call starts sit in a ring of slots; a call may start once
-- the oldest of them is a window old. paused_until holds a provider 429 for
-- all workers. Time is the database clock, the one clock workers share.
CREATE TABLE channel_limits (
    channel      TEXT PRIMARY KEY CHECK (channel IN ('sms', 'email')),
    calls        INT NOT NULL CHECK (calls > 0),
    window_ms    INT NOT NULL CHECK (window_ms > 0),
    next_slot    INT NOT NULL DEFAULT 0,
    paused_until TIMESTAMPTZ
);
CREATE TABLE channel_grants (
    channel    TEXT NOT NULL REFERENCES channel_limits (channel),
    slot       INT NOT NULL,
    granted_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (channel, slot)
);

-- grant_call lets one call start on a channel now and returns the grant's
-- instant, unless the channel is paused or `calls` calls already started in
-- the last window; then it returns how long to wait (ms) and whether the
-- wait is a pause. The row lock serializes grants on a channel across
-- workers.
-- +goose StatementBegin
CREATE FUNCTION grant_call(p_channel TEXT, OUT granted_at TIMESTAMPTZ, OUT wait_ms DOUBLE PRECISION,
                           OUT paused BOOLEAN) AS $$
DECLARE
    lim    channel_limits%ROWTYPE;
    now_   TIMESTAMPTZ;
    oldest TIMESTAMPTZ;
    frees  TIMESTAMPTZ;
BEGIN
    SELECT * INTO lim FROM channel_limits WHERE channel = p_channel FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no send limit for channel %', p_channel;
    END IF;
    now_ := clock_timestamp();
    paused := lim.paused_until IS NOT NULL AND lim.paused_until > now_;
    IF paused THEN
        wait_ms := extract(epoch FROM lim.paused_until - now_)::double precision * 1000;
        RETURN;
    END IF;
    SELECT g.granted_at INTO oldest FROM channel_grants g WHERE g.channel = p_channel AND g.slot = lim.next_slot;
    frees := oldest + make_interval(secs => lim.window_ms / 1000.0);
    IF frees > now_ THEN
        wait_ms := extract(epoch FROM frees - now_)::double precision * 1000;
        RETURN;
    END IF;
    INSERT INTO channel_grants (channel, slot, granted_at) VALUES (p_channel, lim.next_slot, now_)
    ON CONFLICT (channel, slot) DO UPDATE SET granted_at = EXCLUDED.granted_at;
    UPDATE channel_limits SET next_slot = (lim.next_slot + 1) % lim.calls WHERE channel = p_channel;
    granted_at := now_;
    wait_ms := 0;
END
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION grant_call(TEXT);
DROP TABLE channel_grants;
DROP TABLE channel_limits;
DROP TABLE worker_heartbeats;
DROP TABLE deliveries;
DROP TABLE llm_calls;
DROP TABLE turn_campaign_refs;
ALTER TABLE turns DROP CONSTRAINT turns_focus_campaign_fk;
ALTER TABLE campaigns DROP CONSTRAINT campaigns_current_version_fk, DROP CONSTRAINT campaigns_approved_version_fk;
DROP TABLE campaign_versions;
DROP TABLE campaigns;
DROP TABLE messages;
DROP TABLE turns;
DROP TABLE chats;
DROP TABLE user_events;
DROP TABLE users;
