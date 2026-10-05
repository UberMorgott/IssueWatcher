package runner

import (
	"cmp"
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
	labels []provider.Label   // label flow: the repository's labels
	triage *store.TriageInput // triage flow: the project's open issues
	topN   int                // triage flow: picks that get a fix job
	// autopilot: an autopilot fix (docs/AUTOPILOT.md → Fix run): commits
	// reference the issue neutrally ("Refs #N"); the release run closes it.
	autopilot bool
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
		src := ""
		if is.Source != "" {
			src = "source: " + is.Source + " mod page report (not a repository issue); "
		}
		block := "#" + strconv.Itoa(is.Number) + " " + oneLineTitle(is.Title) + "\n  " + src + "labels: " + labels + "; age: " + age +
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
	key := p.in.ProjectKey
	if flow == flowFix || flow == flowFixDirect || flow == flowFixFolder || flow == flowReview {
		key = cmp.Or(p.in.CodeKey, key) // code work follows the code project's notes (a mod page's linked repo)
	}
	if pa, ok := cfg.Projects[key]; ok && strings.TrimSpace(pa.Prompt) != "" {
		system += "\n\nProject notes from the maintainer:\n" + render(pa.Prompt, p)
	}
	switch flow {
	case flowReply:
		task = render(cfg.Prompts.Reply, p)
	case flowFixDirect:
		tmpl := modTemplate(cfg.Prompts.FixDir, p.in)
		if p.autopilot {
			tmpl = autopilotTemplate(tmpl, p.in)
		}
		task = render(tmpl, p) + modNote(p.in) + autopilotNote(p)
	case flowFixFolder:
		task = render(folderFixPrompt, p) + modNote(p.in)
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
		if pa, ok := cfg.Projects[p.in.ProjectKey]; ok && strings.TrimSpace(pa.TriagePrompt) != "" {
			task += "\n\nTriage criteria of this project from the maintainer:\n" + render(pa.TriagePrompt, p)
		}
	case flowReview:
		task = render(cfg.Prompts.Review, p)
		if !strings.Contains(cfg.Prompts.Review, "{diff}") {
			task += "\n\nThe change:\n" + render("{diff}", p)
		}
	default:
		task = render(modTemplate(cfg.Prompts.Fix, p.in), p) + modNote(p.in)
	}
	return system, task
}

// closingRefRe finds a closing reference to the item in a fix template
// ("Fixes #{issue.number}", optionally quoted).
var closingRefRe = regexp.MustCompile(`(?i)"?\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?):?\s+#\{issue\.number\}"?`)

// modTemplate: a mod-page item's number is the dashboard's own ordinal, so a
// "Fixes #N" commit would close an unrelated issue of the code repository;
// the fix references the report URL instead.
func modTemplate(tmpl string, in store.JobInput) string {
	if !in.Mod {
		return tmpl
	}
	return closingRefRe.ReplaceAllString(tmpl, `"Reported on {issue.url}"`)
}

// autopilotTemplate: an autopilot fix must not close its issue on push (the
// release run closes it after the reply), so the closing reference becomes a
// neutral "Refs #N" (a mod-page report keeps its report URL).
func autopilotTemplate(tmpl string, in store.JobInput) string {
	if in.Mod {
		return tmpl
	}
	return closingRefRe.ReplaceAllString(tmpl, `"Refs #{issue.number}"`)
}

// autopilotNote spells the reference rule out for autopilot fixes.
func autopilotNote(p promptInput) string {
	if !p.autopilot || p.in.Mod {
		return ""
	}
	n := strconv.Itoa(p.in.Number)
	return "\n\nThis fix runs unattended (IssueWatcher autopilot). Reference the issue neutrally with \"Refs #" + n +
		"\" in the commit message. Never use a closing keyword (Fixes, Closes, Resolves and their forms) for #" + n +
		": the issue is closed by the release, after the reporter is answered."
}

// modNote tells the agent where a mod-page report comes from and forbids #N references.
func modNote(in store.JobInput) string {
	if !in.Mod {
		return ""
	}
	return "\n\nThis report comes from the mod page " + in.ProjectName + " on " + in.Platform + ", not from the issue tracker of the code. " +
		"The code is the linked project " + in.CodeProject + " (the current folder). Never write \"Fixes #N\", \"Closes #N\" or any other #number " +
		"reference in commit messages: it would close an unrelated issue of " + in.CodeProject + "; reference the report URL " + in.URL + " instead."
}
