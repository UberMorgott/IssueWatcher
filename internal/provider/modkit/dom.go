package modkit

import (
	"regexp"
	"slices"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// ParseHTML parses a page or widget answer as a browser's DOMParser does
// (scripting off: <noscript> content is markup).
func ParseHTML(s string) (*html.Node, error) {
	return html.ParseWithOptions(strings.NewReader(s), html.ParseOptionEnableScripting(false))
}

// Attr is an attribute value ("" = missing); ok tells missing from empty.
func Attr(n *html.Node, name string) (string, bool) {
	if n == nil {
		return "", false
	}
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == name {
			return a.Val, true
		}
	}
	return "", false
}

// AttrOr is an attribute value or "".
func AttrOr(n *html.Node, name string) string {
	v, _ := Attr(n, name)
	return v
}

// HasClass reports whether n's class list contains c.
func HasClass(n *html.Node, c string) bool {
	return n != nil && n.Type == html.ElementNode && slices.Contains(strings.Fields(AttrOr(n, "class")), c)
}

// Is reports an element with tag (a lower-case name, "" = any).
func Is(n *html.Node, tag string) bool {
	return n != nil && n.Type == html.ElementNode && (tag == "" || n.Data == tag)
}

// Pred selects elements.
type Pred func(*html.Node) bool

// Tag selects tag elements.
func Tag(tag string) Pred { return func(n *html.Node) bool { return Is(n, tag) } }

// Class selects elements (tag "" = any) with class c.
func Class(tag, c string) Pred {
	return func(n *html.Node) bool { return Is(n, tag) && HasClass(n, c) }
}

// ID selects the element with id.
func ID(id string) Pred { return func(n *html.Node) bool { return Is(n, "") && AttrOr(n, "id") == id } }

// HasAttr selects elements (tag "" = any) carrying attribute name.
func HasAttr(tag, name string) Pred {
	return func(n *html.Node) bool {
		_, ok := Attr(n, name)
		return Is(n, tag) && ok
	}
}

// And combines predicates.
func And(ps ...Pred) Pred {
	return func(n *html.Node) bool {
		for _, p := range ps {
			if !p(n) {
				return false
			}
		}
		return true
	}
}

// All returns the descendants of n (not n) matching p, in document order.
func All(n *html.Node, p Pred) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			if p(c) {
				out = append(out, c)
			}
			walk(c)
		}
	}
	if n != nil {
		walk(n)
	}
	return out
}

// First is the first descendant matching p (nil = none).
func First(n *html.Node, p Pred) *html.Node {
	if n == nil {
		return nil
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if p(c) {
			return c
		}
		if f := First(c, p); f != nil {
			return f
		}
	}
	return nil
}

// Child is the first direct child matching p.
func Child(n *html.Node, p Pred) *html.Node {
	if n == nil {
		return nil
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if p(c) {
			return c
		}
	}
	return nil
}

// Children are the direct children matching p.
func Children(n *html.Node, p Pred) []*html.Node {
	var out []*html.Node
	if n == nil {
		return nil
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if p(c) {
			out = append(out, c)
		}
	}
	return out
}

// TextContent is the DOM textContent of n (text of every descendant text node).
func TextContent(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
			return
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// jsSpace is JavaScript's \s (String.prototype.trim and /\s/).
func jsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
		return true
	}
	return r >= '\u2000' && r <= '\u200a'
}

// JSTrim is String.prototype.trim.
func JSTrim(s string) string { return strings.TrimFunc(s, jsSpace) }

// Txt is textContent with whitespace runs collapsed and trimmed (the sites'
// one-line texts: names, titles, statuses).
func Txt(n *html.Node) string {
	return JSTrim(strings.Join(strings.FieldsFunc(TextContent(n), jsSpace), " "))
}

var (
	blankRun  = regexp.MustCompile("[ \t\u00a0]+")
	spaceNL   = regexp.MustCompile(" ?\n ?")
	manyNL    = regexp.MustCompile("\n{3,}")
	blockTags = map[atom.Atom]bool{atom.P: true, atom.Div: true, atom.Li: true, atom.Blockquote: true, atom.Pre: true,
		atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.Tr: true}
)

// BlockText is a post body as text, exactly as the reference servers render
// it: <br> → line break, a break after each block element, scripts and
// styles dropped, blank runs collapsed, at most one empty line.
func BlockText(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		switch {
		case x.Type == html.TextNode:
			b.WriteString(x.Data)
			return
		case x.Type == html.ElementNode && (x.DataAtom == atom.Script || x.DataAtom == atom.Style):
			return
		case x.Type == html.ElementNode && x.DataAtom == atom.Br:
			b.WriteString("\n")
			return
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if x.Type == html.ElementNode && blockTags[x.DataAtom] {
			b.WriteString("\n")
		}
	}
	walk(n)
	s := blankRun.ReplaceAllString(b.String(), " ")
	s = spaceNL.ReplaceAllString(s, "\n")
	s = manyNL.ReplaceAllString(s, "\n\n")
	return JSTrim(s)
}

// Detach removes n from its parent.
func Detach(n *html.Node) {
	if n != nil && n.Parent != nil {
		n.Parent.RemoveChild(n)
	}
}
