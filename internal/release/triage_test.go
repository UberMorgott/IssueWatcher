package release

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// fakeTriager returns the verdict set for an item (by issue number), else a
// fixable bug; it records the requests.
type fakeTriager struct {
	mu      sync.Mutex
	byNum   map[int]Classification
	numOf   func(item int64) int
	reqs    []ClassifyRequest
	failing int // fail this many calls first
}

func (f *fakeTriager) Classify(_ context.Context, req ClassifyRequest) (Classification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.failing > 0 {
		f.failing--
		return Classification{}, errors.New("agent down")
	}
	if c, ok := f.byNum[f.numOf(req.ItemID)]; ok {
		return c, nil
	}
	return Classification{Kind: KindBug, Actionable: true, Language: "en", Reason: "crash"}, nil
}

// fakeDrafter drafts with draft (nil = an error: the template is used).
type fakeDrafter struct {
	mu    sync.Mutex
	draft func(req DraftRequest) string
	reqs  []DraftRequest
}

func (f *fakeDrafter) DraftReply(_ context.Context, req DraftRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.draft == nil {
		return "", errors.New("drafter down")
	}
	return f.draft(req), nil
}

// ruDraft is a Russian release reply naming the version and every link once.
func ruDraft(req DraftRequest) string {
	switch req.Kind {
	case DraftReleased:
		var links []string
		for _, l := range req.Links {
			links = append(links, l.URL)
		}
		return "Спасибо! Исправлено в версии " + req.Version + ", скачать: " + strings.Join(links, " , ")
	case DraftQuestion:
		return "Да, это включается в настройках мода."
	case DraftFeedback:
		return "Спасибо, рад, что всё работает!"
	}
	return "Спасибо! Пришлите, пожалуйста, сохранение и лог."
}

type triageChain struct {
	*chain
	tr     *fakeTriager
	dr     *fakeDrafter
	items  map[int]int64 // issue number → item id
	all    []provider.Item
	pause  int
	collab map[int64]bool
}

// newTriageChain: the chain with auto-triage on, no fix rules needed
// (rules "none"), a drafter writing Russian, and issue #1 synced.
func newTriageChain(t *testing.T) *triageChain {
	t.Helper()
	c := &triageChain{chain: newChain(t), items: map[int]int64{}, collab: map[int64]bool{}}
	c.fixer.rules = "none"
	c.items[1] = c.item
	c.all = []provider.Item{c.issue()}
	c.tr = &fakeTriager{byNum: map[int]Classification{}, numOf: func(id int64) int {
		for n, x := range c.items {
			if x == id {
				return n
			}
		}
		return 0
	}}
	c.dr = &fakeDrafter{draft: ruDraft}
	c.deps.Triager, c.deps.Drafter = c.tr, c.dr
	c.deps.PauseProject = func(_ context.Context, project string) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		pa := c.cfg.Agents.Projects[project]
		pa.Autopilot.Enabled = false
		c.cfg.Agents.Projects[project] = pa
		c.pause++
		return nil
	}
	c.deps.Collaborator = func(_ context.Context, item int64) (bool, error) { return c.collab[item], nil }
	return c
}

// add syncs issue #n by author (a new_issue event) and returns its item id.
func (c *triageChain) add(n int, author, title string) int64 {
	c.t.Helper()
	at := time.Now()
	it := provider.Item{ExternalID: "I_" + strconv.Itoa(n), Number: n, Title: title, URL: fmt.Sprintf("https://github.com/octo/mod/issues/%d", n),
		Author: author, Open: true, CreatedAt: at, UpdatedAt: at}
	c.all = append(c.all, it)
	evs, err := c.st.ApplyItems(context.Background(), c.src, c.codeID, c.all, "me")
	if err != nil {
		c.t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Kind == store.EventNewIssue {
			c.items[n] = ev.ItemID
		}
	}
	if c.items[n] == 0 {
		c.t.Fatalf("issue #%d: no new_issue event in %+v", n, evs)
	}
	return c.items[n]
}

func (c *triageChain) runOf(item int64) store.Run {
	c.t.Helper()
	r, err := c.st.LatestFixRun(context.Background(), item)
	if err != nil {
		c.t.Fatalf("fix run of %d: %v", item, err)
	}
	return r
}

func (c *triageChain) replies(item int64) []string {
	c.items0().mu.Lock()
	defer c.items0().mu.Unlock()
	return append([]string(nil), c.chain.items.replies[item]...)
}

