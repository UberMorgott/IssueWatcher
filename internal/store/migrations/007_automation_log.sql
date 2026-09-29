-- Automation decisions (docs/ARCHITECTURE.md → Automation): one row per sync
-- event that matched a rule, queued or skipped with the reason.
CREATE TABLE automation_log (
    id       INTEGER PRIMARY KEY,
    at       TEXT NOT NULL,                  -- UTC, 2006-01-02T15:04:05Z
    item_id  INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    event    TEXT NOT NULL,                  -- new_issue | new_comment
    rule_id  TEXT NOT NULL,
    flow     TEXT NOT NULL,                  -- fix | reply | label
    decision TEXT NOT NULL CHECK (decision IN ('queued', 'skipped')),
    reason   TEXT NOT NULL DEFAULT '',       -- skipped: why (runner reason codes)
    job_id   INTEGER REFERENCES jobs(id) ON DELETE SET NULL
);
-- Rule-made job counts for the caps (origin, project / rule, created_at).
CREATE INDEX jobs_rule_project ON jobs(origin, project_id, created_at);
CREATE INDEX jobs_rule_id ON jobs(origin, rule_id, created_at);
