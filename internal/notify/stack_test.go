package notify

import (
	"strconv"
	"testing"
	"time"
)

func card(n int) Card {
	return Card{Kind: KindComment, Title: "c" + strconv.Itoa(n), ItemID: strconv.Itoa(n)}
}

func titles(v []stackEntry) []string {
	out := make([]string, len(v))
	for i, e := range v {
		out[i] = e.card.Title
	}
	return out
}

func TestStackGroupsOverflow(t *testing.T) {
	s := newStack(5 * time.Second)
	for n := 1; n <= 3; n++ {
		s.push(card(n))
	}
	if got := titles(s.visible()); len(got) != 3 || got[0] != "c3" || got[2] != "c1" {
		t.Fatalf("three cards, newest first: %v", got)
	}
	s.push(card(4)) // 4 > MaxStack: c1, c2 fold into the group
	v := s.visible()
	if got := titles(v); len(got) != MaxStack || got[0] != "c4" || got[1] != "c3" || got[2] != "+2 ещё" {
		t.Fatalf("grouped: %v", got)
	}
	groupID := v[2].id
	s.push(card(5))
	v = s.visible()
	if got := titles(v); got[0] != "c5" || got[1] != "c4" || got[2] != "+3 ещё" || v[2].id != groupID {
		t.Fatalf("group grows in place: %v (id %d → %d)", got, groupID, v[2].id)
	}
	if v[2].card.Text != "3 новых комментария" {
		t.Fatalf("group text %q", v[2].card.Text)
	}
}

func TestStackCountdownAndHover(t *testing.T) {
	s := newStack(5 * time.Second)
	s.push(card(1))
	if s.tick(3 * time.Second) {
		t.Fatal("closed too early")
	}
	s.setHovered(true)
	if s.tick(time.Minute) || len(s.visible()) != 1 {
		t.Fatal("hover must pause the countdown")
	}
	s.setHovered(false) // 2 s left, grace keeps at least unhoverGrace
	if s.tick(unhoverGrace - time.Millisecond) {
		t.Fatal("grace after hover")
	}
	if !s.tick(time.Millisecond) || len(s.visible()) != 0 {
		t.Fatal("card should expire")
	}
}

func TestStackCloseAndClickRouting(t *testing.T) {
	s := newStack(time.Minute)
	for n := 1; n <= 4; n++ {
		s.push(card(n))
	}
	v := s.visible() // c4, c3, +2
	if path, ok := s.click(v[0].id); !ok || path != "/item/4" {
		t.Fatalf("card click → %q %v", path, ok)
	}
	if path, ok := s.click(v[2].id); !ok || path != UnreadPath {
		t.Fatalf("group click → %q %v", path, ok)
	}
	if got := titles(s.visible()); len(got) != 1 || got[0] != "c3" {
		t.Fatalf("after clicks: %v", got)
	}
	if _, ok := s.click(999); ok {
		t.Fatal("unknown id routed")
	}
	if !s.close(v[1].id) || len(s.visible()) != 0 || s.close(v[1].id) {
		t.Fatal("close")
	}
	s.push(card(9)) // group cleared: a fresh card shows alone
	if got := titles(s.visible()); len(got) != 1 {
		t.Fatalf("after reset: %v", got)
	}
}
