package notify

import (
	"strings"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/store"
)

func TestBalloons(t *testing.T) {
	evs := []store.Event{
		{Kind: store.EventNewIssue, ItemID: 7, Repo: "o/app", Number: 3, Title: "crash", Actor: "alice"},
		{Kind: store.EventNewComment, ItemID: 8, Repo: "o/app", Number: 4, Actor: "bob", Body: "line1\n\nline2"},
		{Kind: store.EventClosed, ItemID: 9, Repo: "o/app", Number: 5, Title: "old"},
	}
	var b []Balloon
	for _, e := range evs {
		b = append(b, Balloons([]store.Event{e})...)
	}
	if len(b) != 3 || b[0].Title != "Новый issue · o/app#3" || b[0].ItemID != "7" ||
		b[1].Text != "bob: line1 line2" || b[2].Title != "Закрыт · o/app#5" {
		t.Fatalf("balloons %+v", b)
	}
	b = Balloons(append(evs, evs[1]))
	if len(b) != 1 || b[0].ItemID != "" || b[0].Title != "4 обновления" ||
		b[0].Text != "1 новый issue, 2 новых комментария, 1 закрыт" {
		t.Fatalf("summary %+v", b)
	}
	if got := clip(strings.Repeat("я", 300), 10); len([]rune(got)) != 10 {
		t.Fatalf("clip %q", got)
	}
}

func TestRuPlural(t *testing.T) {
	for n, want := range map[int]string{0: "c", 1: "a", 2: "b", 4: "b", 5: "c", 11: "c", 12: "c", 14: "c", 21: "a", 22: "b", 25: "c", 101: "a", 111: "c"} {
		if got := ruPlural(n, "a", "b", "c"); got != want {
			t.Errorf("ruPlural(%d) = %s, want %s", n, got, want)
		}
	}
}
