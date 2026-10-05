package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/replystyle"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Reporter replies drafted by a read-only agent (docs/AUTOPILOT.md → Replies):
// in the reporter's language, short and friendly, saying once what was fixed,
// the version and where to get it (only the platforms that have it). The app
// checks the draft (length, version, links only from the facts, each once);
// a draft that fails the check, or no drafter, falls back to the template.
// The body is stored on the step before the send, so a resend after a crash
// posts the same text and the probe looks for the same marker.

// Draft kinds (DraftRequest.Kind, replyState.Kind).
const (
	DraftReleased  = "released"   // the fix is out: version + links
	DraftQuestion  = "question"   // answer the reporter's question
	DraftNeedsInfo = "needs_info" // ask for the details a fix needs
	// Not a bug (a fix run's agent or triage said so): a short human reply.
	DraftFeedback   = replystyle.KindFeedback   // thanks / praise: "Thanks, glad it works for you!"
	DraftSuggestion = replystyle.KindSuggestion // an idea: thanks, fits or not, no promises
)

// Link is a place the release can be downloaded from.
type Link struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// DraftRequest is one reply to draft.
type DraftRequest struct {
	ItemID   int64
	Kind     string   // released | question | needs_info | feedback | suggestion
	Version  string   // released: X.Y.Z
	Changes  []string // released: what was fixed (commit subjects / changelog lines)
	Links    []Link   // released: where to download (available targets + the GitHub release)
	Notes    string   // needs_info: what is missing; question / suggestion: what the fixing agent found
	Language string   // the reporter's language from triage ("" = detect from the item)
	Limit    int      // max characters of the reply
	LogPath  string   // the agent's step log (JSONL)
}

// Drafter drafts a reporter reply with a read-only agent (runner.Runner).
type Drafter interface {
	DraftReply(ctx context.Context, req DraftRequest) (string, error)
}

// replyState is a reply step's request_json: the draft kind, then the body
// stored before the send and the marker its probe looks for.
type replyState struct {
	Kind    string `json:"kind,omitempty"`
	Body    string `json:"body,omitempty"`
	Marker  string `json:"marker,omitempty"`
	Drafted bool   `json:"drafted,omitempty"` // false = the template
	Note    string `json:"note,omitempty"`    // why the template was used
}

func replyStateOf(st store.Step) replyState {
	var rs replyState
	_ = json.Unmarshal(st.Request, &rs)
	return rs
}

// replyLimit is the reply length limit of platform (Steam comments: 999).
func replyLimit(platform string) int {
	if platform == "steam" {
		return 999
	}
	return maxReplyRunes
}

var urlRe = regexp.MustCompile(`https?://[^\s<>()\[\]"'` + "`" + `]+`)

func trimURL(u string) string { return strings.TrimRight(u, ".,;:!?*_") }

// checkDraft: non-empty, within limit; every URL one of allowed; a release
// reply names the version and each link exactly once.
func checkDraft(body string, req DraftRequest, allowed []string) error {
	body = strings.TrimSpace(body)
	switch {
	case body == "":
		return errors.New("empty draft")
	case len([]rune(body)) > req.Limit:
		return fmt.Errorf("draft is %d characters, the limit is %d", len([]rune(body)), req.Limit)
	}
	for _, u := range urlRe.FindAllString(body, -1) {
		if u = trimURL(u); !slices.Contains(allowed, u) {
			return fmt.Errorf("draft links to %s, which is not one of the release's pages", u)
		}
	}
	if p := replystyle.Check(body, replystyle.Input{Kind: req.Kind, Limit: req.Limit}); len(p) > 0 {
		return errors.New("reply style: " + strings.Join(p, "; "))
	}
	if req.Kind != DraftReleased {
		return nil
	}
	if req.Version != "" && !strings.Contains(body, req.Version) {
		return fmt.Errorf("draft does not name the version %s", req.Version)
	}
	for _, l := range req.Links {
		if n := strings.Count(body, l.URL); n != 1 {
			return fmt.Errorf("draft has the link %s %d times (want once)", l.URL, n)
		}
	}
	return nil
}

