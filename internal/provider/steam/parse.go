package steam

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// accountIDBase turns a 32-bit account id (data-miniprofile) into a SteamID64.
const accountIDBase = 76561197960265728

// comment is one Workshop comment parsed from the render endpoint's HTML.
type comment struct {
	ID        string // numeric comment id (comment_<id>)
	Author    string // persona name
	SteamID   string // author's SteamID64 ("" when the profile link has no miniprofile)
	Body      string // plain text: <br> → newline, links as text (url), emoticons as :name:
	CreatedAt time.Time
}

// workshopFile is one of the owner's items from the myworkshopfiles page.
type workshopFile struct {
	ID    string
	AppID int
	Title string
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, class string) bool {
	return n.Type == html.ElementNode && strings.Contains(" "+attr(n, "class")+" ", " "+class+" ")
}

// find returns the first descendant of n (n included) matching f.
func find(n *html.Node, f func(*html.Node) bool) *html.Node {
	if f(n) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if m := find(c, f); m != nil {
			return m
		}
	}
	return nil
}

// each calls f on every descendant matching match, not descending into matches.
func each(n *html.Node, match func(*html.Node) bool, f func(*html.Node)) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if match(c) {
			f(c)
			continue
		}
		each(c, match, f)
	}
}

func parseFragment(s string) (*html.Node, error) {
	ctx := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(strings.NewReader(s), ctx)
	if err != nil {
		return nil, err
	}
	root := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	for _, n := range nodes {
		root.AppendChild(n)
	}
	return root, nil
}

// parseComments reads comments_html of the render endpoint, newest first as
// Steam sends them. A comment block without an id or timestamp is an error:
// the markup changed and a silent partial list would lose comments.
func parseComments(s string) ([]comment, error) {
	root, err := parseFragment(s)
	if err != nil {
		return nil, fmt.Errorf("steam: parse comments: %w", err)
	}
	var (
		out  []comment
		perr error
	)
	each(root, func(n *html.Node) bool { return hasClass(n, "commentthread_comment") }, func(n *html.Node) {
		if perr != nil {
			return
		}
		c, err := parseComment(n)
		if err != nil {
			perr = err
			return
		}
		out = append(out, c)
	})
	return out, perr
}

func parseComment(n *html.Node) (comment, error) {
	var c comment
	id, ok := strings.CutPrefix(attr(n, "id"), "comment_")
	if !ok || !isDigits(id) {
		return c, fmt.Errorf("steam: comment without id (%q)", attr(n, "id"))
	}
	c.ID = id
	if a := find(n, func(n *html.Node) bool { return hasClass(n, "commentthread_author_link") }); a != nil {
		c.Author = strings.TrimSpace(textOf(a))
		if mp, err := strconv.ParseUint(attr(a, "data-miniprofile"), 10, 32); err == nil && mp > 0 {
			c.SteamID = strconv.FormatUint(accountIDBase+mp, 10)
		}
	}
	ts := find(n, func(n *html.Node) bool { return n.Type == html.ElementNode && attr(n, "data-timestamp") != "" })
	if ts == nil {
		return c, fmt.Errorf("steam: comment %s without timestamp", id)
	}
	sec, err := strconv.ParseInt(attr(ts, "data-timestamp"), 10, 64)
	if err != nil || sec <= 0 {
		return c, fmt.Errorf("steam: comment %s: bad timestamp %q", id, attr(ts, "data-timestamp"))
	}
	c.CreatedAt = time.Unix(sec, 0).UTC()
	if t := find(n, func(n *html.Node) bool { return hasClass(n, "commentthread_comment_text") }); t != nil {
		c.Body = bodyText(t)
	}
	return c, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// textOf is the concatenated text of n.
func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

var spaceRun = regexp.MustCompile(`[ \t\r\n]+`)

// bodyText renders a comment's markup as plain text. Whitespace in the markup
// is layout (collapsed); line breaks come from <br> and block quotes.
func bodyText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			b.WriteString(spaceRun.ReplaceAllString(n.Data, " "))
			return
		case n.Type != html.ElementNode:
		case n.DataAtom == atom.Br:
			b.WriteString("\n")
			return
		case n.DataAtom == atom.Img:
			if alt := attr(n, "alt"); alt != "" {
				b.WriteString(alt)
			}
			return
		case n.DataAtom == atom.A:
			text := strings.TrimSpace(textOf(n))
			href := attr(n, "href")
			if u, ok := strings.CutPrefix(href, "https://steamcommunity.com/linkfilter/?u="); ok {
				href = u
			}
			b.WriteString(text)
			if href != "" && href != text && !strings.HasPrefix(href, "javascript:") {
				b.WriteString(" (" + href + ")")
			}
			return
		case n.DataAtom == atom.Blockquote:
			b.WriteString("\n> ")
			defer b.WriteString("\n")
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	lines := strings.Split(b.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

var showingRe = regexp.MustCompile(`Showing\s+([\d,]+)\s*-\s*([\d,]+)\s+of\s+([\d,]+)`)

// parseWorkshopFiles reads a myworkshopfiles page: the items on it and the
// total ("Showing 1-30 of 42 entries"; -1 when the counter is missing).
func parseWorkshopFiles(s string) ([]workshopFile, int, error) {
	root, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return nil, 0, fmt.Errorf("steam: parse workshop page: %w", err)
	}
	var out []workshopFile
	each(root, func(n *html.Node) bool { return hasClass(n, "workshopItem") }, func(n *html.Node) {
		a := find(n, func(n *html.Node) bool { return n.Type == html.ElementNode && attr(n, "data-publishedfileid") != "" })
		if a == nil || !isDigits(attr(a, "data-publishedfileid")) {
			return
		}
		f := workshopFile{ID: attr(a, "data-publishedfileid")}
		f.AppID, _ = strconv.Atoi(attr(a, "data-appid"))
		if t := find(n, func(n *html.Node) bool { return hasClass(n, "workshopItemTitle") }); t != nil {
			f.Title = strings.TrimSpace(textOf(t))
		}
		out = append(out, f)
	})
	total := -1
	if m := showingRe.FindStringSubmatch(s); m != nil {
		total, _ = strconv.Atoi(strings.ReplaceAll(m[3], ",", ""))
	} else if strings.Contains(s, "workshopBrowseItems") || len(out) > 0 {
		total = len(out)
	}
	return out, total, nil
}