func (c *triageChain) items0() *fakeItems { return c.chain.items }

// Auto-fix with triage needs no rule: a bug starts a fix and is pushed; the
// project's own fix rules still filter; questions are answered, details are
// asked for (held, and the reporter's answer looks at it again), feature
// requests and spam are ignored without a reply.
func TestTriageClasses(t *testing.T) {
	t.Run("bug without rules", func(t *testing.T) {
		c := newTriageChain(t)
		en := c.engine(false)
		c.tick(en)
		c.tick(en)
		fr := c.fixRun()
		if fr.State != store.RunPushed || c.fixer.fixes != 1 {
			t.Fatalf("run %+v fixes %d steps %+v", fr, c.fixer.fixes, c.steps(fr.ID))
		}
		if st := c.steps(fr.ID)["triage"]; st.State != store.StepSent || !strings.Contains(st.ExternalRef, `"kind":"bug"`) {
			t.Fatalf("triage step %+v", st)
		}
	})
	t.Run("rules filter", func(t *testing.T) {
		c := newTriageChain(t)
		c.fixer.rules = "filtered"
		en := c.engine(false)
		c.tick(en)
		if fr := c.fixRun(); fr.State != store.RunIgnored || c.fixer.fixes != 0 {
			t.Fatalf("run %+v fixes %d", fr, c.fixer.fixes)
		}
	})
	t.Run("without triage a rule is needed", func(t *testing.T) {
		c := newTriageChain(t)
		pa := c.cfg.Agents.Projects[codeKey]
		pa.Autopilot.AutoTriage = false
		c.cfg.Agents.Projects[codeKey] = pa
		en := c.engine(false)
		c.tick(en)
		if runs, _ := c.st.Runs(t.Context(), store.RunFilter{Kind: store.RunKindFix}); len(runs) != 0 || len(c.tr.reqs) != 0 {
			t.Fatalf("no rule, no triage: runs %+v triage calls %d", runs, len(c.tr.reqs))
		}
	})
	t.Run("question", func(t *testing.T) {
		c := newTriageChain(t)
		c.tr.byNum[1] = Classification{Kind: KindQuestion, Language: "ru", Reason: "asks how"}
		en := c.engine(false)
		c.tick(en)
		fr := c.fixRun()
		rs := c.replies(c.item)
		if fr.State != store.RunAnswered || len(rs) != 1 || !strings.HasPrefix(rs[0], "Да, это включается") || c.fixer.fixes != 0 {
			t.Fatalf("run %+v replies %q fixes %d", fr, rs, c.fixer.fixes)
		}
		if !strings.Contains(rs[0], "issuewatcher:reply:fix:") || c.dr.reqs[0].Language != "ru" || c.dr.reqs[0].Kind != DraftQuestion {
			t.Fatalf("reply %q draft req %+v", rs[0], c.dr.reqs[0])
		}
	})
	t.Run("needs info, then the reporter answers", func(t *testing.T) {
		c := newTriageChain(t)
		c.tr.byNum[1] = Classification{Kind: KindBug, Actionable: false, Missing: "the save file", Language: "ru"}
		en := c.engine(false)
		c.tick(en)
		fr := c.fixRun()
		if fr.State != store.RunHeld || fr.HeldReason != HeldNeedsInfo || len(c.replies(c.item)) != 1 || c.dr.reqs[0].Notes != "the save file" {
			t.Fatalf("run %+v replies %q", fr, c.replies(c.item))
		}
		// The reporter adds the details: the held run ends, a new one triages again (now a bug) and fixes.
		delete(c.tr.byNum, 1)
		cm := provider.Comment{ExternalID: "C_9", Author: "reporter", Body: "here is the save", CreatedAt: time.Now(), UpdatedAt: time.Now()}
		c.all[0] = c.issue(cm)
		if _, err := c.st.ApplyItems(t.Context(), c.src, c.codeID, c.all, "me"); err != nil {
			t.Fatal(err)
		}
		c.tick(en)
		c.tick(en)
		if old := c.run(fr.ID); old.State != store.RunCancelled {
			t.Fatalf("old run %+v", old)
		}
		if nr := c.runOf(c.item); nr.ID == fr.ID || nr.State != store.RunPushed || c.fixer.fixes != 1 {
			t.Fatalf("new run %+v fixes %d", nr, c.fixer.fixes)
		}
	})
	t.Run("fix agent: not a bug → short reply, no release", func(t *testing.T) {
		c := newTriageChain(t)
		c.fixer.nonBug = "feedback"
		en := c.engine(false)
		c.tick(en)
		c.tick(en)
		fr := c.fixRun()
		rs := c.replies(c.item)
		if fr.State != store.RunAnswered || len(rs) != 1 || !strings.HasPrefix(rs[0], "Спасибо, рад") || c.dr.reqs[0].Kind != DraftFeedback {
			t.Fatalf("run %+v replies %q reqs %+v steps %+v", fr, rs, c.dr.reqs, c.steps(fr.ID))
		}
		if st := c.steps(fr.ID)["push"]; st.State != store.StepSkipped {
			t.Fatalf("push step %+v", st)
		}
	})
	t.Run("triage feedback → short reply", func(t *testing.T) {
		c := newTriageChain(t)
		c.tr.byNum[1] = Classification{Kind: KindFeedback, Language: "ru", Reason: "says thanks"}
		en := c.engine(false)
		c.tick(en)
		fr := c.fixRun()
		if rs := c.replies(c.item); fr.State != store.RunAnswered || len(rs) != 1 || c.fixer.fixes != 0 || c.dr.reqs[0].Kind != DraftFeedback {
			t.Fatalf("run %+v replies %q fixes %d", fr, rs, c.fixer.fixes)
		}
	})
	t.Run("slop draft → template", func(t *testing.T) {
		c := newTriageChain(t)
		c.tr.byNum[1] = Classification{Kind: KindFeedback, Language: "en", Reason: "says thanks"}
		c.dr.draft = func(DraftRequest) string { return "Thank you so much for your kind words! Happy gaming!" }
		en := c.engine(false)
		c.tick(en)
		if rs := c.replies(c.item); len(rs) != 1 || !strings.HasPrefix(rs[0], "Thanks, glad it works for you!") {
			t.Fatalf("replies %q", rs)
		}
	})
	t.Run("fix agent needs info → asks", func(t *testing.T) {
		c := newTriageChain(t)
		c.fixer.needsInfo = true
		en := c.engine(false)
		c.tick(en)
		c.tick(en)
		fr := c.fixRun()
		rs := c.replies(c.item)
		if fr.State != store.RunHeld || fr.HeldReason != HeldNeedsInfo || len(rs) != 1 || !strings.Contains(c.dr.reqs[0].Notes, "Which save") {
			t.Fatalf("run %+v replies %q reqs %+v", fr, rs, c.dr.reqs)
		}
	})
	for _, kind := range []string{KindFeature, KindSpam, KindOther} {
		t.Run(kind, func(t *testing.T) {
			c := newTriageChain(t)
			c.tr.byNum[1] = Classification{Kind: kind}
			en := c.engine(false)
			c.tick(en)
			if fr := c.fixRun(); fr.State != store.RunIgnored || len(c.replies(c.item)) != 0 || c.fixer.fixes != 0 {
				t.Fatalf("run %+v", fr)
			}
		})
	}
	t.Run("triage retried then held", func(t *testing.T) {
		c := newTriageChain(t)
		c.tr.failing = 99
		en := c.engine(false)
		for range maxTriageAttempts {
			c.tick(en)
		}
		if fr := c.fixRun(); fr.State != store.RunHeld || fr.HeldReason != HeldTriageFailed {
			t.Fatalf("run %+v", fr)
		}
	})
}

