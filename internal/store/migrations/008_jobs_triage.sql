-- Project triage: a triage job belongs to a project, not an issue (item_id
-- NULL), and ranks the project's open issues; the app then queues fix jobs
-- for the top picks. Owner rule: no row-preserving migration, the jobs table
-- starts fresh (as in 006); ids continue after the old maximum.
CREATE TABLE jobs_new (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    item_id     INTEGER REFERENCES items(id) ON DELETE CASCADE, -- NULL: a project job (triage)
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    flow        TEXT NOT NULL CHECK (flow IN ('fix', 'reply', 'label', 'triage')),
    state       TEXT NOT NULL DEFAULT 'queued'
                CHECK (state IN ('queued', 'running', 'needs_review', 'done', 'failed', 'cancelled')),
    origin      TEXT NOT NULL DEFAULT 'manual' CHECK (origin IN ('manual', 'rule')),
    rule_id     TEXT NOT NULL DEFAULT '',
    profile_id  TEXT NOT NULL DEFAULT '',
    attempt     INTEGER NOT NULL DEFAULT 1,
    phase       TEXT NOT NULL DEFAULT '',
    branch      TEXT NOT NULL DEFAULT '',
    worktree    TEXT NOT NULL DEFAULT '',
    base_sha    TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    result      TEXT NOT NULL DEFAULT '{}', -- JSON runner.Result
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    started_at  TEXT NOT NULL DEFAULT '',
    finished_at TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    CHECK ((flow = 'triage') = (item_id IS NULL))
);
-- The old sequence too: jobs may be empty while data\jobs\<id> folders exist.
INSERT INTO sqlite_sequence (name, seq) SELECT 'jobs_new', max(
    coalesce((SELECT max(id) FROM jobs), 0),
    coalesce((SELECT seq FROM sqlite_sequence WHERE name = 'jobs'), 0));

DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs;

CREATE INDEX jobs_item ON jobs(item_id, id);
CREATE INDEX jobs_state ON jobs(state, id);
CREATE INDEX jobs_rule_project ON jobs(origin, project_id, created_at);
CREATE INDEX jobs_rule_id ON jobs(origin, rule_id, created_at);
-- One unfinished job per item and flow; one unfinished triage per project.
CREATE UNIQUE INDEX jobs_active ON jobs(item_id, flow) WHERE state IN ('queued', 'running', 'needs_review');
CREATE UNIQUE INDEX jobs_active_triage ON jobs(project_id) WHERE flow = 'triage' AND state IN ('queued', 'running', 'needs_review');
