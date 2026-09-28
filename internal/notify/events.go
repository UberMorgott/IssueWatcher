package notify

import (
	"strconv"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/store"
)

// MaxBalloons is how many events get their own balloon per sync cycle; more
// collapse into one summary that opens the dashboard. One, because the tray
// keeps a single click target (the latest balloon) and Windows queues bursts.
const MaxBalloons = 1

// Balloon is one tray notification; ItemID "" opens the dashboard.
type Balloon struct {
	Title  string
	Text   string
	ItemID string
}

// Balloons turns sync events into tray notifications (UI text is Russian).
func Balloons(events []store.Event) []Balloon {
	if len(events) > MaxBalloons {
		n := len(events)
		return []Balloon{{
			Title: strconv.Itoa(n) + " " + ruPlural(n, "обновление", "обновления", "обновлений"),
			Text:  summary(events),
		}}
	}
	out := make([]Balloon, 0, len(events))
	for _, e := range events {
		ref := e.Repo + "#" + strconv.Itoa(e.Number)
		b := Balloon{ItemID: strconv.FormatInt(e.ItemID, 10)}
		switch e.Kind {
		case store.EventNewIssue:
			b.Title, b.Text = "Новый issue · "+ref, clip(e.Title, 120)+" — "+e.Actor
		case store.EventNewComment:
			b.Title, b.Text = "Новый комментарий · "+ref, e.Actor+": "+clip(oneLine(e.Body), 160)
		case store.EventClosed:
			b.Title, b.Text = "Закрыт · "+ref, clip(e.Title, 160)
		default:
			b.Title, b.Text = string(e.Kind)+" · "+ref, clip(e.Title, 160)
		}
		out = append(out, b)
	}
	return out
}

func summary(events []store.Event) string {
	var issues, comments, closed int
	for _, e := range events {
		switch e.Kind {
		case store.EventNewIssue:
			issues++
		case store.EventNewComment:
			comments++
		case store.EventClosed:
			closed++
		}
	}
	var parts []string
	for _, p := range []struct {
		n     int
		forms [3]string
	}{
		{issues, [3]string{"новый issue", "новых issue", "новых issue"}},
		{comments, [3]string{"новый комментарий", "новых комментария", "новых комментариев"}},
		{closed, [3]string{"закрыт", "закрыто", "закрыто"}},
	} {
		if p.n > 0 {
			parts = append(parts, strconv.Itoa(p.n)+" "+ruPlural(p.n, p.forms[0], p.forms[1], p.forms[2]))
		}
	}
	return strings.Join(parts, ", ")
}

// ruPlural picks the Russian form for n: one (1, 21), few (2–4, 22–24), many (0, 5–20, 25…).
func ruPlural(n int, one, few, many string) string {
	if n < 0 {
		n = -n
	}
	switch d, dd := n%10, n%100; {
	case d == 1 && dd != 11:
		return one
	case d >= 2 && d <= 4 && (dd < 12 || dd > 14):
		return few
	default:
		return many
	}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip shortens s to n runes (balloon text is capped at 255 UTF-16 units).
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