// draftBody drafts req (the drafter, checked; else fallback) and appends the
// GitHub marker. drafted = the agent's text was used; note says why not.
func (e *Engine) draftBody(ctx context.Context, req DraftRequest, allowed []string, fallback, marker string, github bool) (body string, drafted bool, note string) {
	suffix := ""
	if github {
		suffix = "\n\n<!-- " + marker + " -->"
	}
	req.Limit = max(100, req.Limit-len([]rune(suffix)))
	if e.d.Drafter != nil {
		b, err := e.d.Drafter.DraftReply(ctx, req)
		if err == nil {
			err = checkDraft(b, req, allowed)
		}
		if err == nil {
			return strings.TrimSpace(b) + suffix, true, ""
		}
		note = err.Error()
		e.d.Log.Warn("autopilot: reply draft refused, template used", "item", req.ItemID, "err", err)
	} else {
		note = "no drafter"
	}
	if fallback == "" {
		return "", false, note
	}
	if r := []rune(fallback); len(r) > req.Limit {
		fallback = string(r[:req.Limit])
	}
	return fallback + suffix, false, note
}

// releasedTemplate is the fallback reply of a release: version, links, thanks.
func releasedTemplate(tag string, links []Link) string {
	var b strings.Builder
	b.WriteString("Fixed in " + tag + ".\n\n")
	for _, l := range links {
		b.WriteString("- " + l.Name + ": " + l.URL + "\n")
	}
	if len(links) == 0 {
		b.WriteString("The update is out.\n")
	}
	b.WriteString("\nThanks for the report!")
	return b.String()
}

const needsInfoTemplate = "Thanks for the report! To fix this I need a bit more detail: the steps to reproduce it, " +
	"the mod and game versions, and the error message or log if there is one."

// releaseLinks are the downloadable places of release run id: its GitHub
// release and every available target with a page.
func (e *Engine) releaseLinks(ctx context.Context, rel store.Run) ([]Link, *Manifest, error) {
	m, err := manifestOf(rel)
	if err != nil {
		return nil, nil, err
	}
	links, err := e.replyLinks(ctx, &rc{run: rel, m: m})
	return links, m, err
}

// fixChanges are the commit subjects of fix run fr (what it fixed).
func (e *Engine) fixChanges(ctx context.Context, fr store.Run) ([]string, string) {
	fm, err := fixManifestOf(fr)
	if err != nil || fm.HeadSHA == "" {
		return nil, ""
	}
	lang := ""
	if fm.Triage != nil {
		lang = fm.Triage.Language
	}
	return e.subjects(ctx, fm.Folder, fm.StartSHA, fm.HeadSHA), lang
}

func linkURLs(links []Link, extra ...string) []string {
	out := slices.Clone(extra)
	for _, l := range links {
		out = append(out, l.URL)
	}
	return slices.DeleteFunc(out, func(s string) bool { return s == "" })
}

// draftFor drafts the reply of a fix run's own reply step (kind draft) and
// returns its body ("" = none: a question the drafter could not answer).
func (e *Engine) draftFor(ctx context.Context, r store.Run, m *FixManifest, kind, marker string) (string, replyState) {
	code, _ := e.d.Store.ItemCodeProject(ctx, m.ItemID)
	req := DraftRequest{ItemID: m.ItemID, Kind: kind, Limit: replyLimit(code.Platform), LogPath: e.agentLog(r.ID, StepReply)}
	if m.Triage != nil {
		req.Language = m.Triage.Language
		req.Notes = m.Triage.Missing
	}
	if m.Notes != "" {
		req.Notes = strings.TrimSpace(req.Notes + "\n" + m.Notes)
	}
	allowed := []string{m.RepoURL, m.ItemURL}
	fallback := ""
	switch kind {
	case DraftNeedsInfo:
		fallback = needsInfoTemplate
	case DraftFeedback, DraftSuggestion:
		fallback = replystyle.Fallback(kind, req.Language)
	case DraftReleased:
		rel, err := e.d.Store.Run(ctx, m.ReleaseRun)
		if err == nil {
			links, rm, lerr := e.releaseLinks(ctx, rel)
			if lerr == nil {
				req.Version, req.Links = rm.Version, links
				if orig, err := e.d.Store.Run(ctx, m.OriginalRun); err == nil {
					req.Changes, _ = e.fixChanges(ctx, orig)
				}
				fallback = releasedTemplate(rm.Tag, links)
			}
		}
	}
	body, drafted, note := e.draftBody(ctx, req, linkURLs(req.Links, allowed...), fallback, marker, code.Platform == store.CodePlatform)
	rs := replyState{Kind: kind, Body: body, Drafted: drafted, Note: note}
	rs.Marker = marker
	if code.Platform != store.CodePlatform {
		rs.Marker = firstLine(body)
	}
	return body, rs
}

// firstLine is a reply's first non-empty line (the marker on platforms
// without hidden comments), at most 120 runes.
func firstLine(s string) string {
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return clipRunes(l, 120)
		}
	}
	return ""
}