// A duplicate of an item being fixed joins that run (one fix, both replied
// after the release); a duplicate of an already released fix is answered at
// once with that release.
func TestTriageDuplicates(t *testing.T) {
	c := newTriageChain(t)
	en := c.engine(false)
	c.tick(en) // #1 → triage → fix job
	dup := c.add(2, "other", "Game crashes when saving")
	c.tr.byNum[2] = Classification{Kind: KindDuplicate, DuplicateOf: c.item, Actionable: true, Language: "ru"}
	c.tick(en) // #1 pushed; #2 → duplicate of #1's open run
	first := c.runOf(c.item)
	if d := c.runOf(dup); d.State != store.RunDuplicate {
		t.Fatalf("duplicate run %+v steps %+v", d, c.steps(d.ID))
	}
	its, _ := c.st.RunItems(t.Context(), first.ID)
	if len(its) != 2 || its[1].ItemID != dup || its[1].Role != store.RunItemDuplicate {
		t.Fatalf("original run items %+v", its)
	}
	if len(c.tr.reqs) < 2 || !hasCandidate(c.tr.reqs[1].Candidates, c.item, "fixing") {
		t.Fatalf("candidates %+v", c.tr.reqs)
	}
	c.later(2 * time.Minute)
	c.tick(en) // release v1.0.1 answers both
	rr := c.releaseRun()
	if rr.State != store.RunDone || c.fixer.fixes != 1 {
		t.Fatalf("release %+v fixes %d", rr, c.fixer.fixes)
	}
	for _, id := range []int64{c.item, dup} {
		if rs := c.replies(id); len(rs) != 1 || !strings.Contains(rs[0], "1.0.1") {
			t.Fatalf("item %d replies %q", id, rs)
		}
	}

	// #3 duplicates the released #1: answered at once with v1.0.1 and its links, no fix.
	late := c.add(3, "third", "Crash on save still?")
	c.tr.byNum[3] = Classification{Kind: KindDuplicate, DuplicateOf: c.item, Actionable: true, Language: "ru"}
	c.tick(en)
	d := c.runOf(late)
	rs := c.replies(late)
	if d.State != store.RunDuplicate || len(rs) != 1 || !strings.HasPrefix(rs[0], "Спасибо! Исправлено в версии 1.0.1") ||
		!strings.Contains(rs[0], "https://www.nexusmods.com/wartales/mods/202") || c.fixer.fixes != 1 {
		t.Fatalf("late duplicate %+v replies %q fixes %d", d, rs, c.fixer.fixes)
	}
}

