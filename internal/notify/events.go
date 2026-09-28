package notify

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Kind picks a card's icon.
type Kind int

// Card kinds.
const (
	KindIssue Kind = iota
	KindComment
	KindClosed
	KindGroup // "+N ещё": cards that did not fit the stack
)

// UnreadPath is the dashboard route of a grouped card.
const UnreadPath = "/issues?unread=1"

// Card is one popup notification (UI text is Russian).
type Card struct {
	Kind   Kind
	Title  string    // "Новый комментарий"
	Ref    string    // "owner/repo#42"
	Text   string    // snippet, wrapped to two lines
	Time   time.Time // shown as HH:MM
	ItemID string    // "" opens the unread list
	Path   string    // explicit dashboard route (agent job cards); wins over ItemID
}

// Target is the dashboard route a click on the card opens.
func (c Card) Target() string {
	if c.Path != "" {
		return c.Path
	}
	if c.Kind == KindGroup || c.ItemID == "" {
		return UnreadPath
	}
	return "/item/" + url.PathEscape(c.ItemID)
}

// Cards turns sync events into popup cards, one per event.
func Cards(events []store.Event, now time.Time) []Card {
	out := make([]Card, 0, len(events))
	for _, e := range events {
		c := Card{
			Ref:    e.Repo + "#" + strconv.Itoa(e.Number),
			Time:   now,
			ItemID: strconv.FormatInt(e.ItemID, 10),
		}
		switch e.Kind {
		case store.EventNewIssue:
			c.Kind, c.Title, c.Text = KindIssue, "Новый issue", clip(e.Title, 200)
			if e.Actor != "" {
				c.Text = e.Actor + ": " + c.Text
			}
		case store.EventNewComment:
			c.Kind, c.Title, c.Text = KindComment, "Новый комментарий", e.Actor+": "+clip(oneLine(e.Body), 200)
		case store.EventClosed:
			c.Kind, c.Title, c.Text = KindClosed, "Issue закрыт", clip(e.Title, 200)
		default:
			c.Kind, c.Title, c.Text = KindIssue, string(e.Kind), clip(e.Title, 200)
		}
		out = append(out, c)
	}
	return out
}

// JobCard is the «Агент закончил: repo#N» card of a finished agent job; a click
// opens the job. ok = review ready (✓ icon), otherwise it failed.
func JobCard(jobID int64, repo string, number int, ok bool, text string, now time.Time) Card {
	c := Card{Kind: KindClosed, Title: "Агент закончил", Ref: repo + "#" + strconv.Itoa(number), Time: now,
		Text: clip(oneLine(text), 200), Path: "/jobs/" + strconv.FormatInt(jobID, 10)}
	if !ok {
		c.Kind, c.Title = KindIssue, "Агент: ошибка"
	}
	if c.Text == "" {
		c.Text = "Результат готов к проверке"
	}
	return c
}

// SampleCard is the tray «Тестовое уведомление» card: kinds rotate with n,
// the item id cycles through 1..5.
func SampleCard(n int64, now time.Time) Card {
	id := strconv.FormatInt(n%5+1, 10)
	c := Card{Ref: "UberMorgott/IssueWatcher#" + id, Time: now, ItemID: id}
	switch n % 3 {
	case 0:
		c.Kind, c.Title, c.Text = KindComment, "Новый комментарий", "octocat: Тестовое уведомление №"+strconv.FormatInt(n, 10)+
			". Нажмите на карточку, чтобы открыть issue "+id+" на дашборде."
	case 1:
		c.Kind, c.Title, c.Text = KindIssue, "Новый issue", "octocat: Тестовое уведомление №"+strconv.FormatInt(n, 10)
	default:
		c.Kind, c.Title, c.Text = KindClosed, "Issue закрыт", "Тестовое уведомление №"+strconv.FormatInt(n, 10)
	}
	return c
}

// groupCard summarises cards that did not fit the stack.
func groupCard(cards []Card, now time.Time) Card {
	n := len(cards)
	return Card{Kind: KindGroup, Title: "+" + strconv.Itoa(n) + " ещё", Text: summary(cards), Time: now}
}

func summary(cards []Card) string {
	var issues, comments, closed int
	for _, c := range cards {
		switch c.Kind {
		case KindIssue:
			issues++
		case KindComment:
			comments++
		case KindClosed:
			closed++
		case KindGroup:
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

// clip shortens s to n runes; the renderer ellipsizes to the card width.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
