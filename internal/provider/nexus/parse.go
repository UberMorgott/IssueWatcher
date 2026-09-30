package nexus

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/UberMorgott/issuewatcher/internal/provider/modkit"
)

// Parsers of the www.nexusmods.com widgets. They produce exactly what the
// reference MCP server produced (its site-parsers + json-shapes), so the
// native engine stores the same items, ids, authors and bodies.

// jsNumber is JavaScript's Number(s) || 0.
func jsNumber(s string) float64 {
	s = modkit.JSTrim(s)
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

// isoUTC is the server's isoUtc(unix s|ms): nil for 0.
func isoUTC(d float64) *string {
	if d == 0 {
		return nil
	}
	ms := d
	if d < 1e12 {
		ms = d * 1000
	}
	t := time.UnixMilli(int64(ms)).UTC()
	s := t.Format("2006-01-02T15:04:05.000Z")
	return &s
}

var localStampRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})[ T](\d{2}:\d{2}(?::\d{2})?)$`)

// localStamp is the server's localStamp: "YYYY-MM-DD HH:MM" → "YYYY-MM-DDTHH:MM".
func localStamp(s string) *string {
	m := localStampRe.FindStringSubmatch(modkit.JSTrim(s))
	if m == nil {
		return nil
	}
	v := m[1] + "T" + m[2]
	return &v
}

// maxPage is the highest number among the pagination links (at least 1).
func maxPage(doc *html.Node) int {
	most := 1
	for _, ul := range modkit.All(doc, modkit.Class("", "pagination")) {
		for _, li := range modkit.All(ul, modkit.Tag("li")) {
			for _, a := range modkit.All(li, modkit.Tag("a")) {
				if n, ok := jsParseInt(modkit.TextContent(a)); ok && n > most {
					most = n
				}
			}
		}
	}
	return most
}

// jsParseInt is parseInt(s.trim(), 10): leading digits (optionally signed).
func jsParseInt(s string) (int, bool) {
	s = modkit.JSTrim(s)
	end := 0
	if end < len(s) && (s[end] == '-' || s[end] == '+') {
		end++
	}
	start := end
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == start {
		return 0, false
	}
	n, err := strconv.Atoi(s[:end])
	return n, err == nil
}

func orQ(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// commentPage is a parsed CommentContainer widget.
type commentPage struct {
	commentsResult
	csrf string
}

// parseComments parses a CommentContainer answer.
func parseComments(body string) (commentPage, error) {
	doc, err := modkit.ParseHTML(body)
	if err != nil {
		return commentPage{}, err
	}
	pick := func(li *html.Node) post {
		head := modkit.Child(li, modkit.Class("", "comment-head"))
		content := modkit.Child(li, modkit.Class("", "comment-content"))
		name := modkit.First(modkit.First(head, modkit.Class("", "comment-name")), modkit.Tag("a"))
		p := post{ID: strings.Replace(modkit.AttrOr(li, "id"), "comment-", "", 1), Author: orQ(modkit.Txt(name))}
		p.CreatedAt = isoUTC(jsNumber(modkit.AttrOr(modkit.First(content, modkit.HasAttr("time", "data-date")), "data-date")))
		p.Body = modkit.BlockText(modkit.First(content, modkit.Class("", "comment-content-text")))
		return p
	}
	var out commentPage
	for _, li := range modkit.All(doc, modkit.Class("li", "comment")) {
		if modkit.HasClass(li.Parent, "comment-kids") {
			continue
		}
		t := thread{post: pick(li), Sticky: modkit.HasClass(li, "comment-sticky")}
		content := modkit.Child(li, modkit.Class("", "comment-content"))
		for _, l := range modkit.All(content, modkit.Class("", "locked")) {
			if !strings.Contains(modkit.AttrOr(l, "style"), "none") {
				t.Locked = true
				break
			}
		}
		for _, kids := range modkit.Children(li, modkit.Class("ol", "comment-kids")) {
			for _, k := range modkit.Children(kids, modkit.Class("li", "comment")) {
				t.Replies = append(t.Replies, pick(k))
			}
		}
		if t.Replies == nil {
			t.Replies = []post{}
		}
		out.Comments = append(out.Comments, t)
	}
	out.Total = int(jsNumber(modkit.AttrOr(modkit.First(doc, modkit.ID("comment-count")), "data-comment-count")))
	out.Page = int(jsNumber(modkit.AttrOr(modkit.First(doc, modkit.ID("current-page-number")), "value")))
	if out.Page == 0 {
		out.Page = 1
	}
	out.Pages = maxPage(doc)
	out.csrf = modkit.AttrOr(modkit.First(doc, modkit.HasAttr("", "data-csrf-token")), "data-csrf-token")
	return out, nil
}

var bugClosed = map[string]bool{"fixed": true, "duplicate": true, "not_a_bug": true, "wont_fix": true}

var wontFixRe = regexp.MustCompile(`won.?t fix`)

// bugStatusKey maps a Bugs-tab status label to its key ("" = unknown).
func bugStatusKey(label string) string {
	l := strings.ToLower(label)
	switch {
	case strings.Contains(l, "new"):
		return "new"
	case strings.Contains(l, "looked"):
		return "looking"
	case strings.Contains(l, "known"):
		return "known"
	case strings.Contains(l, "duplicate"):
		return "duplicate"
	case strings.Contains(l, "not a bug"):
		return "not_a_bug"
	case wontFixRe.MatchString(l):
		return "wont_fix"
	case strings.Contains(l, "fixed"):
		return "fixed"
	case strings.Contains(l, "need"):
		return "need_info"
	}
	return ""
}

// parseBugs parses a ModBugsTab answer; enabled=false when the tab is off.
func parseBugs(body, listURL string, page int) (bugsResult, bool, error) {
	doc, err := modkit.ParseHTML(body)
	if err != nil {
		return bugsResult{}, false, err
	}
	enabled := modkit.First(doc, modkit.ID("tab-modbugs")) != nil
	r := bugsResult{Page: page, Pages: maxPage(doc), URL: listURL, Bugs: []bugRow{}}
	for _, tr := range modkit.All(doc, modkit.And(modkit.Class("tr", "mod-issue-row"), modkit.HasAttr("tr", "data-issue-id"))) {
		status := modkit.Txt(modkit.First(tr, modkit.Class("td", "table-bug-status")))
		b := bugRow{
			ID:      modkit.AttrOr(tr, "data-issue-id"),
			Title:   modkit.Txt(modkit.First(tr, modkit.Class("a", "issue-title"))),
			Status:  status,
			Replies: int(jsNumber(modkit.Txt(modkit.First(tr, modkit.Class("td", "table-bug-replies"))))),
		}
		key := bugStatusKey(status)
		if key != "" {
			b.StatusKey = &key
		}
		b.Open = key == "" || !bugClosed[key]
		post := modkit.First(tr, modkit.Class("td", "table-bug-post"))
		b.LastPostAt = isoUTC(jsNumber(modkit.AttrOr(modkit.First(post, modkit.HasAttr("time", "data-date")), "data-date")))
		r.Bugs = append(r.Bugs, b)
	}
	return r, enabled, nil
}

// bugPage is a parsed ModBugReplyList.
type bugPage struct {
	posts []bugPostRaw
	token string
}

type bugPostRaw struct {
	bugPost
	isReport bool
}

// parseBugReplies parses a ModBugReplyList answer.
func parseBugReplies(body string) (bugPage, error) {
	doc, err := modkit.ParseHTML(body)
	if err != nil {
		return bugPage{}, err
	}
	var out bugPage
	for _, li := range modkit.All(doc, func(n *html.Node) bool {
		return modkit.Is(n, "li") && modkit.HasClass(n, "comment") && strings.HasPrefix(modkit.AttrOr(n, "id"), "bug-")
	}) {
		id := modkit.AttrOr(li, "id")
		content := modkit.First(li, modkit.Class("", "comment-content"))
		date := modkit.AttrOr(modkit.First(content, modkit.HasAttr("time", "datetime")), "datetime")
		var text string
		if content != nil {
			clone := cloneNode(content)
			for _, x := range modkit.All(clone, func(n *html.Node) bool {
				return modkit.Is(n, "time") || modkit.Is(n, "script") || modkit.HasClass(n, "comment-reply")
			}) {
				modkit.Detach(x)
			}
			for _, x := range modkit.All(clone, func(n *html.Node) bool {
				return modkit.Is(n, "a") || modkit.Is(n, "button") || modkit.Is(n, "li")
			}) {
				if modkit.JSTrim(modkit.TextContent(x)) == "Edit post" {
					modkit.Detach(x)
				}
			}
			text = modkit.BlockText(clone)
		}
		p := bugPostRaw{
			isReport:       strings.HasPrefix(id, "bug-issue-tile-"),
			ID:             strings.TrimPrefix(strings.TrimPrefix(id, "bug-issue-tile-"), "bug-reply-tile-"),
			Author:         orQ(modkit.Txt(modkit.First(modkit.First(li, modkit.Class("", "comment-name")), modkit.Tag("a")))),
			CreatedAtLocal: localStamp(date),
			Body:           text,
		}
		out.posts = append(out.posts, p)
	}
	out.token = modkit.AttrOr(modkit.First(doc, modkit.And(modkit.Class("", "add-bug-reply"), modkit.HasAttr("", "data-csrf-token"))), "data-csrf-token")
	return out, nil
}

// result is the get_mod_bug shape: the report and the other posts.
func (b bugPage) result() bugResult {
	var r bugResult
	idx := 0
	for i, p := range b.posts {
		if p.isReport {
			idx = i
			break
		}
	}
	r.Replies = []bugPost{}
	for i, p := range b.posts {
		if i == idx {
			r.Report = p.bugPost
			continue
		}
		r.Replies = append(r.Replies, p.bugPost)
	}
	return r
}

func cloneNode(n *html.Node) *html.Node {
	c := &html.Node{Type: n.Type, DataAtom: n.DataAtom, Data: n.Data, Namespace: n.Namespace, Attr: append([]html.Attribute(nil), n.Attr...)}
	for k := n.FirstChild; k != nil; k = k.NextSibling {
		c.AppendChild(cloneNode(k))
	}
	return c
}

var threadIDRe = regexp.MustCompile(`CommentContainer\?[^"']*?thread_id=(\d+)`)

// parseThreadID finds the Posts thread id on a mod page (0 = none).
func parseThreadID(page string) int {
	m := threadIDRe.FindStringSubmatch(page)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}
