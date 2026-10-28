package outbound

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/modkit"
	"github.com/UberMorgott/issuewatcher/internal/provider/steam"
)

// Request is one outbound call.
type Request struct {
	Verb string `json:"verb"`
	URL  string `json:"url"`
	// Parent is the comment a reply answers (an id or its URL) when URL is the page.
	Parent string `json:"parent,omitempty"`
	Body   string `json:"body,omitempty"`
	DryRun bool   `json:"dryRun,omitempty"`
}

// Plan is what a write sends (the whole answer of a dry run).
type Plan struct {
	// Endpoint is the platform call the body goes to.
	Endpoint string `json:"endpoint"`
	// Gate is the check that lets the write through (open, or it is refused).
	Gate string `json:"gate"`
	// LiveVerified: this verb on this platform has been posted live once and
	// read back (docs/AGENT-CLI.md → Support matrix); false = only fake-tested.
	LiveVerified bool   `json:"liveVerified"`
	Body         string `json:"body"`
	BodyLength   int    `json:"bodyLength"`
	MaxLength    int    `json:"maxLength"`
	// Markdown: the platform renders Markdown (else plain text / BBCode).
	Markdown bool   `json:"markdown"`
	Note     string `json:"note,omitempty"`
}

// Comment is a comment posted or read.
type Comment struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"`
	Body      string    `json:"body,omitempty"`
	URL       string    `json:"url,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
}

// Thread is one item read (an issue, a root comment with its replies, a bug
// report, a discussion thread).
type Thread struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind,omitempty"`
	Title     string    `json:"title,omitempty"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	URL       string    `json:"url,omitempty"`
	Open      bool      `json:"open"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
	Comments  []Comment `json:"comments"`
}

// Result codes of a success.
const (
	CodeOK     = "ok"
	CodeDryRun = "dry_run"
	CodePosted = "posted"
)

// Result is the answer of Do (stable JSON, docs/AGENT-CLI.md → JSON).
type Result struct {
	OK       bool   `json:"ok"`
	Code     string `json:"code"`
	Verb     string `json:"verb"`
	Provider string `json:"provider"`
	Target   Target `json:"target"`
	// URL is the posted comment's permalink, else the target page.
	URL     string   `json:"url"`
	DryRun  bool     `json:"dryRun,omitempty"`
	Plan    *Plan    `json:"plan,omitempty"`
	Comment *Comment `json:"comment,omitempty"`
	Threads []Thread `json:"threads,omitempty"`
	Count   int      `json:"count,omitempty"`
}

// Service runs outbound calls through the app's providers.
type Service struct {
	// Providers returns the connected provider of platform, nil when the
	// platform is off or not wired.
	Providers func(platform string) provider.Provider
}

// topLevelPoster posts a new top-level comment on a Workshop item (steam.Provider).
type topLevelPoster interface {
	PostComment(ctx context.Context, fileID, body string, dryRun bool) (steam.CommentResult, error)
}

// issueFetcher loads GitHub issues by number (github.Provider, provider.Poller).
type issueFetcher interface {
	FetchChanged(ctx context.Context, project provider.Project, numbers []int) ([]provider.Item, error)
}

// discussionReader reads one discussion thread, or every thread of a mod when
// thread is "", without touching the syncer's sweep state (factorio.Provider).
type discussionReader interface {
	ReadDiscussion(ctx context.Context, mod, thread string) ([]provider.Item, error)
}

// liveVerified: verb × platform posted live once and read back (TASKS.md).
var liveVerified = map[string]bool{
	"github/" + VerbPost:    true, // reply_item on sandbox issue, 2026-09-29 (same addComment call)
	"github/" + VerbReply:   true,
	"factorio/" + VerbReply: true, // item 807, 2026-10-26
}

// Do resolves req.URL and runs req.Verb. Refusals and failures are *Error.
func (s *Service) Do(ctx context.Context, req Request) (Result, error) {
	switch req.Verb {
	case VerbPost, VerbReply, VerbRead, VerbDownload:
	default:
		return Result{}, errf(CodeUnsupported, "unknown verb %q (post, reply, read, download)", req.Verb)
	}
	t, err := Resolve(req.URL, req.Parent)
	if err != nil {
		return Result{}, err
	}
	var p provider.Provider
	if s.Providers != nil {
		p = s.Providers(t.Platform)
	}
	if p == nil {
		return Result{}, errf(CodeNoProvider, "%s is not connected or switched off (Settings › Платформы)", t.Platform)
	}
	res := Result{Verb: req.Verb, Provider: t.Platform, Target: t, URL: t.URL}
	if req.Verb == VerbRead || req.Verb == VerbDownload {
		threads, err := s.read(ctx, p, t)
		if err != nil {
			return Result{}, mapErr(err)
		}
		res.OK, res.Code, res.Threads, res.Count = true, CodeOK, threads, len(threads)
		return res, nil
	}
	if strings.TrimSpace(req.Body) == "" {
		return Result{}, errf(CodeEmptyBody, "empty body")
	}
	return s.write(ctx, p, t, req, res)
}

