package runner

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Untrusted text (issue title, body, comments, a diff made from them) reaches
// the agent only inside these blocks; the system prompt says what they are.
const (
	untrustedOpen  = "<untrusted-issue-content"
	untrustedClose = "</untrusted-issue-content>"

	maxBodyBytes     = 60 << 10
	maxCommentsBytes = 60 << 10
	maxDiffBytes     = 100 << 10
)

// tagRe finds anything that could open or close an untrusted block, in any case
// and spacing, so issue text cannot end its block early.
var tagRe = regexp.MustCompile(`(?i)<\s*/?\s*untrusted-issue-content[^>]*>?`)

func defang(s string) string { return tagRe.ReplaceAllString(s, "[tag removed]") }

// untrusted wraps s as data from the issue tracker.
func untrusted(source, s string) string {
	s = strings.TrimSpace(defang(s))
	if s == "" {
		s = "(empty)"
	}
	return untrustedOpen + ` source="` + source + `">` + "\n" + s + "\n" + untrustedClose
}

// oneLineTitle keeps a title on one line without block tags.
func oneLineTitle(s string) string {
	s = strings.Join(strings.Fields(defang(s)), " ")
	s = strings.ReplaceAll(s, `"`, "'")
	return clipRunes(s, 300)
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// clipHead keeps the first n bytes (on a rune boundary).
func clipHead(s string, n int, note string) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "\n… " + note
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// promptInput is everything the templates may reference.
type promptInput struct {
	in     store.JobInput
	branch string
	diff   string
}

func commentsText(cs []store.Comment) string {
	if len(cs) == 0 {
		return "(no comments)"
	}
	// Newest comments matter most: drop the oldest first when over the limit.
	parts := make([]string, 0, len(cs))
	total := 0
	for i, c := range slices.Backward(cs) {
		p := "--- " + c.Author + " (" + c.CreatedAt + "):\n" + strings.TrimSpace(c.Body)
		if total+len(p) > maxCommentsBytes {
			parts = append(parts, "--- ("+strconv.Itoa(i+1)+" older comments omitted)")
			break
		}
		total += len(p)
		parts = append(parts, p)
	}
	slices.Reverse(parts)
	return strings.Join(parts, "\n\n")
}

// render fills a template's variables.
func render(tmpl string, p promptInput) string {
	return strings.NewReplacer(
		"{repo}", p.in.ProjectName,
		"{issue.number}", strconv.Itoa(p.in.Number),
		"{issue.title}", oneLineTitle(p.in.Title),
		"{issue.url}", p.in.URL,
		"{issue.body}", untrusted("issue body", clipHead(p.in.Body, maxBodyBytes, "(issue body truncated)")),
		"{comments}", untrusted("comments", commentsText(p.in.Comments)),
		"{localPath}", p.in.LocalPath,
		"{branch}", p.branch,
		"{diff}", untrusted("diff", clipHead(p.diff, maxDiffBytes, "(diff truncated)")),
	).Replace(tmpl)
}

// prompts builds the system text (global + project layer) and the task text of flow.
func prompts(cfg config.Agents, flow string, p promptInput) (system, task string) {
	system = render(cfg.Prompts.System, p)
	if pa, ok := cfg.Projects[p.in.ProjectName]; ok && strings.TrimSpace(pa.Prompt) != "" {
		system += "\n\nProject notes from the maintainer:\n" + render(pa.Prompt, p)
	}
	switch flow {
	case flowReply:
		task = render(cfg.Prompts.Reply, p)
	case flowFixDirect:
		task = render(cfg.Prompts.FixDir, p)
	case flowReview:
		task = render(cfg.Prompts.Review, p)
		if !strings.Contains(cfg.Prompts.Review, "{diff}") {
			task += "\n\nThe change:\n" + render("{diff}", p)
		}
	default:
		task = render(cfg.Prompts.Fix, p)
	}
	return system, task
}
