-- Phase 1 sync: incremental cursors, repo membership, issue body/close time,
-- unread flag for the tray badge, comment links.

ALTER TABLE projects ADD COLUMN sync_cursor TEXT NOT NULL DEFAULT ''; -- max item updated_at seen
ALTER TABLE projects ADD COLUMN synced_at   TEXT NOT NULL DEFAULT ''; -- '' = never synced (baseline pending)
ALTER TABLE projects ADD COLUMN active      INTEGER NOT NULL DEFAULT 1; -- 0 = no longer reachable

ALTER TABLE items ADD COLUMN body      TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN closed_at TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN unread    INTEGER NOT NULL DEFAULT 0;

ALTER TABLE comments ADD COLUMN url TEXT NOT NULL DEFAULT '';

CREATE INDEX items_updated ON items(updated_at);
CREATE INDEX items_unread ON items(unread) WHERE unread = 1;
