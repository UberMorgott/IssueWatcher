-- Tiered sync: per-project change-detection state (ETags per exact URL,
-- since marks; JSON of provider.PollState) and the time of the last check.

ALTER TABLE projects ADD COLUMN poll_state TEXT NOT NULL DEFAULT '{}';
ALTER TABLE projects ADD COLUMN checked_at TEXT NOT NULL DEFAULT '';
CREATE INDEX items_project_updated ON items(project_id, updated_at);
