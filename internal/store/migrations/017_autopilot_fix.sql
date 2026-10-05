-- Autopilot Phase 3 (docs/AUTOPILOT.md → Fix run, Event inbox): an inbox event
-- is pending while consumed_run_id IS NULL and outcome = ''; outcome records
-- why one was dropped without a run (autopilot off, no fix rule, closed item…)
-- or 'attached' when it joined an open fix run of its item.
ALTER TABLE autopilot_inbox ADD COLUMN outcome TEXT NOT NULL DEFAULT '';

-- Fix runs by their release (claim / released) and by state (the coalescing timer).
CREATE INDEX autopilot_runs_release ON autopilot_runs(release_id) WHERE release_id IS NOT NULL;
CREATE INDEX autopilot_runs_fix_state ON autopilot_runs(project_id, state) WHERE kind = 'fix';
