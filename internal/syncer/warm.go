package syncer

import (
	"context"
	"errors"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// spread is a stable per-key offset in [lo, lo+width): it staggers the work a
// restart finds overdue instead of firing it all at t0.
func spread(lo, width time.Duration, key int64) time.Duration {
	return lo + time.Duration(float64(width)*(float64(jitter(1000, key, 7))-900)/200)
}

// warmStart resumes the schedule from SQLite on the first Step: the stored
// source of the platform (the local account's for providers that know it
// without a network call, else the most recently reconciled one), its change
// check targets with their poll state, the next reconcile from the persisted
// reconciled_at and each target's next check from its checked_at. Overdue work
// is spread over the first minutes, never run at t0. Nothing stored (first run,
// a new account, a source never reconciled) leaves the syncer empty: the first
// Step bootstraps with a full reconcile as before.
func (s *Syncer) warmStart(ctx context.Context, now time.Time) {
	p := s.opts.Provider
	account := ""
	if la, ok := p.(provider.LocalAccounter); ok {
		a, signedIn := la.LocalAccount()
		if !signedIn {
			return
		}
		account = a
	}
	src, ok, err := s.opts.Store.LastSource(ctx, p.Platform(), account)
	if err != nil {
		s.opts.Log.Error("sync: warm start", "err", err)
		return
	}
	if !ok || src.ReconciledAt.IsZero() {
		return
	}
	rows, err := s.opts.Store.PollTargets(ctx, src.ID)
	if err != nil {
		s.opts.Log.Error("sync: warm start", "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.source, s.login = src.ID, src.Account
	s.reconciledOnce, s.verifyAccount = true, true
	s.status.SignedIn = true
	s.status.LastSync = src.ReconciledAt.UTC().Format(time.RFC3339)
	next := src.ReconciledAt.Add(jitter(s.plan_.Reconcile, src.ID, src.ReconciledAt.Unix()/60))
	if next.Before(now.Add(2 * time.Minute)) {
		next = now.Add(spread(2*time.Minute, 3*time.Minute, src.ID))
	}
	s.nextReconcile = next
	s.targets = make(map[int64]*target, len(rows))
	for _, r := range rows {
		t := &target{Project: r.Project, poll: r.Poll, activity: r.Activity}
		d := s.plan_.Idle
		if !t.activity.IsZero() && now.Sub(t.activity) <= s.plan_.ActiveWindow {
			d = s.plan_.Active
		}
		t.next = r.Checked.Add(max(jitter(d, t.ID, r.Checked.Unix()/60), s.minPoll))
		if r.Checked.IsZero() || t.next.Before(now.Add(5*time.Second)) {
			t.next = now.Add(spread(5*time.Second, 55*time.Second, t.ID))
		}
		s.targets[r.ID] = t
	}
	s.opts.Log.Info("sync: resumed from the store", "source", s.labelLocked(), "projects", len(rows),
		"reconciledAt", src.ReconciledAt.UTC().Format(time.RFC3339), "nextReconcile", next.UTC().Format(time.RFC3339))
}

// accountConfirmed is true once the account of a resumed source is confirmed
// by the provider (the first change check after a warm start asks Account
// once). Another account, signed out or a refused session force a reconcile,
// which handles each as a fresh start does; a transient failure postpones the
// due checks by a minute.
func (s *Syncer) accountConfirmed(ctx context.Context, now time.Time) bool {
	s.mu.Lock()
	verify, login := s.verifyAccount, s.login
	due := false
	for _, t := range s.targets {
		if !t.next.After(now) {
			due = true
			break
		}
	}
	s.mu.Unlock()
	if !verify {
		return true
	}
	if !due {
		return false // nothing to check yet: ask when the first check is due
	}
	got, err := s.opts.Provider.Account(ctx)
	switch {
	case err == nil && got == login:
		s.mu.Lock()
		s.verifyAccount = false
		s.mu.Unlock()
		return true
	case err == nil, errors.Is(err, provider.ErrNotSignedIn), errors.Is(err, provider.ErrRelogin):
		s.opts.Log.Info("sync: stored account not confirmed; reconciling", "stored", login, "account", got, "err", err)
		s.mu.Lock()
		s.verifyAccount, s.forceReconcile = false, true
		s.nextReconcile = now
		s.targets = map[int64]*target{}
		s.mu.Unlock()
		return false
	default:
		if ctx.Err() == nil {
			s.opts.Log.Warn("sync: confirm account", "err", err)
		}
		s.afterError(err, now)
		s.mu.Lock()
		for _, t := range s.targets {
			if !t.next.After(now) {
				t.next = now.Add(time.Minute)
			}
		}
		s.mu.Unlock()
		return false
	}
}
