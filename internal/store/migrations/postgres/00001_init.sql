-- +goose Up
CREATE TABLE requests (
  id                  TEXT PRIMARY KEY,   -- ULID, sortable by time
  ts                  BIGINT NOT NULL,    -- unix ms
  upstream            TEXT NOT NULL,
  path                TEXT,
  model               TEXT,
  key_id              TEXT,               -- proxy key or hash of the provider key
  user_id             TEXT,
  session_id          TEXT,
  status              INTEGER,
  streamed            INTEGER,
  tokens_in           BIGINT,
  tokens_out          BIGINT,
  tokens_cache_read   BIGINT,
  tokens_cache_write  BIGINT,
  cost_usd            DOUBLE PRECISION,              -- NULL = price unknown
  latency_ms          BIGINT,
  ttft_ms             BIGINT,
  error               TEXT
);
CREATE INDEX idx_requests_ts ON requests(ts);
CREATE INDEX idx_requests_key_ts ON requests(key_id, ts);
CREATE INDEX idx_requests_user_ts ON requests(user_id, ts);
CREATE INDEX idx_requests_session ON requests(session_id);

CREATE TABLE request_properties (
  request_id TEXT NOT NULL,
  name       TEXT NOT NULL,
  value      TEXT
);
CREATE INDEX idx_props_request ON request_properties(request_id);
CREATE INDEX idx_props_name_value ON request_properties(name, value);

CREATE TABLE request_bodies (
  request_id TEXT PRIMARY KEY,
  req        BYTEA,  -- zstd
  resp       BYTEA   -- zstd
);

CREATE TABLE keys (
  id               TEXT PRIMARY KEY,
  name             TEXT NOT NULL,
  hash             TEXT NOT NULL UNIQUE,
  kind             TEXT NOT NULL DEFAULT 'managed',
  budget_day_usd   DOUBLE PRECISION,
  budget_month_usd DOUBLE PRECISION,
  created          BIGINT NOT NULL
);

-- +goose Down
DROP TABLE keys;
DROP TABLE request_bodies;
DROP TABLE request_properties;
DROP TABLE requests;