// write posts (or plans) req.Body on t.
func (s *Service) write(ctx context.Context, p provider.Provider, t Target, req Request, res Result) (Result, error) {
	caps := p.Capabilities()
	plan := &Plan{Gate: "Capabilities.Reply", LiveVerified: liveVerified[t.Platform+"/"+req.Verb], Body: req.Body,
		BodyLength: provider.ReplyLength(req.Body), MaxLength: caps.ReplyLimit(), Markdown: caps.Markdown}
	ext := t.Thread
	switch {
	case t.Platform == "github":
		items, err := s.issue(ctx, p, t)
		if err != nil {
			return Result{}, err
		}
		ext = items[0].ExternalID
		plan.Endpoint = "GraphQL addComment(subjectId " + ext + ")"
		if req.Verb == VerbReply {
			plan.Note = "GitHub issue comments are flat: the reply is a new comment on the issue"
		}
	case t.Platform == "steam" && req.Verb == VerbPost:
		return s.steamPost(ctx, p, t, req, res, plan)
	case req.Verb == VerbPost:
		return Result{}, errf(CodeUnsupported, "%s: a new top-level comment is not supported yet; reply to a thread instead (--parent or a thread URL)", t.Platform)
	case ext == "":
		return Result{}, errf(CodeNeedsParent, "%s: reply needs the thread or comment (a comment/thread URL or --parent)", t.Platform)
	case t.Platform == "steam":
		plan.Endpoint = "POST steamcommunity.com/comment/PublishedFile_Public/post/<creator>/" + t.Project + "/"
		plan.Note = "Steam comments are flat: a new top-level comment starting with @<author> of comment " + t.Comment + " (unless the body starts with @)"
		plan.Gate = "Capabilities.Reply (Steam web session verified by one live post)"
	case t.Platform == "nexus" && strings.HasPrefix(ext, "bug:"):
		plan.Endpoint = "reply_mod_bug (/mod_bug/" + t.Comment + "/reply)"
	case t.Platform == "nexus":
		plan.Endpoint = "post_mod_comment{parent_id " + t.Comment + "} (/mod/comment)"
	case t.Platform == "factorio":
		plan.Endpoint = "thread reply form " + t.URL
	case t.Platform == "curseforge":
		plan.Endpoint = "POST /api/v1/comments {entityId " + t.Project + ", parentId " + t.Comment + "}"
	default:
		return Result{}, errf(CodeUnsupported, "%s: %s is not supported", t.Platform, req.Verb)
	}
	if !caps.Reply {
		return Result{}, errf(CodeReplyOff, "%s: replying is off (gate %s is closed)", t.Platform, plan.Gate)
	}
	if plan.BodyLength > plan.MaxLength {
		return Result{}, errf(CodeTooLong, "%d characters, %s accepts at most %d", plan.BodyLength, t.Platform, plan.MaxLength)
	}
	res.Plan = plan
	if req.DryRun {
		res.OK, res.Code, res.DryRun = true, CodeDryRun, true
		return res, nil
	}
	c, err := p.Reply(ctx, ext, req.Body)
	if err != nil {
		return Result{}, mapErr(err)
	}
	res.OK, res.Code, res.Comment = true, CodePosted, toComment(c, false)
	if c.URL != "" {
		res.URL = c.URL
	}
	return res, nil
}

// steamPost: a new top-level comment through steam.Provider.PostComment (its
// own gate: the signed-in web session; Capabilities.Reply is not needed).
func (s *Service) steamPost(ctx context.Context, p provider.Provider, t Target, req Request, res Result, plan *Plan) (Result, error) {
	tp, ok := p.(topLevelPoster)
	if !ok {
		return Result{}, errf(CodeUnsupported, "steam: this provider cannot post top-level comments")
	}
	plan.Gate = "signed-in Steam web session (stored cookies, not expired)"
	r, err := tp.PostComment(ctx, t.Project, req.Body, req.DryRun)
	if err != nil {
		return Result{}, mapErr(err)
	}
	plan.Endpoint, plan.Body, plan.BodyLength, plan.MaxLength = "POST "+r.Endpoint, r.Body, r.BodyLength, steam.MaxReply
	if !r.SignedIn {
		plan.Note = "not signed in: the post would be refused (not_signed_in)"
	}
	res.Plan = plan
	if req.DryRun {
		res.OK, res.Code, res.DryRun = true, CodeDryRun, true
		return res, nil
	}
	res.OK, res.Code, res.URL = true, CodePosted, r.URL
	if r.Comment != nil {
		res.Comment = toComment(*r.Comment, false)
	}
	return res, nil
}

