-- Phase 2 job queue (internal/runner). The 001 placeholder table was never
-- written to, so it is replaced rather than altered (SQLite cannot change CHECK constraints).
DROP TABLE jobs;

CREATE TABLE jobs (
    id          INTEGER PRIMARY KEY,
    item_id     INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    flow        TEXT NOT NULL CHECK (flow IN ('fix', 'reply')),
    state       TEXT NOT NULL DEFAULT 'queued'
                CHECK (state IN ('queued', 'running', 'needs_review', 'done', 'failed', 'cancelled')),
    profile_id  TEXT NOT NULL DEFAULT '',   -- config agents.profiles[].id
    attempt     INTEGER NOT NULL DEFAULT 1, -- a retry is a new attempt of the same job
    phase       TEXT NOT NULL DEFAULT '',   -- step while running: prepare, agent, verify, review, publish
    branch      TEXT NOT NULL DEFAULT '',   -- fix: iw/<number>-<slug>
    worktree    TEXT NOT NULL DEFAULT '',   -- fix: data\worktrees\<project>\<job>; '' once removed
    base_sha    TEXT NOT NULL DEFAULT '',   -- fix: commit the branch started from
    error       TEXT NOT NULL DEFAULT '',   -- why it failed / was cancelled
    result      TEXT NOT NULL DEFAULT '{}', -- JSON runner.Result
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    started_at  TEXT NOT NULL DEFAULT '',
    finished_at TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE INDEX jobs_item ON jobs(item_id, id);
CREATE INDEX jobs_state ON jobs(state, id);
-- One unfinished job per item and flow (a batch skips items that already have one).
CREATE UNIQUE INDEX jobs_active ON jobs(item_id, flow) WHERE state IN ('queued', 'running', 'needs_review');
