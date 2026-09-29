package runner

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
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
	labels []provider.Label // label flow: the repository's labels
	triage *store.TriageInput // triage flow: the project's open issues
	topN   int                // triage flow: picks that get a fix job
}

// maxTriageIssuesBytes bounds {issues}.
const maxTriageIssuesBytes = 120 << 10

// issuesText lists the open issues for {issues}, one block per issue, in one
// untrusted block (titles and bodies come from the tracker).
func issuesText(t *store.TriageInput, now time.Time) string {
	if t == nil || len(t.Issues) == 0 {
		return untrusted("open issues", "(no open issues)")
	}
	var b strings.Builder
	for _, is := range t.Issues {
		age := "?"
		if c, err := time.Parse(time.RFC3339, is.CreatedAt); err == nil {
			age = strconv.Itoa(max(0, int(now.Sub(c).Hours()/24))) + "d"
		}
		labels := "-"
		if len(is.Labels) > 0 {
			labels = strings.Join(is.Labels, ", ")
		}
		body := strings.Join(strings.Fields(is.Body), " ")
		if len([]rune(body)) >= triageBodyRunes {
			body = clipRunes(body, triageBodyRunes-1) // ends with …
		}
		block := "#" + strconv.Itoa(is.Number) + " " + oneLineTitle(is.Title) + "\n  labels: " + labels + "; age: " + age +
			"; comments: " + strconv.Itoa(is.Comments) + "; author: " + is.Author + "\n  " + body + "\n"
		if b.Len()+len(block) > maxTriageIssuesBytes {
			b.WriteString("… (more issues omitted: list them with the MCP tools if available)\n")
			break
		}
		b.WriteString(block)
	}
	if t.More {
		b.WriteString("… (only the " + strconv.Itoa(len(t.Issues)) + " most recently updated open issues are listed)\n")
	}
	return untrusted("open issues", b.String())
}

// labelsText lists the repository's labels, one per line, for {labels}.
func labelsText(ls []provider.Label) string {
	if len(ls) == 0 {
		return "(no labels)"
	}
	var b strings.Builder
	for _, l := range ls {
		b.WriteString("- " + oneLineTitle(l.Name))
		if d := oneLineTitle(l.Description); d != "" {
			b.WriteString(" — " + d)
		}
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n")
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
		"{labels}", labelsText(p.labels),
		"{issues}", issuesText(p.triage, time.Now()),
		"{topN}", strconv.Itoa(p.topN),
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
	case flowLabel:
		task = render(cfg.Prompts.Label, p)
		if !strings.Contains(cfg.Prompts.Label, "{labels}") {
			task += "\n\nLabels that exist in the repository (pick only from this list):\n" + labelsText(p.labels)
		}
	case flowTriage:
		task = render(cfg.Prompts.Triage, p)
		if !strings.Contains(cfg.Prompts.Triage, "{issues}") {
			task += "\n\nOpen issues:\n" + render("{issues}", p)
		}
		if pa, ok := cfg.Projects[p.in.ProjectName]; ok && strings.TrimSpace(pa.TriagePrompt) != "" {
			task += "\n\nTriage criteria of this project from the maintainer:\n" + render(pa.TriagePrompt, p)
		}
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
