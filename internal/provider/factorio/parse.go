package factorio

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/UberMorgott/issuewatcher/internal/provider/modkit"
)

// modRef is a mod on a user page.
type modRef struct {
	Name  string // URL name, the project id
	Title string
}

// parseUserMods lists the mods of a /user/<name> page (and its last page number).
func parseUserMods(body string) ([]modRef, int, error) {
	doc, err := modkit.ParseHTML(body)
	if err != nil {
		return nil, 0, err
	}
	var out []modRef
	seen := map[string]bool{}
	for _, list := range modkit.All(doc, modkit.Class("", "mod-list")) {
		for _, h2 := range modkit.All(list, modkit.Tag("h2")) {
			a := modkit.First(h2, modkit.Tag("a"))
			name, ok := strings.CutPrefix(modkit.AttrOr(a, "href"), "/mod/")
			if !ok || name == "" || strings.Contains(name, "/") {
				continue
			}
			name, _ = url.PathUnescape(name)
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, modRef{Name: name, Title: modkit.Txt(a)})
		}
	}
	return out, lastPage(doc, regexp.MustCompile(`[?&]page=(\d+)`)), nil
}

// row is one thread of a discussion list page.
type row struct {
	ID       string // 24 hex
	Title    string
	Category string
	Author   string
	Posted   string // raw ISO µs time (UTC, no zone)
	Replies  int
	LastBy   string
	Last     string // raw ISO µs time of the last message
}

var threadHrefRe = regexp.MustCompile(`/discussion/([0-9a-f]{24})$`)

// parseList parses a /mod/<name>/discussion[/page/N] page.
func parseList(body string) ([]row, int, error) {
	doc, err := modkit.ParseHTML(body)
	if err != nil {
		return nil, 0, err
	}
	var out []row
	for _, tr := range modkit.All(doc, modkit.Class("tr", "discussion-list-message")) {
		tds := modkit.Children(tr, modkit.Tag("td"))
		if len(tds) < 3 {
			continue
		}
		var r row
		for _, a := range modkit.All(tds[0], modkit.Tag("a")) {
			if m := threadHrefRe.FindStringSubmatch(modkit.AttrOr(a, "href")); m != nil {
				r.ID, r.Title = m[1], modkit.Txt(a)
				break
			}
		}
		if r.ID == "" {
			continue
		}
		r.Author = userOf(tds[0])
		r.Posted = titleTime(tds[0])
		r.Category = modkit.Txt(tds[1])
		stats := modkit.Children(tds[2], modkit.Class("div", "text-right"))
		if len(stats) > 0 {
			r.Replies, _ = strconv.Atoi(modkit.Txt(stats[0]))
		}
		if len(stats) > 1 {
			r.LastBy, r.Last = userOf(stats[1]), titleTime(stats[1])
		}
		if r.Last == "" {
			r.LastBy, r.Last = r.Author, r.Posted
		}
		out = append(out, r)
	}
	return out, lastPage(doc, regexp.MustCompile(`/discussion/page/(\d+)$`)), nil
}

// userOf is the first /user/<name> link's text under n.
func userOf(n *html.Node) string {
	for _, a := range modkit.All(n, modkit.Tag("a")) {
		if strings.HasPrefix(modkit.AttrOr(a, "href"), "/user/") {
			return modkit.Txt(a)
		}
	}
	return ""
}

var isoRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?$`)

// titleTime is the first ISO time in a title attribute under n (the page shows
// "2 days ago" and keeps the exact UTC time in the title).
func titleTime(n *html.Node) string {
	for _, x := range modkit.All(n, modkit.HasAttr("", "title")) {
		if t := strings.TrimSpace(modkit.AttrOr(x, "title")); isoRe.MatchString(t) {
			return t
		}
	}
	return ""
}

func lastPage(doc *html.Node, re *regexp.Regexp) int {
	most := 1
	for _, a := range modkit.All(doc, modkit.Tag("a")) {
		if m := re.FindStringSubmatch(modkit.AttrOr(a, "href")); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > most {
				most = n
			}
		}
	}
	return most
}

// message is one post of a thread (the site gives no message ids).
type message struct {
	Author string
	At     string // raw ISO µs time
	Body   string
	Owner  bool // the mod author's star
}

// replyForm is the thread's reply form as the signed-in page renders it.
type replyForm struct {
	Action   string
	Method   string
	Enctype  string
	Fields   [][2]string // hidden and preset fields, in order
	TextName string      // the textarea the message goes into
	Disabled bool        // "You need to own Factorio…" / signed out
}

// parseThread parses /mod/<name>/discussion/<id>.
func parseThread(body string) ([]message, replyForm, error) {
	doc, err := modkit.ParseHTML(body)
	if err != nil {
		return nil, replyForm{}, err
	}
	var out []message
	for _, m := range modkit.All(doc, modkit.Class("div", "discussion-message")) {
		head := modkit.First(m, modkit.Class("", "discussion-message-header"))
		msgBody := modkit.First(m, modkit.Class("", "discussion-message-body"))
		if head == nil || msgBody == nil {
			continue
		}
		author := modkit.First(head, modkit.Class("", "discussion-message-author"))
		out = append(out, message{
			Author: userOf(author), At: titleTime(head), Body: modkit.BlockText(msgBody),
			Owner: modkit.HasClass(head, "discussion-message-header-owner") || modkit.First(author, modkit.Class("i", "fa-star")) != nil,
		})
	}
	var f replyForm
	if form := modkit.First(doc, modkit.Class("form", "discussion-message-editor")); form != nil {
		f.Action, f.Method, f.Enctype = modkit.AttrOr(form, "action"), strings.ToLower(modkit.AttrOr(form, "method")), modkit.AttrOr(form, "enctype")
		for _, in := range modkit.All(form, func(n *html.Node) bool { return modkit.Is(n, "input") || modkit.Is(n, "textarea") }) {
			name := modkit.AttrOr(in, "name")
			if name == "" {
				continue
			}
			if _, dis := modkit.Attr(in, "disabled"); dis {
				continue
			}
			if in.Data == "textarea" {
				if f.TextName == "" {
					f.TextName = name
				}
				continue
			}
			switch strings.ToLower(modkit.AttrOr(in, "type")) {
			case "submit", "button", "file", "image", "reset":
				continue
			case "checkbox", "radio":
				if _, on := modkit.Attr(in, "checked"); !on {
					continue
				}
			}
			f.Fields = append(f.Fields, [2]string{name, modkit.AttrOr(in, "value")})
		}
		for _, b := range modkit.All(form, modkit.Tag("button")) {
			if _, dis := modkit.Attr(b, "disabled"); dis {
				f.Disabled = true
			}
		}
		if f.TextName == "" {
			f.Disabled = true
		}
	}
	return out, f, nil
}

// parseTime reads the site's UTC "2006-01-02T15:04:05.999999".
func parseTime(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02T15:04:05.999999999", s, time.UTC)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

var userLinkRe = regexp.MustCompile(`href="/user/([^"/?#]+)"`)

// signedInUser finds the signed-in username on a mods.factorio.com page
// ("" = signed out): the header carries a logout link and the own profile.
func signedInUser(body string) string {
	head := body
	if i := strings.Index(body, `class="container"`); i > 0 {
		head = body[:i]
	}
	if !strings.Contains(head, `href="/logout`) {
		return ""
	}
	if m := userLinkRe.FindStringSubmatch(head); m != nil {
		name, _ := url.PathUnescape(m[1])
		return name
	}
	return ""
}
