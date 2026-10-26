PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS workspaces (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    owner         TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL,
    image_id      TEXT NOT NULL DEFAULT '',
    golden_id     TEXT,
    pid           INTEGER,
    created_at    INTEGER NOT NULL,
    last_activity INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS ip_allocations (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace_id  TEXT NOT NULL REFERENCES workspaces(id),
    block_idx     INTEGER NOT NULL UNIQUE,
    host_ip       TEXT NOT NULL,
    ns_ip         TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS goldens (
    id         TEXT PRIMARY KEY,
    image_id   TEXT NOT NULL,
    is_active  INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);
