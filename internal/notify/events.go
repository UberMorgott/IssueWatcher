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

// UnreadPath is the dashboard route of a grouped card: unread issues; a group
// of comment-thread cards only opens UnreadCommentsPath, a mixed one Overview.
const (
	UnreadPath         = "/issues?unread=1"
	UnreadCommentsPath = "/comments?unread=1"
	OverviewPath       = "/"
)

// Card is one popup notification (UI text is Russian).
type Card struct {
	Kind   Kind
	Title  string    // "Новый комментарий"
	Ref    string    // "owner/repo#42"
	Text   string    // snippet, wrapped to two lines
	Time   time.Time // shown as HH:MM
	ItemID string    // "" opens the unread list
	Thread bool      // the item is a mod page comment thread (Comments page), not an issue
	Path   string    // explicit dashboard route (agent job cards); wins over ItemID
}

// Target is the dashboard route a click on the card opens.
func (c Card) Target() string {
	if c.Path != "" {
		return c.Path
	}
	if c.Kind == KindGroup || c.ItemID == "" {
		if c.Thread {
			return UnreadCommentsPath
		}
		return UnreadPath
	}
	return "/item/" + url.PathEscape(c.ItemID)
}

// Cards turns sync events into popup cards, one per event.
func Cards(events []store.Event, now time.Time) []Card {
	out := make([]Card, 0, len(events))
	for _, e := range events {
		c := Card{
			Ref:    ref(e),
			Time:   now,
			ItemID: strconv.FormatInt(e.ItemID, 10),
			Thread: e.ItemKind == store.KindComment,
		}
		switch e.Kind {
		case store.EventNewIssue:
			c.Kind, c.Title, c.Text = KindIssue, "Новый issue", clip(e.Title, 200)
			if e.Actor != "" {
				c.Text = e.Actor + ": " + c.Text
			}
		case store.EventNewItem: // a mod page's new comment thread or bug report
			c.Kind, c.Title, c.Text = KindComment, "Новый комментарий", clip(e.Title, 200)
			if e.ItemKind == store.KindBug {
				c.Kind, c.Title = KindIssue, "Новый баг-репорт"
			}
			if e.Actor != "" {
				c.Text = e.Actor + ": " + c.Text
			}
		case store.EventNewComment:
			c.Kind, c.Title, c.Text = KindComment, "Новый комментарий", e.Actor+": "+clip(oneLine(e.Body), 200)
		case store.EventClosed:
			c.Kind, c.Title, c.Text = KindClosed, "Issue закрыт", clip(e.Title, 200)
			if e.ItemKind == store.KindBug {
				c.Title = "Баг-репорт закрыт"
			}
		default:
			c.Kind, c.Title, c.Text = KindIssue, string(e.Kind), clip(e.Title, 200)
		}
		out = append(out, c)
	}
	return out
}

// ref names an item on a card: owner/repo#42 for a GitHub issue, the mod page
// name for a mod page's comment thread or bug report (their numbers are
// internal ids, meaningless to the reader).
func ref(e store.Event) string {
	if e.ItemKind == store.KindComment || e.ItemKind == store.KindBug {
		return e.Repo
	}
	return e.Repo + "#" + strconv.Itoa(e.Number)
}

// JobCard is the card of a finished agent job; a click opens the issue.
// ok = finished: «Агент закончил», repo#N — outcome (✓ icon); otherwise
// «Агент: ошибка», repo#N and the reason as text.
func JobCard(itemID int64, repo string, number int, ok bool, outcome, text string, now time.Time) Card {
	c := Card{Kind: KindClosed, Title: "Агент закончил", Ref: repo + "#" + strconv.Itoa(number), Time: now,
		Text: clip(oneLine(text), 200), ItemID: strconv.FormatInt(itemID, 10)}
	if !ok {
		c.Kind, c.Title = KindIssue, "Агент: ошибка"
		if c.Text == "" {
			c.Text = "Задача не выполнена"
		}
		return c
	}
	if outcome != "" {
		c.Ref += " — " + outcome
	}
	if c.Text == "" {
		c.Text = "Результат готов к проверке"
	}
	return c
}

// ReloginCard asks to sign in to a platform again; a click opens Подключения
// with that platform's sign-in started (QR code or sign-in window).
func ReloginCard(platform, name string, now time.Time) Card {
	return Card{Kind: KindIssue, Title: name + ": войдите снова", Ref: name,
		Text: "Сессия истекла — нажмите, чтобы войти", Time: now, Path: "/connections?login=" + url.QueryEscape(platform)}
}

// failReasons are the Russian card texts of the runner's error codes.
// agent_failed has none: «the agent failed» says nothing the title does not,
// the CLI's own error is the reason.
var failReasons = map[string]string{
	"no_folder":   "Папка проекта не привязана",
	"no_profile":  "Нет профиля агента для задачи",
	"no_cli":      "CLI агента не найден",
	"agent_auth":  "CLI агента не вошёл в аккаунт: сессия истекла — войдите заново (claude → /login)",
	"timeout":     "Агент не уложился в лимит времени",
	"git":         "Ошибка на шаге git",
	"interrupted": "Задача прервана перезапуском",
}

// FailReason is the card text of a failed job: the Russian reason of a known
// error code, else the raw error (agent_failed: what the CLI said).
func FailReason(code, err string) string {
	if r, ok := failReasons[code]; ok {
		return r
	}
	return err
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
	g := Card{Kind: KindGroup, Title: "+" + strconv.Itoa(n) + " ещё", Text: summary(cards), Time: now}
	threads := 0
	for _, c := range cards {
		if c.Thread {
			threads++
		}
	}
	switch {
	case threads == n:
		g.Thread = true
	case threads > 0:
		g.Path = OverviewPath // issues and comment threads: Overview shows both
	}
	return g
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
