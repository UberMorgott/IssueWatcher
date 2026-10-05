package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Reply and close steps of a release run (docs/AUTOPILOT.md → reply:<item>,
// close:<item>): after every target is available (or skipped), each item of
// the manifest gets one reply with the version and the links of the available
// targets; a GitHub issue is then closed as completed.

// maxReplyRunes bounds a reply (docs: ≤ 2000 chars).
const maxReplyRunes = 2000

// hasGate: the project has a verify gate in its folder.
func (e *Engine) hasGate(project, localPath string) bool {
	if e.d.HasGate != nil {
		return e.d.HasGate(project, localPath)
	}
	if e.d.Gate == nil {
		return false
	}
	return exists(filepath.Join(localPath, ".aegis")) || strings.TrimSpace(e.d.Settings().Agents.Projects[project].Verify) != ""
}

// refusalEvents are the refusals that need the owner's configuration: a real
// start refused for them writes an attention event (once while unread).
var refusalEvents = []string{CodeNoVerify, CodeNoSmoke}

// quietRefusals never write an event (they clear on their own: wait, retry).
var quietRefusals = []string{CodeBusy, CodeCapReached}

// refusedEvent records why a release could not start: the configuration
// refusals for every origin, any other refusal of the autopilot timer.
func (e *Engine) refusedEvent(ctx context.Context, p Plan, origin string) {
	for _, r := range p.Refusals {
		switch {
		case slices.Contains(refusalEvents, r.Code):
		case origin == store.RunOriginAuto && !slices.Contains(quietRefusals, r.Code):
		default:
			continue
		}
		e.attentionOnce(ctx, store.AutopilotEvent{ProjectID: p.ProjectID, Kind: "release.refused", Severity: store.SeverityAttention,
			Title: fmt.Sprintf("%s: релиз не запущен (%s)", p.Project, r.Code)}, map[string]any{"code": r.Code, "message": r.Message, "origin": origin})
	}
}

// attentionOnce writes ev unless an unread event of the same kind, project,
// item and detail code exists.
func (e *Engine) attentionOnce(ctx context.Context, ev store.AutopilotEvent, detail map[string]any) {
	ctx = context.WithoutCancel(ctx)
	code, _ := detail["code"].(string)
	if list, err := e.d.Store.Events(ctx, true, 500); err == nil {
		for _, x := range list {
			if x.Kind != ev.Kind || x.ProjectID != ev.ProjectID || x.ItemID != ev.ItemID {
				continue
			}
			var d map[string]any
			_ = json.Unmarshal(x.Detail, &d)
			if c, _ := d["code"].(string); c == code {
				return
			}
		}
	}
	ev.Detail, _ = json.Marshal(detail)
	out, err := e.d.Store.AddEvent(ctx, ev)
	if err != nil {
		e.d.Log.Error("autopilot: event", "kind", ev.Kind, "err", err)
		return
	}
	e.d.OnEvent(out)
}

// itemEvent writes an activity log event about one item of run r.
func (e *Engine) itemEvent(ctx context.Context, r store.Run, item int64, kind, severity, title string, detail map[string]any) {
	b, _ := json.Marshal(detail)
	ev, err := e.d.Store.AddEvent(context.WithoutCancel(ctx), store.AutopilotEvent{RunID: r.ID, ProjectID: r.ProjectID, ItemID: item,
		Kind: kind, Severity: severity, Title: title, Detail: b})
	if err != nil {
		e.d.Log.Error("autopilot: event", "run", r.ID, "err", err)
		return
	}
	e.d.OnEvent(ev)
}

// sentEvent logs a reply posted / an issue closed.
func (e *Engine) sentEvent(ctx context.Context, c *rc, st store.Step, ref string) {
	item, _ := strconv.ParseInt(st.Target, 10, 64)
	switch st.Step {
	case StepReply:
		e.itemEvent(ctx, c.run, item, "reply.posted", store.SeverityInfo, "Ответ отправлен: "+c.m.Tag,
			map[string]any{"version": c.m.Version, "ref": ref})
	case StepClose:
		e.itemEvent(ctx, c.run, item, "issue.closed", store.SeverityInfo, "Issue закрыт после релиза "+c.m.Tag,
			map[string]any{"version": c.m.Version})
	}
}

func (c *rc) replyItem(target string) (ReplyItem, bool) {
	for _, it := range c.m.Replies {
		if strconv.FormatInt(it.ID, 10) == target {
			return it, true
		}
	}
	return ReplyItem{}, false
}

// replyMarker identifies the reply on its item: GitHub gets an invisible
// HTML comment; elsewhere the first line (with the version) is the marker.
func replyMarker(c *rc, it ReplyItem) string {
	if it.Platform == store.CodePlatform {
		return fmt.Sprintf("issuewatcher:reply:%d:%d", c.run.ID, it.ID)
	}
	return replyHead(c)
}

func replyHead(c *rc) string { return "Fixed in " + c.m.Tag + "." }

var platformNames = map[string]string{"nexus": "Nexus Mods", "factorio": "Factorio Mod Portal", "steam": "Steam Workshop",
	"curseforge": "CurseForge", "github": "GitHub"}

// checkReply: every target's publish and availability went through or was
// skipped (a failed target holds the replies until the owner skips it).
func (e *Engine) checkReply(ctx context.Context, c *rc, st store.Step) (string, error) {
	if e.d.Items == nil {
		return "failed:" + stepName(st), errors.New("replies are not available")
	}
	if _, ok := c.replyItem(st.Target); !ok {
		return "failed:" + stepName(st), errors.New("item not in the manifest")
	}
	steps, err := e.d.Store.RunSteps(ctx, c.run.ID)
	if err != nil {
		return "check:" + stepName(st), err
	}
	for _, s := range steps {
		if (s.Step == StepPublish || s.Step == StepAvailable) && s.State != store.StepSent && s.State != store.StepSkipped {
			return HeldReplyWaiting, fmt.Errorf("%s is %s: skip the target or resume once it is available", stepName(s), s.State)
		}
	}
	if replyStateOf(st).Body != "" {
		return "", nil // drafted before (a crash or a failed send): the same text again
	}
	// Draft once, stored before the send: a resend posts the same text and the
	// probe looks for the same marker.
	it, _ := c.replyItem(st.Target)
	body, drafted, note := e.releaseDraft(ctx, c, it)
	rs := replyState{Kind: DraftReleased, Body: body, Drafted: drafted, Note: note, Marker: replyMarker(c, it)}
	if it.Platform != store.CodePlatform {
		rs.Marker = firstLine(body)
	}
	b, _ := json.Marshal(rs)
	if _, err := e.transition(ctx, st, store.StepPending, store.StepPending, store.StepUpdate{Request: b}); err != nil {
		return "check:" + stepName(st), err
	}
	return "", nil
}

// releaseDraft drafts item it's release reply: what its fix run changed, the
// version and the links of the available, non-skipped targets; the template
// when the drafter fails or its draft does not pass the check.
func (e *Engine) releaseDraft(ctx context.Context, c *rc, it ReplyItem) (string, bool, string) {
	links, err := e.replyLinks(ctx, c)
	if err != nil {
		links = nil
	}
	req := DraftRequest{ItemID: it.ID, Kind: DraftReleased, Version: c.m.Version, Links: links, Limit: replyLimit(it.Platform),
		LogPath: filepath.Join(e.runDir(c.run.ID), "logs", "reply-"+strconv.FormatInt(it.ID, 10)+".log")}
	for _, id := range c.m.FixRuns {
		fr, err := e.d.Store.Run(ctx, id)
		if err != nil {
			continue
		}
		its, _ := e.d.Store.RunItems(ctx, id)
		if slices.ContainsFunc(its, func(x store.RunItem) bool { return x.ItemID == it.ID }) {
			req.Changes, req.Language = e.fixChanges(ctx, fr)
			break
		}
	}
	if len(req.Changes) == 0 && c.m.Changelog != "" {
		for l := range strings.SplitSeq(c.m.Changelog, "\n") {
			if l = strings.TrimSpace(l); l != "" && len(req.Changes) < 10 {
				req.Changes = append(req.Changes, l)
			}
		}
	}
	return e.draftBody(ctx, req, linkURLs(links, c.m.RepoURL, it.URL), releasedTemplate(c.m.Tag, links), replyMarker(c, it),
		it.Platform == store.CodePlatform)
}

// replyLinks: the GitHub release and every available target with a page.
func (e *Engine) replyLinks(ctx context.Context, c *rc) ([]Link, error) {
	steps, err := e.d.Store.RunSteps(ctx, c.run.ID)
	if err != nil {
		return nil, err
	}
	var links []Link
	for _, st := range steps {
		switch {
		case st.Step == StepGHRelease && st.State == store.StepSent && strings.HasPrefix(st.ExternalRef, "http"):
			links = append(links, Link{Name: "GitHub", URL: st.ExternalRef})
		case st.Step == StepAvailable && st.State == store.StepSent:
			if t, ok := c.target(st.Target); ok && t.URL != "" {
				links = append(links, Link{Name: cmpOrStr(platformNames[t.Platform], t.Platform), URL: t.URL})
			}
		}
	}
	return links, nil
}

// probeReply: a step never sent has nothing to find; after a send the own
// reply is looked up (no proof of absence → unknown).
func (e *Engine) probeReply(ctx context.Context, c *rc, st store.Step) (probeResult, error) {
	if st.Attempt == 0 && st.State == store.StepPending {
		return probeResult{}, nil
	}
	if e.d.Items == nil {
		return probeResult{}, errors.New("replies are not available")
	}
	it, ok := c.replyItem(st.Target)
	if !ok {
		return probeResult{}, errors.New("item not in the manifest")
	}
	marker := replyMarker(c, it)
	if rs := replyStateOf(st); rs.Marker != "" {
		marker = rs.Marker // the stored draft's (its first line off GitHub)
	}
	ref, found, err := e.d.Items.FindReply(ctx, it.ID, marker)
	if err != nil || !found {
		return probeResult{}, err
	}
	return probeResult{present: true, ref: ref}, nil
}

func (e *Engine) sendReply(ctx context.Context, c *rc, st store.Step) (string, error) {
	it, _ := c.replyItem(st.Target)
	rs := replyStateOf(st)
	if rs.Body == "" { // checkReply stores it before every send
		return "", holdError{reason: "failed:" + stepName(st), err: errors.New("no stored reply draft"), definite: true}
	}
	return e.d.Items.Reply(ctx, it.ID, rs.Body)
}

func (e *Engine) probeClose(ctx context.Context, c *rc, st store.Step) (probeResult, error) {
	if e.d.Items == nil {
		return probeResult{}, errors.New("closing items is not available")
	}
	it, ok := c.replyItem(st.Target)
	if !ok {
		return probeResult{}, errors.New("item not in the manifest")
	}
	closed, err := e.d.Items.ItemClosed(ctx, it.ID)
	if err != nil || !closed {
		return probeResult{}, err
	}
	return probeResult{present: true, ref: "closed"}, nil
}

func (e *Engine) sendClose(ctx context.Context, c *rc, st store.Step) (string, error) {
	it, _ := c.replyItem(st.Target)
	if err := e.d.Items.CloseItem(ctx, it.ID); err != nil {
		return "", err
	}
	return "closed", nil
}