// fixReplyStep posts a fix run's reply (question answer, details request, or
// a duplicate's release) once: the draft is stored first, the send is
// compare-and-set, a crash in between is settled by the read-back probe.
func (e *Engine) fixReplyStep(ctx context.Context, r store.Run, m *FixManifest, st store.Step) (bool, error) {
	if e.d.Items == nil {
		_, err := e.transition(ctx, st, st.State, store.StepSkipped, store.StepUpdate{Error: new("replies are not available")})
		return err == nil, err
	}
	rs := replyStateOf(st)
	if st.State == store.StepSending || (st.State == store.StepPending && st.Attempt > 0) {
		ref, found, err := e.d.Items.FindReply(ctx, m.ItemID, rs.Marker)
		switch {
		case err != nil:
			if st.State == store.StepSending {
				st, err = e.transition(ctx, st, store.StepSending, store.StepUnknown, store.StepUpdate{Error: new("probe: " + err.Error())})
			} else {
				st, err = e.transition(ctx, st, store.StepPending, store.StepUnknown, store.StepUpdate{Error: new("probe: " + err.Error())})
			}
			if err == nil {
				e.holdFix(ctx, r, m, "check:"+stepName(st), st.Error)
			}
			return false, err
		case found:
			_, err := e.transition(ctx, st, st.State, store.StepSent, store.StepUpdate{ExternalRef: &ref, Error: new("")})
			return err == nil, err
		}
		if st.State == store.StepSending {
			n, err := e.transition(ctx, st, store.StepSending, store.StepPending, store.StepUpdate{Error: new("not posted (probe); will post")})
			if err != nil {
				return false, err
			}
			st = n
		}
	}
	if rs.Body == "" {
		marker := fmt.Sprintf("issuewatcher:reply:fix:%d:%d", r.ID, m.ItemID)
		body, nrs := e.draftFor(ctx, r, m, rs.Kind, marker)
		if body == "" { // nothing to post: the owner answers
			_, err := e.transition(ctx, st, store.StepPending, store.StepSkipped, store.StepUpdate{Error: new("no draft: " + nrs.Note)})
			if err == nil {
				e.itemEvent(ctx, r, m.ItemID, "reply.pending", store.SeverityAttention, fmt.Sprintf("#%d: ответ не составлен — ответьте сами", m.Number),
					map[string]any{"reason": nrs.Note, "url": m.ItemURL})
			}
			return err == nil, err
		}
		b, _ := json.Marshal(nrs)
		n, err := e.transition(ctx, st, store.StepPending, store.StepPending, store.StepUpdate{Request: b})
		if err != nil {
			return false, err
		}
		st, rs = n, nrs
	}
	key := st.Target + ":fix:" + strconv.FormatInt(r.ID, 10)
	sending, err := e.transition(ctx, st, store.StepPending, store.StepSending, store.StepUpdate{IncAttempt: true, IdemKey: &key})
	if err != nil {
		return false, err
	}
	if e.hook != nil {
		if err := e.hook("before", sending); err != nil {
			return false, errCrash
		}
	}
	ref, serr := e.d.Items.Reply(ctx, m.ItemID, rs.Body)
	if e.hook != nil {
		if err := e.hook("after", sending); err != nil {
			return false, errCrash
		}
	}
	ctx = context.WithoutCancel(ctx)
	if serr == nil {
		_, err := e.transition(ctx, sending, store.StepSending, store.StepSent, store.StepUpdate{ExternalRef: &ref, Error: new("")})
		if err == nil {
			e.itemEvent(ctx, r, m.ItemID, "reply.posted", store.SeverityInfo, fmt.Sprintf("#%d: ответ отправлен (%s)", m.Number, rs.Kind),
				map[string]any{"ref": ref, "kind": rs.Kind, "drafted": rs.Drafted})
		}
		return err == nil, err
	}
	ref, found, perr := e.d.Items.FindReply(ctx, m.ItemID, rs.Marker)
	switch {
	case perr != nil:
		_, err := e.transition(ctx, sending, store.StepSending, store.StepUnknown, store.StepUpdate{Error: new(serr.Error() + "; probe: " + perr.Error())})
		if err == nil {
			e.holdFix(ctx, r, m, "check:"+stepName(sending), serr.Error())
		}
		return false, err
	case found:
		_, err := e.transition(ctx, sending, store.StepSending, store.StepSent, store.StepUpdate{ExternalRef: &ref, Error: new("")})
		return err == nil, err
	}
	if _, err := e.transition(ctx, sending, store.StepSending, store.StepFailed, store.StepUpdate{Error: new(serr.Error())}); err != nil {
		return false, err
	}
	e.holdFix(ctx, r, m, "failed:"+stepName(sending), serr.Error())
	return false, nil
}
