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
	if len(b) != 3 || b[0].Title != "New issue · o/app#3" || b[0].ItemID != "7" ||
		b[1].Text != "bob: line1 line2" || b[2].Title != "Closed · o/app#5" {
		t.Fatalf("balloons %+v", b)
	}
	b = Balloons(append(evs, evs[1]))
	if len(b) != 1 || b[0].ItemID != "" || b[0].Title != "4 updates" ||
		b[0].Text != "1 new issues, 2 new comments, 1 closed" {
		t.Fatalf("summary %+v", b)
	}
	if got := clip(strings.Repeat("я", 300), 10); len([]rune(got)) != 10 {
		t.Fatalf("clip %q", got)
	}
}
