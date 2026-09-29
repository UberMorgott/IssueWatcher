-- Persisted sync schedule: the time of a source's last completed full
-- reconcile, so a restart resumes from SQLite instead of reconciling again.
-- Existing sources start from their newest project sync (the last reconcile or
-- change check that stored items), '' when never synced.

ALTER TABLE sources ADD COLUMN reconciled_at TEXT NOT NULL DEFAULT '';
UPDATE sources SET reconciled_at = COALESCE((SELECT max(p.synced_at) FROM projects p WHERE p.source_id = sources.id), '');
