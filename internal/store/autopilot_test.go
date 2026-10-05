package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

func releaseSteps() []NewStep {
	return []NewStep{
		{Step: "claim"}, {Step: "bump"}, {Step: "build"}, {Step: "gh_release"},
		{Step: "publish", Target: "nexus:1"}, {Step: "available", Target: "nexus:1"},
	}
}

func stepKeys(steps []Step) []string {
	out := []string{}
	for _, st := range steps {
		k := st.Step
		if st.Target != "" {
			k += ":" + st.Target
		}
		out = append(out, k)
	}
	return out
}

func TestCreateReleaseRunItemsAndSteps(t *testing.T) {
	s, src, p := setup(t)
	ctx := t.Context()
	if _, err := s.ApplyItems(ctx, src, p.ID, []provider.Item{item("a", 1, true, t0), item("b", 2, true, t0)}, ""); err != nil {
		t.Fatal(err)
	}
	var a, b int64
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT id FROM items WHERE number = 1), (SELECT id FROM items WHERE number = 2)`).Scan(&a, &b); err != nil {
		t.Fatal(err)
	}
	steps := append(releaseSteps(), NewStep{Step: "smoke", State: StepSkipped, Error: "no smoke adapter yet (Phase 2)"})
	r, err := s.CreateReleaseRun(ctx, NewReleaseRun{
		ProjectID: p.ID, Origin: RunOriginMCP, Version: "1.2.3", Manifest: []byte(`{"base":"v1.2.2"}`),
		Items: []RunItem{{ItemID: b, Role: RunItemDuplicate}, {ItemID: a}}, Steps: steps,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != RunKindRelease || r.State != RunPending || r.Origin != RunOriginMCP || r.Version != "1.2.3" || string(r.Manifest) != `{"base":"v1.2.2"}` {
		t.Fatalf("run %+v", r)
	}
	items, err := s.RunItems(ctx, r.ID)
	if err != nil || len(items) != 2 || items[0] != (RunItem{a, RunItemPrimary}) || items[1] != (RunItem{b, RunItemDuplicate}) {
		t.Fatalf("items %+v %v", items, err)
	}
	got, err := s.RunSteps(ctx, r.ID)
	if err != nil || len(got) != 7 || got[6].State != StepSkipped || got[6].Error == "" || got[0].Seq != 1 || got[6].Seq != 7 {
		t.Fatalf("steps %+v %v", got, err)
	}

	// gh_asset steps are known after the build: inserted after gh_release, before publish.
	n, err := s.AddSteps(ctx, r.ID, "gh_release", "", []NewStep{
		{Step: "gh_asset", Target: "mod_1.2.3.zip"}, {Step: "gh_asset", Target: "b.zip"}, {Step: "gh_release"},
	})
	if err != nil || n != 2 {
		t.Fatalf("add steps n=%d %v", n, err)
	}
	if n, err := s.AddSteps(ctx, r.ID, "gh_release", "", []NewStep{{Step: "gh_asset", Target: "b.zip"}}); err != nil || n != 0 {
		t.Fatalf("re-add n=%d %v", n, err)
	}
	got, _ = s.RunSteps(ctx, r.ID)
	want := []string{"claim", "bump", "build", "gh_release", "gh_asset:mod_1.2.3.zip", "gh_asset:b.zip", "publish:nexus:1", "available:nexus:1", "smoke"}
	if keys := stepKeys(got); len(keys) != len(want) {
		t.Fatalf("order %v", keys)
	} else {
		for i := range want {
			if keys[i] != want[i] || got[i].Seq != i+1 {
				t.Fatalf("order %v (seq %d at %d)", keys, got[i].Seq, i)
			}
		}
	}
	if _, err := s.AddSteps(ctx, 999, "", "", []NewStep{{Step: "x"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent run: %v", err)
	}
}

func TestReleaseRunOnePerProject(t *testing.T) {
	s, _, p := setup(t)
	ctx := t.Context()
	r, err := s.CreateReleaseRun(ctx, NewReleaseRun{ProjectID: p.ID, Steps: releaseSteps()})
	if err != nil || r.Origin != RunOriginManual {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := s.CreateReleaseRun(ctx, NewReleaseRun{ProjectID: p.ID}); !errors.Is(err, ErrRunBusy) {
		t.Fatalf("second run: %v", err)
	}
	reason := "check:push"
	held, err := s.UpdateRun(ctx, r.ID, []string{RunPending, RunRunning}, RunUpdate{State: RunHeld, HeldReason: &reason})
	if err != nil || held.State != RunHeld || held.HeldReason != reason {
		t.Fatalf("held %+v %v", held, err)
	}
	if _, err := s.CreateReleaseRun(ctx, NewReleaseRun{ProjectID: p.ID}); !errors.Is(err, ErrRunBusy) {
		t.Fatalf("held still busy: %v", err)
	}
	if un, err := s.UnfinishedRuns(ctx); err != nil || len(un) != 1 || un[0].ID != r.ID {
		t.Fatalf("unfinished %+v %v", un, err)
	}
	// CAS conflict: the run is held, not running.
	cur, err := s.UpdateRun(ctx, r.ID, []string{RunRunning}, RunUpdate{State: RunDone})
	if !errors.Is(err, ErrRunState) || cur.State != RunHeld {
		t.Fatalf("cas %+v %v", cur, err)
	}
	if _, err := s.UpdateRun(ctx, 999, []string{RunRunning}, RunUpdate{State: RunDone}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent: %v", err)
	}
	done, err := s.UpdateRun(ctx, r.ID, []string{RunHeld}, RunUpdate{State: RunDone})
	if err != nil || done.State != RunDone || done.HeldReason != "" {
		t.Fatalf("done %+v %v", done, err)
	}
	r2, err := s.CreateReleaseRun(ctx, NewReleaseRun{ProjectID: p.ID})
	if err != nil {
		t.Fatalf("after done: %v", err)
	}
	if runs, err := s.Runs(ctx, RunFilter{ProjectID: p.ID}); err != nil || len(runs) != 2 || runs[0].ID != r2.ID {
		t.Fatalf("runs %+v %v", runs, err)
	}
	if runs, err := s.Runs(ctx, RunFilter{State: RunDone, Limit: 5}); err != nil || len(runs) != 1 || runs[0].ID != r.ID {
		t.Fatalf("done runs %+v %v", runs, err)
	}
	if _, err := s.Run(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("run 999: %v", err)
	}
}

func TestReleaseRunCap(t *testing.T) {
	s, src, p := setup(t)
	ctx := t.Context()
	others, err := s.SyncProjects(ctx, src, []provider.Project{{ExternalID: "o/app", Name: "o/app", URL: "u"}, {ExternalID: "o/lib", Name: "o/lib", URL: "u"}})
	if err != nil || len(others) != 2 {
		t.Fatal(others, err)
	}
	var q int64
	for _, o := range others {
		if o.ID != p.ID {
			q = o.ID
		}
	}
	day := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	create := func(project int64, now time.Time) error {
		r, err := s.CreateReleaseRun(ctx, NewReleaseRun{ProjectID: project, Now: now, MaxPerProjectPerDay: 2, MaxGlobalPerDay: 3})
		if err != nil {
			return err
		}
		// A cancelled run still holds its slot.
		_, err = s.UpdateRun(ctx, r.ID, []string{RunPending}, RunUpdate{State: RunCancelled})
		return err
	}
	if err := create(p.ID, day); err != nil {
		t.Fatal(err)
	}
	if err := create(p.ID, day.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := create(p.ID, day.Add(2*time.Hour)); !errors.Is(err, ErrCapReached) {
		t.Fatalf("project cap: %v", err)
	}
	if err := create(q, day); err != nil {
		t.Fatal(err)
	}
	if err := create(q, day.Add(time.Hour)); !errors.Is(err, ErrCapReached) {
		t.Fatalf("global cap: %v", err)
	}
	// Next UTC day: fresh slots.
	if err := create(p.ID, time.Date(2026, 10, 6, 0, 0, 1, 0, time.UTC)); err != nil {
		t.Fatalf("next day: %v", err)
	}
}

func TestTransitionStepClaim(t *testing.T) {
	s, _, p := setup(t)
	ctx := t.Context()
	r, err := s.CreateReleaseRun(ctx, NewReleaseRun{ProjectID: p.ID, Steps: releaseSteps()})
	if err != nil {
		t.Fatal(err)
	}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		won     int
		lost    int
		failure error
	)
	start := make(chan struct{})
	for range 2 {
		wg.Go(func() {
			<-start
			_, err := s.TransitionStep(ctx, r.ID, "publish", "nexus:1", StepPending, StepSending, StepUpdate{IncAttempt: true})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrStepState):
				lost++
			default:
				failure = err
			}
		})
	}
	close(start)
	wg.Wait()
	if failure != nil || won != 1 || lost != 1 {
		t.Fatalf("won=%d lost=%d err=%v", won, lost, failure)
	}
	steps, _ := s.RunSteps(ctx, r.ID)
	st := steps[4]
	if st.State != StepSending || st.Attempt != 1 || st.StartedAt == "" || st.FinishedAt != "" {
		t.Fatalf("claimed %+v", st)
	}

	// CAS conflict returns the current row.
	cur, err := s.TransitionStep(ctx, r.ID, "publish", "nexus:1", StepPending, StepSending, StepUpdate{})
	if !errors.Is(err, ErrStepState) || cur.State != StepSending {
		t.Fatalf("cas %+v %v", cur, err)
	}
	ref, key := "file:42", "nexus:1:1.2.3:abc"
	sent, err := s.TransitionStep(ctx, r.ID, "publish", "nexus:1", StepSending, StepSent,
		StepUpdate{ExternalRef: &ref, IdemKey: &key, Request: []byte(`{"version":"1.2.3"}`)})
	if err != nil || sent.State != StepSent || sent.FinishedAt == "" || sent.ExternalRef != ref || sent.IdemKey != key || string(sent.Request) != `{"version":"1.2.3"}` {
		t.Fatalf("sent %+v %v", sent, err)
	}
	if _, err := s.TransitionStep(ctx, r.ID, "nope", "", StepPending, StepSending, StepUpdate{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent step: %v", err)
	}
	if _, err := s.TransitionStep(ctx, r.ID, "claim", "", StepPending, "bogus", StepUpdate{}); err == nil {
		t.Fatal("bad state accepted")
	}
}

func TestAutopilotEvents(t *testing.T) {
	s, _, p := setup(t)
	ctx := t.Context()
	old := autopilotEventKeep
	autopilotEventKeep = 3
	t.Cleanup(func() { autopilotEventKeep = old })

	var ids []int64
	for i, sev := range []string{"", SeverityAttention, SeverityInfo, SeverityAttention, ""} {
		e, err := s.AddEvent(ctx, AutopilotEvent{ProjectID: p.ID, Kind: "release_done", Severity: sev, Title: string(rune('a' + i))})
		if err != nil {
			t.Fatal(err)
		}
		if e.Severity == "" || e.At == "" || string(e.Detail) != "{}" || e.RunID != 0 {
			t.Fatalf("event %+v", e)
		}
		ids = append(ids, e.ID)
	}
	all, err := s.Events(ctx, false, 0)
	if err != nil || len(all) != 3 || all[0].ID != ids[4] || all[2].ID != ids[2] {
		t.Fatalf("pruned %+v %v", all, err)
	}
	if u, a, err := s.UnreadEventCount(ctx); err != nil || u != 3 || a != 1 {
		t.Fatalf("unread %d attention %d %v", u, a, err)
	}
	if n, err := s.MarkEventsRead(ctx, []int64{ids[3], ids[0]}, false); err != nil || n != 1 {
		t.Fatalf("mark n=%d %v", n, err)
	}
	unread, err := s.Events(ctx, true, 10)
	if err != nil || len(unread) != 2 || unread[0].ID != ids[4] {
		t.Fatalf("unread list %+v %v", unread, err)
	}
	if n, err := s.MarkEventsRead(ctx, nil, true); err != nil || n != 2 {
		t.Fatalf("mark all n=%d %v", n, err)
	}
	if u, a, err := s.UnreadEventCount(ctx); err != nil || u != 0 || a != 0 {
		t.Fatalf("after all %d %d %v", u, a, err)
	}
}

// Migration 016 on a database at 015 with data: the tables appear, rows stay.
func TestMigration016OnExistingDB(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateTo(ctx, db, 15); err != nil {
		t.Fatal(err)
	}
	src, err := New(db).UpsertSource(ctx, "github", "me")
	if err != nil {
		t.Fatal(err)
	}
	projects, err := seedProjects(ctx, db, src, []provider.Project{{ExternalID: "o/app", Name: "o/app", URL: "u"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 17 {
		t.Fatalf("version %d %v", version, err)
	}
	s := New(db)
	r, err := s.CreateReleaseRun(ctx, NewReleaseRun{ProjectID: projects[0].ID, Steps: releaseSteps()})
	if err != nil || r.ProjectID != projects[0].ID {
		t.Fatalf("%+v %v", r, err)
	}
}
