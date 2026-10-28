// Package outbound addresses a public target (an issue, a Workshop item, a mod
// page, a discussion thread, a comment) directly by its URL, without a prior
// item sync: Resolve maps the URL to a platform and the provider's ids, and
// Service posts, replies or reads through the existing providers and gates
// (docs/AGENT-CLI.md).
package outbound

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Verbs.
const (
	VerbPost     = "post"     // a new top-level comment on the target
	VerbReply    = "reply"    // a reply inside the target thread / to a comment
	VerbRead     = "read"     // the target thread(s) with their comments
	VerbDownload = "download" // read, exported to a file by the caller
)

// Target kinds.
const (
	KindIssue        = "issue"         // GitHub issue
	KindWorkshopItem = "workshop_item" // Steam Workshop item (comment thread)
	KindMod          = "mod"           // Nexus / CurseForge / Factorio mod page
	KindThread       = "thread"        // a comment thread or bug report on a mod page
)

// Error codes (stable; docs/AGENT-CLI.md → Error codes).
const (
	CodeBadURL         = "bad_url"
	CodeUnsupported    = "unsupported" // platform, target or verb not supported (yet)
	CodeNeedsParent    = "needs_parent"
	CodeEmptyBody      = "empty_body"
	CodeReplyOff       = "reply_off"
	CodeNoProvider     = "no_provider"
	CodeNotSignedIn    = "not_signed_in"
	CodeRelogin        = "relogin"
	CodeTooLong        = "too_long"
	CodeRateLimited    = "rate_limited"
	CodeOutcomeUnknown = "outcome_unknown"
	CodeNotFound       = "not_found"
	CodePlatformError  = "platform_error"
)

// Error is a refusal or failure with a stable code.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func errf(code, format string, a ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// Target is a resolved URL: the platform and the provider's ids.
type Target struct {
	Platform string `json:"platform"`
	Kind     string `json:"kind"`
	// URL is the canonical page of the target.
	URL string `json:"url"`
	// Project is the provider's project id: owner/repo, publishedfileid,
	// game/modid (Nexus), mod name (Factorio), project id (CurseForge).
	Project string `json:"project"`
	// Number is the GitHub issue number.
	Number int `json:"number,omitempty"`
	// Thread is the provider item id a reply goes to (comment:…, bug:…,
	// thread:…); "" = the page itself.
	Thread string `json:"thread,omitempty"`
	// Comment is the comment the URL points at, when it names one.
	Comment string `json:"comment,omitempty"`
}

var (
	digitsRe     = regexp.MustCompile(`^[0-9]+$`)
	ghNameRe     = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	ghRepoRe     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	ghCommentRe  = regexp.MustCompile(`^issuecomment-([0-9]+)$`)
	steamCommRe  = regexp.MustCompile(`^comment_([0-9]+)$`)
	nexusGameRe  = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
	factorioRe   = regexp.MustCompile(`^[A-Za-z0-9 ._+-]{1,100}$`)
	factorioThRe = regexp.MustCompile(`^[0-9a-f]{24}$`)
)

// Resolve maps a target URL (and, for reply, the parent comment: an id or its
// URL) to a Target. An unknown host or path is CodeBadURL / CodeUnsupported.
func Resolve(raw, parent string) (Target, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return Target{}, errf(CodeBadURL, "want an http(s) URL, got %q", raw)
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	parts := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	parent = strings.TrimSpace(parent)
	switch host {
	case "github.com":
		return resolveGitHub(parts, u.Fragment)
	case "steamcommunity.com":
		return resolveSteam(u, parts, parent)
	case "nexusmods.com":
		return resolveNexus(u, parts, parent)
	case "mods.factorio.com":
		return resolveFactorio(parts)
	case "curseforge.com", "legacy.curseforge.com", "authors.curseforge.com":
		return resolveCurseForge(parts, parent)
	}
	return Target{}, errf(CodeUnsupported, "no platform serves %s (github.com, steamcommunity.com, nexusmods.com, mods.factorio.com, curseforge.com)", host)
}

func resolveGitHub(parts []string, fragment string) (Target, error) {
	if len(parts) < 4 || !ghNameRe.MatchString(parts[0]) || !ghRepoRe.MatchString(parts[1]) || !digitsRe.MatchString(parts[3]) {
		return Target{}, errf(CodeBadURL, "want https://github.com/<owner>/<repo>/issues/<number>")
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return Target{}, errf(CodeBadURL, "bad issue number %q", parts[3])
	}
	switch parts[2] {
	case "issues":
	case "pull":
		return Target{}, errf(CodeUnsupported, "github: pull requests are not supported yet (issues only)")
	default:
		return Target{}, errf(CodeBadURL, "want https://github.com/<owner>/<repo>/issues/<number>")
	}
	repo := parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
	t := Target{Platform: "github", Kind: KindIssue, Project: repo, Number: n,
		URL: fmt.Sprintf("https://github.com/%s/issues/%d", repo, n)}
	if m := ghCommentRe.FindStringSubmatch(fragment); m != nil {
		t.Comment = m[1]
	}
	return t, nil
}