func hasCandidate(cs []Candidate, item int64, status string) bool {
	for _, c := range cs {
		if c.ItemID == item && c.Status == status {
			return true
		}
	}
	return false
}

// releaseFirst runs issue #1 through to the release v1.0.1.
func (c *triageChain) releaseFirst(en *Engine) {
	c.t.Helper()
	c.tick(en)
	c.tick(en)
	c.later(2 * time.Minute)
	c.tick(en)
	if rr := c.releaseRun(); rr.State != store.RunDone {
		c.t.Fatalf("release %+v", rr)
	}
}

// The regression breaker: one report of a regression of the latest release
// only notes it; a second reporter (or a collaborator alone) switches the
// project's autopilot off.
func TestRegressionBreaker(t *testing.T) {
	reg := Classification{Kind: KindBug, Actionable: true, Regression: true, Language: "en"}
	t.Run("two reporters", func(t *testing.T) {
		c := newTriageChain(t)
		en := c.engine(false)
		c.releaseFirst(en)
		c.tr.byNum[2], c.tr.byNum[3] = reg, reg
		c.add(2, "alice", "Broken since 1.0.1")
		c.tick(en)
		if c.pause != 0 || !strings.Contains(strings.Join(c.eventKinds(), ","), "regression.reported") {
			t.Fatalf("one report: pause %d events %v", c.pause, c.eventKinds())
		}
		c.add(3, "bob", "1.0.1 broke my game")
		c.tick(en)
		if c.pause != 1 || c.cfg.Agents.AutopilotFor(codeKey).Enabled {
			t.Fatalf("two reporters: pause %d", c.pause)
		}
		if !strings.Contains(strings.Join(c.eventKinds(), ","), "autopilot.regression_paused") {
			t.Fatalf("events %v", c.eventKinds())
		}
	})
	t.Run("same reporter twice", func(t *testing.T) {
		c := newTriageChain(t)
		en := c.engine(false)
		c.releaseFirst(en)
		c.tr.byNum[2], c.tr.byNum[3] = reg, reg
		c.add(2, "alice", "Broken since 1.0.1")
		c.add(3, "alice", "Still broken")
		c.tick(en)
		c.tick(en)
		if c.pause != 0 {
			t.Fatalf("pause %d", c.pause)
		}
	})
	t.Run("collaborator", func(t *testing.T) {
		c := newTriageChain(t)
		en := c.engine(false)
		c.releaseFirst(en)
		c.tr.byNum[2] = reg
		id := c.add(2, "teammate", "1.0.1 regressed saving")
		c.collab[id] = true
		c.tick(en)
		if c.pause != 1 {
			t.Fatalf("pause %d events %v", c.pause, c.eventKinds())
		}
	})
}