// issue loads the GitHub issue of t (its node id and comments).
func (s *Service) issue(ctx context.Context, p provider.Provider, t Target) ([]provider.Item, error) {
	f, ok := p.(issueFetcher)
	if !ok {
		return nil, errf(CodeUnsupported, "github: this provider cannot load an issue by number")
	}
	items, err := f.FetchChanged(ctx, provider.Project{ExternalID: t.Project}, []int{t.Number})
	var skipped *provider.SkippedError
	if errors.As(err, &skipped) && len(items) == 0 {
		return nil, errf(CodeNotFound, "github: %s#%d is not an issue (a pull request, deleted or transferred?): %v", t.Project, t.Number, err)
	}
	if err != nil && !errors.As(err, &skipped) {
		return nil, mapErr(err)
	}
	if len(items) == 0 {
		return nil, errf(CodeNotFound, "github: %s#%d not found", t.Project, t.Number)
	}
	return items[:1], nil
}

// read loads t's thread(s).
func (s *Service) read(ctx context.Context, p provider.Provider, t Target) ([]Thread, error) {
	var (
		items []provider.Item
		err   error
	)
	switch t.Platform {
	case "github":
		items, err = s.issue(ctx, p, t)
	case "factorio":
		dr, ok := p.(discussionReader)
		if !ok {
			return nil, errf(CodeUnsupported, "factorio: this provider cannot read a thread by URL")
		}
		items, err = dr.ReadDiscussion(ctx, t.Project, strings.TrimPrefix(t.Thread, "thread:"))
	default:
		items, err = p.SyncItems(ctx, provider.Project{ExternalID: t.Project, URL: t.URL}, time.Time{})
		if err == nil && t.Thread != "" {
			items = filterItems(items, t.Thread)
		}
	}
	if err != nil {
		return nil, err
	}
	if t.Thread != "" && len(items) == 0 {
		return nil, errf(CodeNotFound, "%s: thread %s not found on %s", t.Platform, t.Thread, t.URL)
	}
	out := make([]Thread, 0, len(items))
	for _, it := range items {
		th := Thread{ID: it.ExternalID, Kind: it.Kind, Title: it.Title, Author: it.Author, Body: it.Body, URL: it.URL,
			Open: it.Open, CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt, Comments: make([]Comment, 0, len(it.Comments))}
		for _, c := range it.Comments {
			th.Comments = append(th.Comments, *toComment(c, true))
		}
		out = append(out, th)
	}
	return out, nil
}

func filterItems(items []provider.Item, id string) []provider.Item {
	for _, it := range items {
		if it.ExternalID == id {
			return []provider.Item{it}
		}
	}
	return nil
}

func toComment(c provider.Comment, body bool) *Comment {
	out := &Comment{ID: c.ExternalID, Author: c.Author, URL: c.URL, CreatedAt: c.CreatedAt}
	if body {
		out.Body = c.Body
	}
	return out
}

// mapErr gives a provider error its stable code.
func mapErr(err error) error {
	var e *Error
	var rl *provider.RateLimitError
	switch {
	case errors.As(err, &e):
		return e
	case errors.Is(err, modkit.ErrUnknownOutcome), errors.Is(err, steam.ErrOutcomeUnknown):
		return &Error{Code: CodeOutcomeUnknown, Msg: err.Error() + " — sent once, never retried: check the page before posting again"}
	case errors.Is(err, provider.ErrReplyTooLong):
		return &Error{Code: CodeTooLong, Msg: err.Error()}
	case errors.Is(err, provider.ErrRelogin):
		return &Error{Code: CodeRelogin, Msg: err.Error()}
	case errors.Is(err, provider.ErrNotSignedIn):
		return &Error{Code: CodeNotSignedIn, Msg: err.Error()}
	case errors.As(err, &rl):
		return &Error{Code: CodeRateLimited, Msg: err.Error()}
	}
	return &Error{Code: CodePlatformError, Msg: fmt.Sprint(err)}
}
