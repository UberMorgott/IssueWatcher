-- Autopilot (docs/AUTOPILOT.md → Persistence, Activity log): runs, their items
-- and persisted steps, the sync event inbox and the activity log.
-- Run state has no CHECK: fix runs (Phase 2) add states of their own and SQLite
-- cannot change a CHECK; the Go consts (store/autopilot.go) are the set.
CREATE TABLE autopilot_runs (
    id              INTEGER PRIMARY KEY,
    kind            TEXT NOT NULL CHECK (kind IN ('fix', 'release')),
    project_id      INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    state           TEXT NOT NULL DEFAULT 'pending',
    origin          TEXT NOT NULL CHECK (origin IN ('auto', 'manual', 'mcp')),
    release_id      INTEGER REFERENCES autopilot_runs(id) ON DELETE SET NULL, -- fix run: the release that claimed it
    manifest_json   TEXT NOT NULL DEFAULT '{}',
    version         TEXT NOT NULL DEFAULT '',
    artifact_sha256 TEXT NOT NULL DEFAULT '',
    held_reason     TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE INDEX autopilot_runs_project ON autopilot_runs(project_id, id);
CREATE INDEX autopilot_runs_created ON autopilot_runs(kind, created_at);
-- One unfinished release run per project.
CREATE UNIQUE INDEX autopilot_runs_release_active ON autopilot_runs(project_id)
    WHERE kind = 'release' AND state IN ('pending', 'running', 'held');

CREATE TABLE autopilot_run_items (
    run_id  INTEGER NOT NULL REFERENCES autopilot_runs(id) ON DELETE CASCADE,
    item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    role    TEXT NOT NULL DEFAULT 'primary' CHECK (role IN ('primary', 'duplicate')),
    PRIMARY KEY (run_id, item_id)
);
CREATE INDEX autopilot_run_items_item ON autopilot_run_items(item_id);

CREATE TABLE autopilot_steps (
    id           INTEGER PRIMARY KEY,
    run_id       INTEGER NOT NULL REFERENCES autopilot_runs(id) ON DELETE CASCADE,
    seq          INTEGER NOT NULL,             -- execution order within the run
    step         TEXT NOT NULL,                -- claim, bump, build, ..., publish, available
    target       TEXT NOT NULL DEFAULT '',     -- gh_asset file name, publish target, reply item
    state        TEXT NOT NULL DEFAULT 'pending'
                 CHECK (state IN ('pending', 'sending', 'sent', 'failed', 'unknown', 'skipped')),
    attempt      INTEGER NOT NULL DEFAULT 0,
    idem_key     TEXT NOT NULL DEFAULT '',
    request_json TEXT NOT NULL DEFAULT '{}',
    external_ref TEXT NOT NULL DEFAULT '',
    error        TEXT NOT NULL DEFAULT '',
    started_at   TEXT NOT NULL DEFAULT '',
    finished_at  TEXT NOT NULL DEFAULT '',
    UNIQUE (run_id, step, target)
);
CREATE INDEX autopilot_steps_order ON autopilot_steps(run_id, seq);

CREATE TABLE autopilot_inbox (
    id              INTEGER PRIMARY KEY,
    source_event    TEXT NOT NULL UNIQUE, -- platform + item external id + comment external id
    item_id         INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    kind            TEXT NOT NULL,
    at              TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    consumed_run_id INTEGER REFERENCES autopilot_runs(id) ON DELETE SET NULL
);
CREATE INDEX autopilot_inbox_pending ON autopilot_inbox(id) WHERE consumed_run_id IS NULL;

-- Activity log: kept to the newest 5000 rows.
CREATE TABLE autopilot_events (
    id          INTEGER PRIMARY KEY,
    at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    run_id      INTEGER REFERENCES autopilot_runs(id) ON DELETE SET NULL,
    project_id  INTEGER REFERENCES projects(id) ON DELETE SET NULL,
    item_id     INTEGER REFERENCES items(id) ON DELETE SET NULL,
    kind        TEXT NOT NULL,
    severity    TEXT NOT NULL DEFAULT 'info' CHECK (severity IN ('info', 'attention')),
    title       TEXT NOT NULL DEFAULT '',
    detail_json TEXT NOT NULL DEFAULT '{}',
    read_at     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX autopilot_events_unread ON autopilot_events(id) WHERE read_at = '';
