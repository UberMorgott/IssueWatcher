package release

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// The autopilot loop (docs/AUTOPILOT.md → Model: two pipelines): every
// TickEvery, and whenever Kick is called (a sync, a pushed fix), it consumes
// the event inbox into fix runs, moves the fix runs forward and lets the
// coalescing timer start release runs. The global kill switch
// (agents.autopilot.paused) stops new fix runs, pushes and releases.

// Kick wakes the autopilot loop (no-op when it is already due).
func (e *Engine) Kick() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

func (e *Engine) loop(ctx context.Context) {
	t := time.NewTicker(e.d.TickEvery)
	defer t.Stop()
	for {
		e.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-e.kick:
		}
	}
}

// Tick runs one autopilot round: inbox → fix runs (in the background) →
// coalescing timer.
func (e *Engine) Tick(ctx context.Context) {
	e.tickMu.Lock()
	defer e.tickMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	cfg := e.d.Settings()
	paused := cfg.Agents.Autopilot.Paused
	if !paused {
		e.consumeInbox(ctx, cfg)
	}
	e.advanceFixRuns(ctx)
	if !paused {
		e.coalesce(ctx, cfg)
	}
}

// coalesce starts a release run (origin auto) for every project whose pushed,
// unclaimed fix runs are due: coalesceMinutes after the newest, or
// maxBatchAgeHours after the oldest. The fix runs are claimed in the run's
// create transaction; a busy project or a used-up cap waits quietly.
func (e *Engine) coalesce(ctx context.Context, cfg config.Settings) {
	runs, err := e.d.Store.Runs(ctx, store.RunFilter{Kind: store.RunKindFix, State: store.RunPushed, Limit: 500})
	if err != nil {
		e.d.Log.Error("autopilot: pushed fix runs", "err", err)
		return
	}
	byProject := map[int64][]store.Run{}
	var order []int64
	for _, r := range slices.Backward(runs) { // oldest first
		if r.ReleaseID != 0 {
			continue
		}
		if _, ok := byProject[r.ProjectID]; !ok {
			order = append(order, r.ProjectID)
		}
		byProject[r.ProjectID] = append(byProject[r.ProjectID], r)
	}
	now := e.d.Now()
	for _, pid := range order {
		repo, err := e.d.Store.Repo(ctx, pid)
		if err != nil {
			continue
		}
		ap := cfg.Agents.AutopilotFor(repo.Key)
		if !ap.Enabled || !ap.AutoRelease {
			continue
		}
		list := byProject[pid]
		var newest, oldest time.Time
		for _, r := range list {
			at, err := time.Parse(time.RFC3339Nano, r.UpdatedAt)
			if err != nil {
				continue
			}
			if newest.IsZero() || at.After(newest) {
				newest = at
			}
			if oldest.IsZero() || at.Before(oldest) {
				oldest = at
			}
		}
		due := !newest.IsZero() && (!now.Before(newest.Add(time.Duration(ap.CoalesceMinutes)*time.Minute)) ||
			!now.Before(oldest.Add(time.Duration(ap.MaxBatchAgeHours)*time.Hour)))
		if !due {
			continue
		}
		var ids, items []int64
		for _, r := range list {
			ids = append(ids, r.ID)
			its, err := e.d.Store.RunItems(ctx, r.ID)
			if err != nil {
				continue
			}
			for _, it := range its {
				if !slices.Contains(items, it.ItemID) {
					items = append(items, it.ItemID)
				}
			}
		}
		run, _, err := e.Release(ctx, pid, Request{Origin: store.RunOriginAuto, Items: items, Claim: ids})
		var refused *RefusedError
		switch {
		case errors.As(err, &refused): // recorded (refusedEvent); retried next round
		case err != nil:
			e.d.Log.Error("autopilot: release", "project", repo.Key, "err", err)
		default:
			e.d.Log.Info("autopilot: release started", "project", repo.Key, "run", run.ID, "fixes", len(ids))
		}
	}
}
