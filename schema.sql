CREATE TABLE IF NOT EXISTS orders (
    id    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    item  TEXT        NOT NULL,
    qty   INT         NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS outbox (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    topic         TEXT        NOT NULL,
    key           TEXT        NOT NULL,
    payload       JSONB       NOT NULL,
    published_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_outbox_unpublished
    ON outbox (created_at) WHERE published_at IS NULL;
