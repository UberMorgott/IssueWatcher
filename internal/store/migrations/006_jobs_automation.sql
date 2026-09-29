-- Phase 3 automation: new flow label and job origin (manual | rule).
-- SQLite cannot alter a CHECK, and jobs now hold real rows, so the table is
-- rebuilt by copying every row (ids, attempts, results kept).
CREATE TABLE jobs_new (
    id          INTEGER PRIMARY KEY,
    item_id     INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    flow        TEXT NOT NULL CHECK (flow IN ('fix', 'reply', 'label')),
    state       TEXT NOT NULL DEFAULT 'queued'
                CHECK (state IN ('queued', 'running', 'needs_review', 'done', 'failed', 'cancelled')),
    origin      TEXT NOT NULL DEFAULT 'manual' CHECK (origin IN ('manual', 'rule')),
    rule_id     TEXT NOT NULL DEFAULT '',   -- config agents.automation.rules[].id (origin rule)
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

INSERT INTO jobs_new (id, item_id, project_id, flow, state, profile_id, attempt, phase, branch, worktree, base_sha,
                      error, result, created_at, started_at, finished_at, updated_at)
SELECT id, item_id, project_id, flow, state, profile_id, attempt, phase, branch, worktree, base_sha,
       error, result, created_at, started_at, finished_at, updated_at
FROM jobs;

DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs;

CREATE INDEX jobs_item ON jobs(item_id, id);
CREATE INDEX jobs_state ON jobs(state, id);
-- One unfinished job per item and flow (a batch skips items that already have one).
CREATE UNIQUE INDEX jobs_active ON jobs(item_id, flow) WHERE state IN ('queued', 'running', 'needs_review');
