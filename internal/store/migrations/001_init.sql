-- Initial schema: minimal columns per domain type in docs/ARCHITECTURE.md.
-- Timestamps are UTC RFC 3339 text.

CREATE TABLE sources (
    id         INTEGER PRIMARY KEY,
    platform   TEXT NOT NULL,              -- github, curseforge, nexus, steam
    account    TEXT NOT NULL,              -- login on that platform
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE (platform, account)
);

CREATE TABLE projects (
    id          INTEGER PRIMARY KEY,
    source_id   INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    external_id TEXT NOT NULL,             -- platform id (e.g. owner/repo)
    name        TEXT NOT NULL,
    url         TEXT NOT NULL DEFAULT '',
    local_path  TEXT NOT NULL DEFAULT '',  -- optional mapped local folder
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE (source_id, external_id)
);

CREATE TABLE items (
    id          INTEGER PRIMARY KEY,
    source_id   INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    external_id TEXT NOT NULL,             -- platform node id
    kind        TEXT NOT NULL DEFAULT 'issue',
    number      INTEGER NOT NULL DEFAULT 0,
    title       TEXT NOT NULL DEFAULT '',
    url         TEXT NOT NULL DEFAULT '',
    author      TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
    raw_status  TEXT NOT NULL DEFAULT '',  -- platform-specific status
    labels      TEXT NOT NULL DEFAULT '[]', -- JSON array
    created_at  TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT '',
    UNIQUE (source_id, external_id)
);
CREATE INDEX items_project ON items(project_id, status);

CREATE TABLE comments (
    id          INTEGER PRIMARY KEY,
    item_id     INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    external_id TEXT NOT NULL,
    author      TEXT NOT NULL DEFAULT '',
    body        TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT '',
    UNIQUE (item_id, external_id)
);

CREATE TABLE jobs (
    id         INTEGER PRIMARY KEY,
    item_id    INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    flow       TEXT NOT NULL CHECK (flow IN ('fix', 'reply', 'verify', 'label')),
    state      TEXT NOT NULL DEFAULT 'queued'
               CHECK (state IN ('queued', 'running', 'needs_review', 'done', 'failed', 'cancelled')),
    attempts   INTEGER NOT NULL DEFAULT 0,
    log_path   TEXT NOT NULL DEFAULT '',
    result     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE INDEX jobs_item ON jobs(item_id, state);
