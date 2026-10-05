package runner

import (
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/store"
)

// replyJob waits for the item's reply job to reach needs_review.
func (e *env) replyJob(item int64) store.Job {
	e.t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; {
		c, err := e.st.Jobs(e.t.Context(), store.JobFilter{ItemID: item, Flow: flowReply})
		if err != nil {
			e.t.Fatal(err)
		}
		if len(c.Items) > 0 && c.Items[0].State == store.JobNeedsReview {
			return c.Items[0]
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("no reply job of item %d: %+v", item, c.Items)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A fix job whose agent says the item is no bug (thanks) ends «Не баг — отзыв»
// with no commit, and a reply draft is queued for the item at once. A slop
// draft is refused and redrafted.
func TestDirectFeedbackDraftsReply(t *testing.T) {
	mode(t, "thanks")
	t.Setenv("FAKECLI_REPLY", "Thank you so much for your kind words! I'm thrilled. Happy gaming!")
	t.Setenv("FAKECLI_REPLY_REDRAFT", "Thanks, glad it works!")
	e := setup(t, 1, nil)
	j := e.wait(e.enqueue("fix", e.items[0])[0].ID, store.JobNeedsReview)
	if res := result(t, j); res.Local.Outcome != OutcomeFeedback || len(res.Local.Commits) != 0 {
		t.Fatalf("outcome: %+v", res.Local)
	}
	if k, err := e.st.ItemNonBug(t.Context(), e.items[0]); err != nil || k != OutcomeFeedback {
		t.Fatalf("item non-bug: %q %v", k, err)
	}
	rj := e.replyJob(e.items[0])
	if d := result(t, rj).Draft; d != "Thanks, glad it works!" {
		t.Fatalf("draft: %q", d)
	}
}

// A draft refused twice falls back to the minimal template of the kind.
func TestDirectFeedbackFallbackTemplate(t *testing.T) {
	mode(t, "thanks")
	t.Setenv("FAKECLI_REPLY", "Thanks! Don't hesitate to reach out if anything breaks.")
	t.Setenv("FAKECLI_REPLY_REDRAFT", "Thanks so much, it means a lot!")
	e := setup(t, 1, nil)
	e.wait(e.enqueue("fix", e.items[0])[0].ID, store.JobNeedsReview)
	if d := result(t, e.replyJob(e.items[0])).Draft; d != "Thanks, glad it works for you!" {
		t.Fatalf("draft: %q", d)
	}
}

// The fix prompt names the non-bug statuses, also for a saved prompt that predates them.
func TestFixPromptNonBugStatuses(t *testing.T) {
	if !strings.Contains(outcomeNote("old prompt"), "feedback (thanks") || outcomeNote("… suggestion …") != "" {
		t.Fatal("outcomeNote")
	}
	for _, s := range []string{OutcomeFeedback, OutcomeQuestion, OutcomeSuggestion, OutcomeNotReproduced} {
		if !agentOutcome(s) || !strings.Contains(schemas[flowFixDirect], `"`+s+`"`) || !strings.Contains(schemas[flowFixFolder], `"`+s+`"`) {
			t.Fatalf("status %s", s)
		}
	}
	if agentOutcome("fixed") {
		t.Fatal("fixed is no outcome of its own")
	}
}
