package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// fakeItems records replies and closes (FindReply reads them back).
type fakeItems struct {
	mu      sync.Mutex
	replies map[int64][]string
	closed  map[int64]int
}

func (f *fakeItems) CanReply(context.Context, int64) bool { return true }

func (f *fakeItems) Reply(_ context.Context, item int64, body string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[item] = append(f.replies[item], body)
	return fmt.Sprintf("https://x/%d#c%d", item, len(f.replies[item])), nil
}

func (f *fakeItems) FindReply(_ context.Context, item int64, marker string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, b := range f.replies[item] {
		if strings.Contains(b, marker) {
			return fmt.Sprintf("https://x/%d#c%d", item, i+1), true, nil
		}
	}
	return "", false, nil
}

func (f *fakeItems) CloseItem(_ context.Context, item int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed[item]++
	return nil
}

func (f *fakeItems) ItemClosed(_ context.Context, item int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed[item] > 0, nil
}

// fakeFixer is the coder: AutopilotFix queues the job like the runner and
// "runs" it at once: a commit "Refs #N" in the folder, the job under review
// with the direct fix's facts.
type fakeFixer struct {
	e       *tenv
	mu      sync.Mutex
	fixes   int
	closing bool // commit with a closing keyword instead
	big     bool // a three-line change
	// rules: "" = a matching fix rule; "none" = no fix rule of the project;
	// "filtered" = the project has fix rules, none matches the item.
	rules     string
	needsInfo bool // the job ends needs_info without a commit
}

func (f *fakeFixer) AutopilotMatch(context.Context, int64, string) (bool, bool, error) {
	switch f.rules {
	case "none":
		return false, false, nil
	case "filtered":
		return false, true, nil
	}
	return true, true, nil
}

func (f *fakeFixer) AutopilotFix(ctx context.Context, item int64, event string, triaged bool) (store.AutomationEntry, *store.Job, error) {
	if m, _, _ := f.AutopilotMatch(ctx, item, event); !m && !triaged {
		return store.AutomationEntry{ItemID: item, Reason: "no_fix_rule"}, nil, nil
	}
	e := f.e
	entry, j, err := e.st.Automate(ctx, store.AutomationRequest{At: time.Now(), ItemID: item, Event: event, RuleID: store.RuleAutopilot,
		Flow: config.FlowFix, TotalCap: 10, MaxAttempts: 3})
	if err != nil || j == nil {
		return entry, j, err
	}
	f.mu.Lock()
	f.fixes++
	n := f.fixes
	closing, big, needsInfo := f.closing, f.big, f.needsInfo
	f.mu.Unlock()
	if needsInfo {
		head := git(e.t, e.folder, "rev-parse", "HEAD")
		res, _ := json.Marshal(map[string]any{"mode": "direct", "agent": map[string]any{"summary": "Which save triggers it?"},
			"local": map[string]any{"dir": e.folder, "branch": "main", "startSha": head, "headSha": head, "commits": []any{}, "outcome": "needs_info"}})
		nj, err := e.st.UpdateJob(ctx, j.ID, nil, store.JobChange{State: new(store.JobNeedsReview), Result: res})
		return entry, &nj, err
	}
	start := git(e.t, e.folder, "rev-parse", "HEAD")
	body := fmt.Sprintf("-- v1 fixed\n-- fix %d\n", n)
	if big {
		body += "-- a\n-- b\n"
	}
	write(e.t, filepath.Join(e.folder, "control.lua"), body)
	msg := "Fix the crash on save\n\nRefs #1"
	if closing {
		msg = "Fix the crash on save\n\nFixes #1"
	}
	git(e.t, e.folder, "commit", "-q", "-am", msg)
	head := git(e.t, e.folder, "rev-parse", "HEAD")
	res, _ := json.Marshal(map[string]any{"mode": "direct", "local": map[string]any{"dir": e.folder, "branch": "main",
		"startSha": start, "headSha": head, "commits": []map[string]any{{"sha": head, "subject": "Fix the crash on save", "refs": !closing, "fixes": closing}},
		"fixesRef": closing, "refsRef": !closing, "outcome": "fixed_local", "autopilot": true}})
	nj, err := e.st.UpdateJob(ctx, j.ID, nil, store.JobChange{State: new(store.JobNeedsReview), Result: res})
	return entry, &nj, err
}

type chain struct {
	*tenv
	items *fakeItems
	fixer *fakeFixer
	src   int64
	item  int64
	shift time.Duration
	tmu   sync.Mutex
}