// Release replies are drafted in the reporter's language (checked: version,
// each link once, no foreign links); a failing or bad draft falls back to the
// template; a crash after the send posts nothing twice (the stored draft's
// marker is found).
func TestDraftedReleaseReply(t *testing.T) {
	t.Run("drafted in the reporter's language", func(t *testing.T) {
		c := newTriageChain(t)
		c.tr.byNum[1] = Classification{Kind: KindBug, Actionable: true, Language: "ru"}
		en := c.engine(false)
		c.releaseFirst(en)
		rs := c.replies(c.item)
		if len(rs) != 1 || !strings.HasPrefix(rs[0], "Спасибо! Исправлено в версии 1.0.1") || !strings.Contains(rs[0], "issuewatcher:reply:") {
			t.Fatalf("replies %q", rs)
		}
		var req DraftRequest
		for _, r := range c.dr.reqs {
			if r.Kind == DraftReleased {
				req = r
			}
		}
		if req.Language != "ru" || req.Version != "1.0.1" || len(req.Links) != 3 || len(req.Changes) == 0 || req.Limit > maxReplyRunes {
			t.Fatalf("draft request %+v", req)
		}
	})
	for name, draft := range map[string]func(DraftRequest) string{
		"drafter fails": nil,
		"foreign link":  func(r DraftRequest) string { return ruDraft(r) + " https://evil.example/x" },
		"link twice":    func(r DraftRequest) string { return ruDraft(r) + " " + r.Links[0].URL },
		"no version":    func(DraftRequest) string { return "Спасибо, исправлено!" },
	} {
		t.Run(name, func(t *testing.T) {
			c := newTriageChain(t)
			c.dr.draft = draft
			en := c.engine(false)
			c.releaseFirst(en)
			if rs := c.replies(c.item); len(rs) != 1 || !strings.HasPrefix(rs[0], "Fixed in v1.0.1.") {
				t.Fatalf("replies %q", rs)
			}
		})
	}
	t.Run("crash after the send", func(t *testing.T) {
		c := newTriageChain(t)
		en := c.engine(false)
		id := strconv.FormatInt(c.item, 10)
		crashed := crashHook(en, StepReply+":"+id, "after")
		c.releaseFirst2(en)
		if !*crashed {
			t.Fatal("no crash")
		}
		en2 := c.engine(false)
		if err := en2.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		en2.Wait()
		if rr := c.releaseRun(); rr.State != store.RunDone {
			t.Fatalf("release %+v", rr)
		}
		if rs := c.replies(c.item); len(rs) != 1 || !strings.HasPrefix(rs[0], "Спасибо!") {
			t.Fatalf("replies %q", rs)
		}
	})
	t.Run("steam limit", func(t *testing.T) {
		if replyLimit("steam") != 999 || replyLimit("nexus") != maxReplyRunes {
			t.Fatal("reply limits")
		}
		long := strings.Repeat("я", 1000)
		if err := checkDraft(long, DraftRequest{Kind: DraftQuestion, Limit: replyLimit("steam")}, nil); err == nil {
			t.Fatal("1000 characters passed the Steam limit")
		}
	})
}

// releaseFirst2 is releaseFirst without the final state check (the release crashes).
func (c *triageChain) releaseFirst2(en *Engine) {
	c.tick(en)
	c.tick(en)
	c.later(2 * time.Minute)
	c.tick(en)
}

// A fix run's own reply (a question) after a crash right after the send: the
// restart finds it by the stored marker and posts nothing again.
func TestTriageReplyCrashSendsOnce(t *testing.T) {
	c := newTriageChain(t)
	c.tr.byNum[1] = Classification{Kind: KindQuestion, Language: "ru"}
	en := c.engine(false)
	crashed := crashHook(en, StepReply+":"+strconv.FormatInt(c.item, 10), "after")
	c.tick(en)
	if !*crashed {
		t.Fatal("no crash")
	}
	if st := c.steps(c.fixRun().ID)["reply:"+strconv.FormatInt(c.item, 10)]; st.State != store.StepSending {
		t.Fatalf("reply step %+v", st)
	}
	en2 := c.engine(false)
	c.tick(en2)
	if fr := c.fixRun(); fr.State != store.RunAnswered || len(c.replies(c.item)) != 1 || len(c.dr.reqs) != 1 {
		t.Fatalf("run %+v replies %d drafts %d", fr, len(c.replies(c.item)), len(c.dr.reqs))
	}
}
