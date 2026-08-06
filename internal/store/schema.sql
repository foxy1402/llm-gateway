CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS providers (
    id               TEXT PRIMARY KEY,
    display          TEXT NOT NULL,
    base_url         TEXT NOT NULL,
    auth_key         TEXT NOT NULL,
    model            TEXT NOT NULL,
    weight           INTEGER NOT NULL DEFAULT 1,
    tags             TEXT NOT NULL DEFAULT '',
    enabled          INTEGER NOT NULL DEFAULT 1,
    responses_native INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE IF NOT EXISTS provider_accounts (
    id          TEXT PRIMARY KEY,
    provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    label       TEXT NOT NULL DEFAULT '',
    auth_key    TEXT NOT NULL,
    model       TEXT NOT NULL DEFAULT '',
    enabled     INTEGER NOT NULL DEFAULT 1,
    position    INTEGER NOT NULL DEFAULT 0,
    weight      INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE IF NOT EXISTS provider_models (
    provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    model_id    TEXT NOT NULL,
    position    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (provider_id, model_id)
);

CREATE TABLE IF NOT EXISTS combos (
    id           TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    rotation     TEXT NOT NULL DEFAULT 'round-robin',
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   INTEGER NOT NULL DEFAULT (unixepoch())
);

-- combo_members: each row pins one provider (+ optionally one account key + model).
-- account_id NULL means rotate across all of the provider's keys; ON DELETE SET NULL
-- falls back to key rotation when the pinned account is removed. Members are always
-- rewritten with sequential positions on save, so (combo_id, position) is the PK.
CREATE TABLE IF NOT EXISTS combo_members (
    combo_id    TEXT NOT NULL REFERENCES combos(id) ON DELETE CASCADE,
    provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    account_id  TEXT REFERENCES provider_accounts(id) ON DELETE SET NULL,
    model       TEXT NOT NULL DEFAULT '',
    position    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (combo_id, position)
);

CREATE TABLE IF NOT EXISTS request_log (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                INTEGER NOT NULL DEFAULT (unixepoch()),
    model_in          TEXT NOT NULL,
    provider_used     TEXT NOT NULL,
    endpoint          TEXT NOT NULL,
    status            INTEGER NOT NULL,
    latency_ms        INTEGER NOT NULL,
    prompt_tokens     INTEGER,
    completion_tokens INTEGER,
    error             TEXT
);

CREATE INDEX IF NOT EXISTS idx_log_ts ON request_log(ts DESC);
CREATE INDEX IF NOT EXISTS idx_log_provider ON request_log(provider_used);
CREATE INDEX IF NOT EXISTS idx_accounts_provider ON provider_accounts(provider_id);
CREATE INDEX IF NOT EXISTS idx_models_provider ON provider_models(provider_id);
