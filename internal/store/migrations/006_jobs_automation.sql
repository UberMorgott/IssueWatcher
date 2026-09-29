-- Phase 3 automation: the jobs table starts fresh with the label flow and the
-- job origin (manual | rule). Owner decision 2026-09-29: no row-preserving
-- migration, old job rows are discarded. Ids continue after the old maximum
-- (AUTOINCREMENT seeded below) so data\jobs\<id> logs/diffs never collide.
CREATE TABLE jobs_new (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    item_id     INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    flow        TEXT NOT NULL CHECK (flow IN ('fix', 'reply', 'label')),
    state       TEXT NOT NULL DEFAULT 'queued'
                CHECK (state IN ('queued', 'running', 'needs_review', 'done', 'failed', 'cancelled')),
    origin      TEXT NOT NULL DEFAULT 'manual' CHECK (origin IN ('manual', 'rule')),
    rule_id     TEXT NOT NULL DEFAULT '',   -- config agents.automation.rules[].id (origin rule)
    profile_id  TEXT NOT NULL DEFAULT '',   -- config agents.profiles[].id
    attempt     INTEGER NOT NULL DEFAULT 1, -- a retry is a new attempt of the same job
    phase       TEXT NOT NULL DEFAULT '',   -- step while running: prepare, agent, check, verify, review, publish
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
INSERT INTO sqlite_sequence (name, seq) SELECT 'jobs_new', coalesce(max(id), 0) FROM jobs;

DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs; -- also renames its sqlite_sequence row

CREATE INDEX jobs_item ON jobs(item_id, id);
CREATE INDEX jobs_state ON jobs(state, id);
-- One unfinished job per item and flow (a batch skips items that already have one).
CREATE UNIQUE INDEX jobs_active ON jobs(item_id, flow) WHERE state IN ('queued', 'running', 'needs_review');
