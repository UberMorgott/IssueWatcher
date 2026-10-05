package store

import (
	"errors"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// The inbox is written in the sync transaction for accepted projects only,
// once per source event (a re-sync or a comment edit adds nothing, the own
// comment never); consuming creates one fix run per item, a second event of
// the same item attaches to it, and a consumed event cannot be consumed again.
func TestAutopilotInbox(t *testing.T) {
	s, src, p := setup(t)
	ctx := t.Context()
	if _, err := s.ApplyItems(ctx, src, p.ID, nil, "me"); err != nil { // baseline
		t.Fatal(err)
	}
	s.SetInboxFilter(func(string) bool { return false })
	if _, err := s.ApplyItems(ctx, src, p.ID, []provider.Item{item("x", 9, true, t0)}, "me"); err != nil {
		t.Fatal(err)
	}
	if ev, _ := s.PendingInbox(ctx, 0); len(ev) != 0 {
		t.Fatalf("filtered project wrote %+v", ev)
	}
	var key string
	s.SetInboxFilter(func(k string) bool { key = k; return true })
	a := item("a", 1, true, t0)
	if _, err := s.ApplyItems(ctx, src, p.ID, []provider.Item{a}, "me"); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(time.Minute)
	a.UpdatedAt = t1
	a.Comments = []provider.Comment{{ExternalID: "c1", Author: "reporter", Body: "more", CreatedAt: t1, UpdatedAt: t1},
		{ExternalID: "c2", Author: "me", Body: "own", CreatedAt: t1, UpdatedAt: t1}}
	for range 2 { // the second pass is a re-sync
		if _, err := s.ApplyItems(ctx, src, p.ID, []provider.Item{a}, "me"); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := s.PendingInbox(ctx, 0)
	if err != nil || len(evs) != 2 || evs[0].Kind != string(EventNewIssue) || evs[1].Kind != string(EventNewComment) ||
		evs[1].SourceEvent != evs[0].SourceEvent+":c1" || key == "" {
		t.Fatalf("inbox %v %+v key %q", err, evs, key)
	}
	steps := []NewStep{{Step: "fix"}, {Step: "verify"}, {Step: "push"}}
	r, attached, err := s.ConsumeInbox(ctx, evs[0].ID, NewFixRun{ProjectID: p.ID, Manifest: []byte(`{}`), Steps: steps, Create: true})
	if err != nil || attached || r.Kind != RunKindFix || r.State != RunPending || r.Origin != RunOriginAuto {
		t.Fatalf("consume %v %v %+v", err, attached, r)
	}
	if _, _, err := s.ConsumeInbox(ctx, evs[0].ID, NewFixRun{ProjectID: p.ID, Create: true}); !errors.Is(err, ErrRunState) {
		t.Fatalf("second consume: %v", err)
	}
	r2, attached, err := s.ConsumeInbox(ctx, evs[1].ID, NewFixRun{ProjectID: p.ID, Create: false})
	if err != nil || !attached || r2.ID != r.ID {
		t.Fatalf("attach %v %v %+v", err, attached, r2)
	}
	if n, _ := s.ItemFixRuns(ctx, evs[0].ItemID); n != 1 {
		t.Fatalf("fix runs %d", n)
	}
	if ev, _ := s.PendingInbox(ctx, 0); len(ev) != 0 {
		t.Fatalf("pending after consume %+v", ev)
	}
}

// A release claims pushed fix runs in its create transaction, all or none;
// released / unclaimed move them on.
func TestClaimFixRuns(t *testing.T) {
	s, _, p := setup(t)
	ctx := t.Context()
	mk := func(state string) int64 {
		var id int64
		if err := s.db.QueryRowContext(ctx, `INSERT INTO autopilot_runs (kind, project_id, state, origin) VALUES ('fix', ?, ?, 'auto') RETURNING id`,
			p.ID, state).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	a, b, held := mk(RunPushed), mk(RunPushed), mk(RunHeld)
	if _, err := s.CreateReleaseRun(ctx, NewReleaseRun{ProjectID: p.ID, Steps: releaseSteps(), Claim: []int64{a, held}}); !errors.Is(err, ErrClaimRace) {
		t.Fatalf("claim of a held fix: %v", err)
	}
	if list, _ := s.ClaimableFixRuns(ctx, p.ID); len(list) != 2 {
		t.Fatalf("rolled back claim left %+v", list)
	}
	r, err := s.CreateReleaseRun(ctx, NewReleaseRun{ProjectID: p.ID, Steps: releaseSteps(), Claim: []int64{a, b}})
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := s.FixRunsOf(ctx, r.ID); len(list) != 2 {
		t.Fatalf("claimed %+v", list)
	}
	if n, err := s.UnclaimFixRuns(ctx, r.ID); err != nil || n != 2 {
		t.Fatalf("unclaim %d %v", n, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE autopilot_runs SET release_id = ? WHERE id IN (?, ?)`, r.ID, a, b); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SetFixRunsReleased(ctx, r.ID); err != nil || n != 2 {
		t.Fatalf("released %d %v", n, err)
	}
	if got, _ := s.Run(ctx, a); got.State != RunReleased || got.ReleaseID != r.ID {
		t.Fatalf("fix run %+v", got)
	}
}