func resolveSteam(u *url.URL, parts []string, parent string) (Target, error) {
	if len(parts) != 2 || (parts[0] != "sharedfiles" && parts[0] != "workshop") || parts[1] != "filedetails" {
		return Target{}, errf(CodeUnsupported, "steam: only Workshop items (https://steamcommunity.com/sharedfiles/filedetails/?id=<id>)")
	}
	id := u.Query().Get("id")
	if !digitsRe.MatchString(id) {
		return Target{}, errf(CodeBadURL, "steam: want ?id=<publishedfileid>")
	}
	t := Target{Platform: "steam", Kind: KindWorkshopItem, Project: id,
		URL: "https://steamcommunity.com/sharedfiles/filedetails/?id=" + id}
	if m := steamCommRe.FindStringSubmatch(u.Fragment); m != nil {
		t.Comment = m[1]
	}
	if parent != "" {
		c := parent
		if pu, err := url.Parse(parent); err == nil && pu.Fragment != "" {
			c = pu.Fragment
		}
		c = strings.TrimPrefix(c, "comment_")
		if !digitsRe.MatchString(c) {
			return Target{}, errf(CodeBadURL, "steam: --parent wants a comment id or a #comment_<id> URL")
		}
		t.Comment = c
	}
	if t.Comment != "" {
		t.Thread = "comment:" + id + "/" + t.Comment
	}
	return t, nil
}

func resolveNexus(u *url.URL, parts []string, parent string) (Target, error) {
	if len(parts) < 3 || parts[1] != "mods" || !nexusGameRe.MatchString(parts[0]) || !digitsRe.MatchString(parts[2]) {
		return Target{}, errf(CodeBadURL, "nexus: want https://www.nexusmods.com/<game>/mods/<id>")
	}
	game, mod := parts[0], strings.TrimLeft(parts[2], "0")
	if mod == "" {
		return Target{}, errf(CodeBadURL, "nexus: bad mod id")
	}
	t := Target{Platform: "nexus", Kind: KindMod, Project: game + "/" + mod,
		URL: "https://www.nexusmods.com/" + game + "/mods/" + mod}
	q := u.Query()
	c := q.Get("jump_to_comment")
	if parent != "" {
		c = parent
		if pu, err := url.Parse(parent); err == nil && pu.Host != "" {
			c = pu.Query().Get("jump_to_comment")
		}
	}
	switch {
	case c == "":
	case strings.HasPrefix(c, "bug:") && digitsRe.MatchString(c[4:]):
		t.Kind, t.Thread, t.Comment = KindThread, c, c[4:]
	case digitsRe.MatchString(c):
		t.Kind, t.Thread, t.Comment = KindThread, "comment:"+t.Project+"/"+c, c
	default:
		return Target{}, errf(CodeBadURL, "nexus: --parent wants a comment id, bug:<id> or a ?jump_to_comment=<id> URL")
	}
	return t, nil
}

func resolveFactorio(parts []string) (Target, error) {
	if len(parts) < 2 || parts[0] != "mod" || !factorioRe.MatchString(parts[1]) {
		return Target{}, errf(CodeBadURL, "factorio: want https://mods.factorio.com/mod/<name>[/discussion/<thread>]")
	}
	mod := parts[1]
	t := Target{Platform: "factorio", Kind: KindMod, Project: mod, URL: "https://mods.factorio.com/mod/" + url.PathEscape(mod)}
	if len(parts) >= 3 && parts[2] == "discussion" {
		if len(parts) == 4 && factorioThRe.MatchString(parts[3]) {
			t.Kind, t.Thread = KindThread, "thread:"+parts[3]
			t.URL += "/discussion/" + parts[3]
			return t, nil
		}
		if len(parts) != 3 {
			return Target{}, errf(CodeBadURL, "factorio: bad discussion thread id")
		}
	}
	return t, nil
}

func resolveCurseForge(parts []string, parent string) (Target, error) {
	if len(parts) < 2 || parts[0] != "projects" || !digitsRe.MatchString(parts[1]) {
		return Target{}, errf(CodeUnsupported, "curseforge: slug URLs are not resolved yet; use https://www.curseforge.com/projects/<project id>")
	}
	id := strings.TrimLeft(parts[1], "0")
	if id == "" {
		return Target{}, errf(CodeBadURL, "curseforge: bad project id")
	}
	t := Target{Platform: "curseforge", Kind: KindMod, Project: id, URL: "https://www.curseforge.com/projects/" + id}
	if parent != "" {
		if !digitsRe.MatchString(parent) {
			return Target{}, errf(CodeBadURL, "curseforge: --parent wants the root comment id")
		}
		t.Kind, t.Thread, t.Comment = KindThread, "comment:"+id+"/"+parent, parent
	}
	return t, nil
}
