package notify

import (
	"slices"
	"time"
)

// MaxStack is how many card windows show at once; with more cards the oldest
// fold into one "+N ещё" card, so the stack never grows past MaxStack.
const MaxStack = 3

// unhoverGrace is the minimum time a card stays after the pointer leaves it.
const unhoverGrace = 2 * time.Second

type stackEntry struct {
	id   uint64
	card Card
	left time.Duration
}

// stack is the popup state without windows: which cards show, their auto-hide
// countdown (paused while hovered), grouping and click routing. The Win32
// manager mirrors it; tests drive it directly.
type stack struct {
	autoHide time.Duration
	nextID   uint64
	cards    []stackEntry // individual cards, newest last
	group    stackEntry   // id 0 = no group card
	grouped  []Card       // cards folded into the group card
	hovered  bool
	now      func() time.Time
}

func newStack(autoHide time.Duration) *stack {
	if autoHide <= 0 {
		autoHide = DefaultAutoHide
	}
	return &stack{autoHide: autoHide, now: time.Now}
}

func (s *stack) id() uint64 { s.nextID++; return s.nextID }

// push adds a card; when the stack would pass MaxStack the oldest individual
// cards fold into the group card, whose countdown restarts.
func (s *stack) push(c Card) {
	s.cards = append(s.cards, stackEntry{id: s.id(), card: c, left: s.autoHide})
	if len(s.cards)+s.groupCount() <= MaxStack {
		return
	}
	for len(s.cards) > MaxStack-1 {
		s.grouped = append(s.grouped, s.cards[0].card)
		s.cards = s.cards[1:]
	}
	if s.group.id == 0 {
		s.group.id = s.id()
	}
	s.group.card = groupCard(s.grouped, s.now())
	s.group.left = s.autoHide
}

func (s *stack) groupCount() int {
	if s.group.id == 0 {
		return 0
	}
	return 1
}

// visible lists the shown cards anchor-first: newest card, older cards, then
// the group card furthest from the corner.
func (s *stack) visible() []stackEntry {
	out := make([]stackEntry, 0, len(s.cards)+1)
	for _, e := range slices.Backward(s.cards) {
		out = append(out, e)
	}
	if s.group.id != 0 {
		out = append(out, s.group)
	}
	return out
}

// setHovered pauses (true) or resumes the countdown of every card.
func (s *stack) setHovered(h bool) {
	if s.hovered && !h {
		for i := range s.cards {
			s.cards[i].left = max(s.cards[i].left, unhoverGrace)
		}
		if s.group.id != 0 {
			s.group.left = max(s.group.left, unhoverGrace)
		}
	}
	s.hovered = h
}

// tick advances the countdown by dt and closes expired cards; it reports
// whether anything closed.
func (s *stack) tick(dt time.Duration) bool {
	if s.hovered || dt <= 0 {
		return false
	}
	closed := false
	kept := s.cards[:0]
	for _, e := range s.cards {
		if e.left -= dt; e.left > 0 {
			kept = append(kept, e)
		} else {
			closed = true
		}
	}
	s.cards = kept
	if s.group.id != 0 {
		if s.group.left -= dt; s.group.left <= 0 {
			s.clearGroup()
			closed = true
		}
	}
	return closed
}

// close removes card id (the × button); closing the group card forgets the
// folded cards (they stay unread on the dashboard).
func (s *stack) close(id uint64) bool {
	if id != 0 && id == s.group.id {
		s.clearGroup()
		return true
	}
	for i, e := range s.cards {
		if e.id == id {
			s.cards = append(s.cards[:i], s.cards[i+1:]...)
			return true
		}
	}
	return false
}

// click closes card id and returns the dashboard route it opens.
func (s *stack) click(id uint64) (string, bool) {
	if id != 0 && id == s.group.id {
		path := s.group.card.Target()
		s.clearGroup()
		return path, true
	}
	for _, e := range s.cards {
		if e.id == id {
			s.close(id)
			return e.card.Target(), true
		}
	}
	return "", false
}

func (s *stack) clearGroup() {
	s.group = stackEntry{}
	s.grouped = nil
}