// newChain is the env with autopilot fully on for the code project, an inbox,
// fake coder / item actions, and an issue #1 synced after the baseline.
func newChain(t *testing.T) *chain {
	t.Helper()
	c := &chain{tenv: newEnv(t), items: &fakeItems{replies: map[int64][]string{}, closed: map[int64]int{}}}
	c.fixer = &fakeFixer{e: c.tenv}
	pa := c.cfg.Agents.Projects[codeKey]
	pa.Autopilot.AutoFix, pa.Autopilot.AutoPush, pa.Autopilot.AutoRelease = true, true, true
	pa.Autopilot.AutoReply, pa.Autopilot.AutoClose, pa.Autopilot.CoalesceMinutes = true, true, 1
	c.cfg.Agents.Projects[codeKey] = pa
	c.deps.Items, c.deps.Fixer = c.items, c.fixer
	c.deps.Now = func() time.Time { c.tmu.Lock(); defer c.tmu.Unlock(); return time.Now().Add(c.shift) }
	c.st.SetInboxFilter(func(key string) bool { return key == codeKey })
	ctx := t.Context()
	var err error
	if c.src, err = c.st.UpsertSource(ctx, "github", "me"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.st.ApplyItems(ctx, c.src, c.codeID, nil, "me"); err != nil { // first sync: baseline
		t.Fatal(err)
	}
	evs, err := c.st.ApplyItems(ctx, c.src, c.codeID, []provider.Item{c.issue()}, "me")
	if err != nil || len(evs) != 1 {
		t.Fatalf("sync %v %+v", err, evs)
	}
	c.item = evs[0].ItemID
	return c
}

func (c *chain) issue(comments ...provider.Comment) provider.Item {
	at := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	up := at
	if len(comments) > 0 {
		up = comments[len(comments)-1].UpdatedAt
	}
	return provider.Item{ExternalID: "I_1", Number: 1, Title: "Crash on save", URL: "https://github.com/octo/mod/issues/1",
		Author: "reporter", Open: true, CreatedAt: at, UpdatedAt: up, Comments: comments}
}

func (c *chain) later(d time.Duration) {
	c.tmu.Lock()
	c.shift += d
	c.tmu.Unlock()
}

// tick runs one autopilot round and waits for what it started.
func (c *chain) tick(en *Engine) {
	en.Tick(context.Background())
	en.Wait()
}

func (c *chain) fixRun() store.Run {
	c.t.Helper()
	runs, err := c.st.Runs(context.Background(), store.RunFilter{Kind: store.RunKindFix})
	if err != nil || len(runs) != 1 {
		c.t.Fatalf("fix runs %v %+v", err, runs)
	}
	return runs[0]
}

func (c *chain) releaseRun() store.Run {
	c.t.Helper()
	runs, err := c.st.Runs(context.Background(), store.RunFilter{Kind: store.RunKindRelease})
	if err != nil || len(runs) != 1 {
		c.t.Fatalf("release runs %v %+v", err, runs)
	}
	return runs[0]
}

func (c *chain) eventKinds() []string {
	evs, _ := c.st.Events(context.Background(), false, 0)
	var out []string
	for _, ev := range slices.Backward(evs) {
		out = append(out, ev.Kind)
	}
	return out
}

// The whole chain with fake platforms: a new issue → inbox → fix run (job
// "Refs #N") → gate → push from the mirror → coalescing timer → release run
// (claims the fix) → publish → reply (version + links) → close; then the own
// reply and the reporter's thanks start nothing.
func TestAutopilotChainEndToEnd(t *testing.T) {
	c := newChain(t)
	en := c.engine(false)
	c.tick(en) // inbox → fix run → job queued (and "done" by the fake coder)
	c.tick(en) // job judged → verify → push → pushed
	fr := c.fixRun()
	if fr.State != store.RunPushed || fr.Origin != store.RunOriginAuto {
		t.Fatalf("fix run %+v steps %+v", fr, c.steps(fr.ID))
	}
	head := git(t, c.folder, "rev-parse", "HEAD")
	if got := git(t, c.bare, "rev-parse", "main"); got != head {
		t.Fatalf("remote main %s, fix head %s", got, head)
	}
	if n := c.git.count("refs/heads/main"); n != 1 {
		t.Fatalf("fix pushes %d", n)
	}
	var fm FixManifest
	_ = json.Unmarshal(fr.Manifest, &fm)
	if j, err := c.st.Job(t.Context(), fm.JobID); err != nil || j.State != store.JobDone || !strings.Contains(string(j.Result), `"pushed":true`) {
		t.Fatalf("job after push %v %+v", err, j)
	}

	c.tick(en) // coalesce: not due yet (1 min after the push)
	if runs, _ := c.st.Runs(t.Context(), store.RunFilter{Kind: store.RunKindRelease}); len(runs) != 0 {
		t.Fatalf("released before the coalescing window: %+v", runs)
	}
	c.later(2 * time.Minute)
	c.tick(en) // due → release run (auto) claims the fix and runs to done
	rr := c.releaseRun()
	if rr.State != store.RunDone || rr.Origin != store.RunOriginAuto || rr.Version != "1.0.1" {
		t.Fatalf("release %+v steps %+v", rr, c.steps(rr.ID))
	}
	id := strconv.FormatInt(c.item, 10)
	steps := c.steps(rr.ID)
	if steps["reply:"+id].State != store.StepSent || steps["close:"+id].State != store.StepSent {
		t.Fatalf("reply/close %+v / %+v", steps["reply:"+id], steps["close:"+id])
	}
	if fr = c.fixRun(); fr.State != store.RunReleased || fr.ReleaseID != rr.ID {
		t.Fatalf("fix run after release %+v", fr)
	}
	c.items.mu.Lock()
	replies, closes := c.items.replies[c.item], c.items.closed[c.item]
	c.items.mu.Unlock()
	if len(replies) != 1 || closes != 1 || !strings.HasPrefix(replies[0], "Fixed in v1.0.1.") ||
		!strings.Contains(replies[0], "https://www.nexusmods.com/wartales/mods/202") || !strings.Contains(replies[0], "Factorio Mod Portal") ||
		!strings.Contains(replies[0], "issuewatcher:reply:") {
		t.Fatalf("replies %q closes %d", replies, closes)
	}
	if n := c.git.count("refs/heads/main"); n != 2 { // the fix + the bump
		t.Fatalf("pushes %d", n)
	}
	kinds := strings.Join(c.eventKinds(), ",")
	for _, want := range []string{"fix.started", "fix.pushed", "release.done", "reply.posted", "issue.closed"} {
		if !strings.Contains(kinds, want) {
			t.Errorf("event %s missing in %s", want, kinds)
		}
	}

	// The own reply synced back, then the reporter's "thanks": no new fix run.
	own := provider.Comment{ExternalID: "C_1", Author: "me", Body: replies[0], CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if evs, err := c.st.ApplyItems(t.Context(), c.src, c.codeID, []provider.Item{c.issue(own)}, "me"); err != nil || len(evs) != 0 {
		t.Fatalf("own reply → events %v %+v", err, evs)
	}
	thanks := provider.Comment{ExternalID: "C_2", Author: "reporter", Body: "thanks!", CreatedAt: time.Now().Add(time.Second), UpdatedAt: time.Now().Add(time.Second)}
	if _, err := c.st.ApplyItems(t.Context(), c.src, c.codeID, []provider.Item{c.issue(own, thanks)}, "me"); err != nil {
		t.Fatal(err)
	}
	c.later(time.Hour)
	c.tick(en)
	if fr := c.fixRun(); fr.State != store.RunReleased { // still exactly one fix run
		t.Fatalf("fix run %+v", fr)
	}
	if pending, _ := c.st.PendingInbox(t.Context(), 0); len(pending) != 0 {
		t.Fatalf("inbox %+v", pending)
	}
	if c.fixer.fixes != 1 || len(c.items.replies[c.item]) != 1 {
		t.Fatalf("fixes %d replies %d", c.fixer.fixes, len(c.items.replies[c.item]))
	}
}

// crashHook aborts the first external send of stepName at point (an app crash).
func crashHook(en *Engine, step, point string) *bool {
	crashed := false
	var mu sync.Mutex
	en.hook = func(p string, st store.Step) error {
		mu.Lock()
		defer mu.Unlock()
		if !crashed && p == point && stepName(st) == step {
			crashed = true
			return errors.New("simulated crash")
		}
		return nil
	}
	return &crashed
}

// A crash right after the fix push and right after the reply: a fresh engine
// (app restart) probes, finds both done and finishes; nothing is sent twice.
func TestAutopilotChainCrashResume(t *testing.T) {
	c := newChain(t)
	en := c.engine(false)
	crashed := crashHook(en, StepPush, "after")
	c.tick(en)
	c.tick(en)
	if !*crashed {
		t.Fatal("no crash at the fix push")
	}
	fr := c.fixRun()
	if st := c.steps(fr.ID)["push"]; st.State != store.StepSending || fr.State != store.RunRunning {
		t.Fatalf("after the crash: run %s push %+v", fr.State, st)
	}

	// Restart: the push is probed (it happened) → pushed; release; crash after the reply.
	en2 := c.engine(false)
	id := strconv.FormatInt(c.item, 10)
	replyCrash := crashHook(en2, StepReply+":"+id, "after")
	c.tick(en2)
	if fr = c.fixRun(); fr.State != store.RunPushed || c.git.count("refs/heads/main") != 1 {
		t.Fatalf("after restart: %+v pushes %d", fr, c.git.count("refs/heads/main"))
	}
	c.later(2 * time.Minute)
	c.tick(en2)
	if !*replyCrash {
		t.Fatal("no crash at the reply")
	}
	rr := c.releaseRun()
	if st := c.steps(rr.ID)["reply:"+id]; st.State != store.StepSending || rr.State != store.RunRunning {
		t.Fatalf("after the reply crash: run %s reply %+v", rr.State, st)
	}

	en3 := c.engine(false)
	if err := en3.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	en3.Wait()
	if rr = c.releaseRun(); rr.State != store.RunDone {
		t.Fatalf("release after restart %+v steps %+v", rr, c.steps(rr.ID))
	}
	c.items.mu.Lock()
	defer c.items.mu.Unlock()
	if len(c.items.replies[c.item]) != 1 || c.items.closed[c.item] != 1 || c.git.count("refs/heads/main") != 2 {
		t.Fatalf("replies %d closes %d pushes %d", len(c.items.replies[c.item]), c.items.closed[c.item], c.git.count("refs/heads/main"))
	}
}

// Rails: a fix commit that closes the issue (Fixes #N) is held before the
// push; a fix over maxDiffLines is held for review; the kill switch stops the
// inbox and the push.
func TestAutopilotFixRails(t *testing.T) {
	t.Run("closing keyword", func(t *testing.T) {
		c := newChain(t)
		c.fixer.closing = true
		en := c.engine(false)
		c.tick(en)
		c.tick(en)
		if fr := c.fixRun(); fr.State != store.RunHeld || fr.HeldReason != HeldClosingKeyword || c.git.count("refs/heads/main") != 0 {
			t.Fatalf("%+v", fr)
		}
	})
	t.Run("diff limit", func(t *testing.T) {
		c := newChain(t)
		pa := c.cfg.Agents.Projects[codeKey]
		pa.Autopilot.MaxDiffLines = 2
		c.fixer.big = true
		c.cfg.Agents.Projects[codeKey] = pa
		en := c.engine(false)
		c.tick(en)
		c.tick(en)
		if fr := c.fixRun(); fr.State != store.RunHeld || fr.HeldReason != HeldDiffTooBig || c.git.count("refs/heads/main") != 0 {
			t.Fatalf("%+v steps %+v", fr, c.steps(fr.ID))
		}
		evs, _ := c.st.Events(t.Context(), true, 0)
		if len(evs) == 0 || evs[0].Kind != "fix.held" || evs[0].Severity != store.SeverityAttention || evs[0].ItemID != c.item {
			t.Fatalf("events %+v", evs)
		}
	})
	t.Run("review paths", func(t *testing.T) {
		if !needsReview(".github/workflows/ci.yml", nil) || !needsReview("build.ps1", nil) || !needsReview("info.json", []string{"info.json"}) ||
			!needsReview("go.mod", nil) || !needsReview("sub/.gitmodules", nil) || needsReview("control.lua", []string{"info.json"}) {
			t.Fatal("needsReview")
		}
	})
	t.Run("paused", func(t *testing.T) {
		c := newChain(t)
		c.cfg.Agents.Autopilot.Paused = true
		en := c.engine(false)
		c.tick(en)
		if runs, _ := c.st.Runs(t.Context(), store.RunFilter{Kind: store.RunKindFix}); len(runs) != 0 {
			t.Fatalf("paused: %+v", runs)
		}
		if pending, _ := c.st.PendingInbox(t.Context(), 0); len(pending) != 1 {
			t.Fatalf("paused inbox %+v", pending)
		}
	})
}
