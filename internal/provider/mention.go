package provider

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// WithMention returns body as a reply to author on a platform without reply
// threads (Capabilities.ReplyThreaded off): the text starts with "@author "
// exactly once. Leading whitespace is dropped; a body that already starts with
// @author (any letter case, followed by a non-name character or the end) is
// kept as it is. An empty author leaves the trimmed body unchanged.
func WithMention(body, author string) string {
	text := strings.TrimLeftFunc(body, unicode.IsSpace)
	author = strings.TrimSpace(author)
	if author == "" {
		return text
	}
	if HasMention(text, author) {
		return text
	}
	return "@" + author + " " + text
}

// HasMention: text starts with @author (any letter case) followed by a
// non-name character or the end.
func HasMention(text, author string) bool {
	m := "@" + author
	if len(text) < len(m) || !strings.EqualFold(text[:len(m)], m) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(text[len(m):])
	return r == utf8.RuneError || !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-'
}
