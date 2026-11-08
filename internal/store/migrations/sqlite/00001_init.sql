-- +goose Up
CREATE TABLE requests (
  id                  TEXT PRIMARY KEY,   -- ULID, sortable by time
  ts                  INTEGER NOT NULL,   -- unix ms
  upstream            TEXT NOT NULL,
  path                TEXT,
  model               TEXT,
  key_id              TEXT,               -- proxy key or hash of the provider key
  user_id             TEXT,
  session_id          TEXT,
  status              INTEGER,
  streamed            INTEGER,
  tokens_in           INTEGER,
  tokens_out          INTEGER,
  tokens_cache_read   INTEGER,
  tokens_cache_write  INTEGER,
  cost_usd            REAL,               -- NULL = price unknown
  latency_ms          INTEGER,
  ttft_ms             INTEGER,
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
  req        BLOB,  -- zstd
  resp       BLOB   -- zstd
);

CREATE TABLE keys (
  id               TEXT PRIMARY KEY,
  name             TEXT NOT NULL,
  hash             TEXT NOT NULL UNIQUE,
  kind             TEXT NOT NULL DEFAULT 'managed',
  budget_day_usd   REAL,
  budget_month_usd REAL,
  created          INTEGER NOT NULL
);

-- +goose Down
DROP TABLE keys;
DROP TABLE request_bodies;
DROP TABLE request_properties;
DROP TABLE requests;
