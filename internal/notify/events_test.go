package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/store"
)

func TestJobCard(t *testing.T) {
	now := time.Now()
	fail := JobCard(1, "o/app", 1, false, "ошибка", FailReason("no_folder", "no local folder is mapped to o/app; m…"), now)
	if fail.Title != "Агент: ошибка" || fail.Ref != "o/app#1" || fail.Text != "Папка проекта не привязана" || fail.Kind != KindIssue {
		t.Fatalf("failed: %+v", fail)
	}
	if c := JobCard(1, "o/app", 1, false, "", FailReason("weird", "boom"), now); c.Text != "boom" {
		t.Fatalf("unknown code: %+v", c)
	}
	done := JobCard(1, "o/app", 1, true, "исправлено локально", "", now)
	if done.Title != "Агент закончил" || done.Ref != "o/app#1 — исправлено локально" || done.Kind != KindClosed {
		t.Fatalf("done: %+v", done)
	}
}

func TestCards(t *testing.T) {
	now := time.Date(2026, 9, 28, 14, 32, 0, 0, time.UTC)
	evs := []store.Event{
		{Kind: store.EventNewIssue, ItemID: 7, Repo: "o/app", Number: 3, Title: "crash", Actor: "alice"},
		{Kind: store.EventNewComment, ItemID: 8, Repo: "o/app", Number: 4, Actor: "bob", Body: "line1\n\nline2"},
		{Kind: store.EventClosed, ItemID: 9, Repo: "o/app", Number: 5, Title: "old"},
	}
	c := Cards(evs, now)
	if len(c) != 3 ||
		c[0] != (Card{Kind: KindIssue, Title: "Новый issue", Ref: "o/app#3", Text: "alice: crash", Time: now, ItemID: "7"}) ||
		c[1].Kind != KindComment || c[1].Text != "bob: line1 line2" ||
		c[2].Kind != KindClosed || c[2].Title != "Issue закрыт" || c[2].Ref != "o/app#5" {
		t.Fatalf("cards %+v", c)
	}
	g := groupCard(append(c, c[1]), now)
	if g.Kind != KindGroup || g.Title != "+4 ещё" || g.Text != "1 новый issue, 2 новых комментария, 1 закрыт" {
		t.Fatalf("group %+v", g)
	}
	if got := clip(strings.Repeat("я", 300), 10); len([]rune(got)) != 10 {
		t.Fatalf("clip %q", got)
	}
}

func TestTarget(t *testing.T) {
	for _, tc := range []struct {
		c    Card
		want string
	}{
		{Card{Kind: KindComment, ItemID: "42"}, "/item/42"},
		{Card{Kind: KindIssue, ItemID: "a/b"}, "/item/a%2Fb"},
		{Card{Kind: KindIssue}, UnreadPath},
		{Card{Kind: KindGroup, ItemID: "5"}, UnreadPath},
	} {
		if got := tc.c.Target(); got != tc.want {
			t.Errorf("%+v → %q, want %q", tc.c, got, tc.want)
		}
	}
}

func TestSampleCard(t *testing.T) {
	kinds := map[Kind]bool{}
	for n := range int64(3) {
		c := SampleCard(n, time.Now())
		kinds[c.Kind] = true
		if c.ItemID == "" || !strings.Contains(c.Ref, "#"+c.ItemID) || c.Title == "" {
			t.Fatalf("sample %d: %+v", n, c)
		}
	}
	if len(kinds) != 3 {
		t.Fatalf("kinds %v", kinds)
	}
}

func TestRuPlural(t *testing.T) {
	for n, want := range map[int]string{0: "c", 1: "a", 2: "b", 4: "b", 5: "c", 11: "c", 12: "c", 14: "c", 21: "a", 22: "b", 25: "c", 101: "a", 111: "c"} {
		if got := ruPlural(n, "a", "b", "c"); got != want {
			t.Errorf("ruPlural(%d) = %s, want %s", n, got, want)
		}
	}
}

func TestPrefsFilter(t *testing.T) {
	evs := []store.Event{
		{Kind: store.EventNewIssue, Repo: "o/a"},
		{Kind: store.EventNewComment, Repo: "o/b"},
		{Kind: store.EventClosed, Repo: "o/a"},
	}
	noon := time.Date(2026, 9, 28, 12, 0, 0, 0, time.Local)
	p := DefaultPrefs()
	if got := p.Filter(evs, noon); len(got) != 3 {
		t.Fatalf("defaults kept %d", len(got))
	}
	p.Kinds = map[store.EventKind]bool{store.EventNewComment: true, store.EventClosed: true}
	p.MutedProjects = map[string]bool{"github:o/a": true}
	if got := p.Filter(evs, noon); len(got) != 1 || got[0].Repo != "o/b" {
		t.Fatalf("kinds+mute %+v", got)
	}
	p = DefaultPrefs()
	p.Enabled = false
	if got := p.Filter(evs, noon); got != nil {
		t.Fatalf("disabled %+v", got)
	}
}

func TestPrefsPick(t *testing.T) {
	evs := []store.Event{{Kind: store.EventNewIssue, ItemID: 1, Repo: "o/a", Number: 1}}
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.Local)
	busy := func() bool { return true }
	p := DefaultPrefs()
	if got := p.Pick(evs, now, busy); len(got) != 1 || got[0].Target() != "/item/1" {
		t.Fatalf("DND ignored by default: %+v", got)
	}
	p.RespectWindowsDnd = true
	if got := p.Pick(evs, now, busy); got != nil {
		t.Fatalf("busy Windows respected: %+v", got)
	}
	if got := p.Pick(evs, now, func() bool { return false }); len(got) != 1 {
		t.Fatalf("not busy: %+v", got)
	}
}

func TestQuietHours(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 9, 28, h, m, 0, 0, time.Local) }
	night := Prefs{QuietFrom: 22 * 60, QuietTo: 7 * 60} // wraps midnight
	day := Prefs{QuietFrom: 13 * 60, QuietTo: 14 * 60}
	for _, tc := range []struct {
		p    Prefs
		t    time.Time
		want bool
	}{
		{night, at(23, 0), true}, {night, at(3, 0), true}, {night, at(7, 0), false}, {night, at(21, 59), false},
		{day, at(13, 0), true}, {day, at(13, 59), true}, {day, at(14, 0), false}, {day, at(9, 0), false},
		{Prefs{}, at(3, 0), false},
	} {
		if got := tc.p.Quiet(tc.t); got != tc.want {
			t.Errorf("%+v at %s = %v, want %v", tc.p, tc.t.Format("15:04"), got, tc.want)
		}
	}
}
