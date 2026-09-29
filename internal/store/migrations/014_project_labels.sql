-- A project's labels (GitHub repository labels) read from SQLite: the label
-- editor never waits for GitHub. labels_at is when the list was fetched
-- ('' = never); a stale list is refreshed in the background.

CREATE TABLE project_labels (
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    color       TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (project_id, name)
);
ALTER TABLE projects ADD COLUMN labels_at TEXT NOT NULL DEFAULT '';
