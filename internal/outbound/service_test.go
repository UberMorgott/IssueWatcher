package outbound

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/modkit"
	"github.com/UberMorgott/issuewatcher/internal/provider/steam"
)

// fake is a provider that records replies and never touches a network.
type fake struct {
	platform string
	caps     provider.Capabilities
	items    []provider.Item
	replyErr error
	replies  []string // ext|body
	syncs    int
}

func (f *fake) Platform() string                                         { return f.platform }
func (f *fake) Capabilities() provider.Capabilities                      { return f.caps }
func (f *fake) Account(context.Context) (string, error)                  { return "me", nil }
func (f *fake) ListProjects(context.Context) ([]provider.Project, error) { return nil, nil }
func (f *fake) SyncItems(_ context.Context, _ provider.Project, _ time.Time) ([]provider.Item, error) {
	f.syncs++
	return f.items, nil
}
func (f *fake) Reply(_ context.Context, ext, body string) (provider.Comment, error) {
	f.replies = append(f.replies, ext+"|"+body)
	if f.replyErr != nil {
		return provider.Comment{}, f.replyErr
	}
	return provider.Comment{ExternalID: "c1", Author: "me", Body: body, URL: "https://example.test/c1"}, nil
}

// fakeGitHub loads issues by number.
type fakeGitHub struct {
	fake
	fetched []int
}

func (f *fakeGitHub) FetchChanged(_ context.Context, project provider.Project, numbers []int) ([]provider.Item, error) {
	f.fetched = append(f.fetched, numbers...)
	if project.ExternalID != "o/r" || numbers[0] != 7 {
		return nil, &provider.SkippedError{Items: map[int]error{numbers[0]: errors.New("not an issue")}}
	}
	return []provider.Item{{ExternalID: "I_kw7", Title: "Crash", Author: "bob", Body: "boom", Open: true,
		Comments: []provider.Comment{{ExternalID: "IC_1", Author: "amy", Body: "same"}}}}, nil
}

// fakeSteam posts top-level comments.
type fakeSteam struct {
	fake
	posts []string
}

func (f *fakeSteam) PostComment(_ context.Context, fileID, body string, dryRun bool) (steam.CommentResult, error) {
	r := steam.CommentResult{Item: fileID, Creator: "765", Endpoint: "https://steamcommunity.com/comment/PublishedFile_Public/post/765/" + fileID + "/",
		Body: body, BodyLength: len(body), SignedIn: true, DryRun: dryRun, URL: "https://steamcommunity.com/sharedfiles/filedetails/?id=" + fileID}
	if !dryRun {
		f.posts = append(f.posts, body)
		r.Comment = &provider.Comment{ExternalID: "comment:" + fileID + "/9", URL: r.URL + "#comment_9"}
		r.URL = r.Comment.URL
	}
	return r, nil
}

// fakeFactorio reads discussions.
type fakeFactorio struct {
	fake
	read []string
}

func (f *fakeFactorio) ReadDiscussion(_ context.Context, mod, thread string) ([]provider.Item, error) {
	f.read = append(f.read, mod+"/"+thread)
	return []provider.Item{{ExternalID: "thread:" + thread, Title: "(untitled)", Body: "hi"}}, nil
}

func service(ps ...provider.Provider) *Service {
	return &Service{Providers: func(platform string) provider.Provider {
		for _, p := range ps {
			if p.Platform() == platform {
				return p
			}
		}
		return nil
	}}
}

func code(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	return fmt.Sprint(err)
}

func TestDryRunPlansPerProvider(t *testing.T) {
	gh := &fakeGitHub{platform: "github", caps: provider.Capabilities{Reply: true, Markdown: true}}
	st := &fakeSteam{platform: "steam", caps: provider.Capabilities{Reply: true, MaxReply: steam.MaxReply}}
	nx := &fake{platform: "nexus", caps: provider.Capabilities{Reply: true}}
	fa := &fakeFactorio{platform: "factorio", caps: provider.Capabilities{Reply: true}}
	cf := &fake{platform: "curseforge", caps: provider.Capabilities{Reply: true}}
	s := service(gh, st, nx, fa, cf)
	cases := []struct {
		verb, url, parent string
		endpoint, gate    string
		live              bool
		max               int
	}{
		{VerbPost, "https://github.com/o/r/issues/7", "", "GraphQL addComment(subjectId I_kw7)", "Capabilities.Reply", true, provider.DefaultMaxReply},
		{VerbReply, "https://github.com/o/r/issues/7#issuecomment-1", "", "GraphQL addComment(subjectId I_kw7)", "Capabilities.Reply", true, provider.DefaultMaxReply},
		{VerbPost, "https://steamcommunity.com/sharedfiles/filedetails/?id=42", "", "POST https://steamcommunity.com/comment/PublishedFile_Public/post/765/42/", "signed-in Steam web session (stored cookies, not expired)", false, steam.MaxReply},
		{VerbReply, "https://steamcommunity.com/sharedfiles/filedetails/?id=42", "600", "POST steamcommunity.com/comment/PublishedFile_Public/post/<creator>/42/", "Capabilities.Reply (Steam web session verified by one live post)", false, steam.MaxReply},
		{VerbReply, "https://www.nexusmods.com/wartales/mods/202?jump_to_comment=5", "", "post_mod_comment{parent_id 5} (/mod/comment)", "Capabilities.Reply", false, provider.DefaultMaxReply},
		{VerbReply, "https://www.nexusmods.com/wartales/mods/202", "bug:900", "reply_mod_bug (/mod_bug/900/reply)", "Capabilities.Reply", false, provider.DefaultMaxReply},
		{VerbReply, "https://mods.factorio.com/mod/m/discussion/65f0c1a2b3c4d5e6f7a8b9c0", "", "thread reply form https://mods.factorio.com/mod/m/discussion/65f0c1a2b3c4d5e6f7a8b9c0", "Capabilities.Reply", true, provider.DefaultMaxReply},
		{VerbReply, "https://www.curseforge.com/projects/99", "8800", "POST /api/v1/comments {entityId 99, parentId 8800}", "Capabilities.Reply", false, provider.DefaultMaxReply},
	}
	for _, c := range cases {
		r, err := s.Do(t.Context(), Request{Verb: c.verb, URL: c.url, Parent: c.parent, Body: "Thanks, fixed", DryRun: true})
		if err != nil {
			t.Errorf("%s %s: %v", c.verb, c.url, err)
			continue
		}
		if !r.OK || r.Code != CodeDryRun || !r.DryRun || r.Plan == nil || r.Comment != nil {
			t.Errorf("%s %s: %+v", c.verb, c.url, r)
			continue
		}
		p := r.Plan
		if p.Endpoint != c.endpoint || p.Gate != c.gate || p.LiveVerified != c.live || p.MaxLength != c.max || p.Body != "Thanks, fixed" || p.BodyLength != 13 {
			t.Errorf("%s %s: plan %+v", c.verb, c.url, *p)
		}
	}
	if len(gh.replies)+len(st.replies)+len(st.posts)+len(nx.replies)+len(fa.replies)+len(cf.replies) != 0 {
		t.Fatalf("a dry run posted: %v %v %v %v %v %v", gh.replies, st.replies, st.posts, nx.replies, fa.replies, cf.replies)
	}
	if !gh.caps.Markdown || len(gh.fetched) != 2 {
		t.Fatalf("github dry run should load the issue: %v", gh.fetched)
	}
}

func TestWriteRefusals(t *testing.T) {
	off := &fake{platform: "nexus", caps: provider.Capabilities{Reply: false}}
	st := &fakeSteam{platform: "steam", caps: provider.Capabilities{Reply: true, MaxReply: 10}}
	gh := &fakeGitHub{platform: "github", caps: provider.Capabilities{Reply: true}}
	s := service(off, st, gh)
	cases := []struct {
		req  Request
		code string
	}{
		{Request{Verb: VerbReply, URL: "https://www.nexusmods.com/g/mods/1?jump_to_comment=2", Body: "x"}, CodeReplyOff},
		{Request{Verb: VerbReply, URL: "https://www.nexusmods.com/g/mods/1?jump_to_comment=2", Body: "x", DryRun: true}, CodeReplyOff},
		{Request{Verb: VerbReply, URL: "https://www.nexusmods.com/g/mods/1", Body: "x"}, CodeNeedsParent},
		{Request{Verb: VerbPost, URL: "https://www.nexusmods.com/g/mods/1", Body: "x"}, CodeUnsupported},
		{Request{Verb: VerbReply, URL: "https://steamcommunity.com/sharedfiles/filedetails/?id=4#comment_5", Body: "much longer than ten", DryRun: true}, CodeTooLong},
		{Request{Verb: VerbPost, URL: "https://github.com/o/r/issues/7", Body: "  \n"}, CodeEmptyBody},
		{Request{Verb: VerbPost, URL: "https://github.com/o/r/issues/8", Body: "x"}, CodeNotFound},
		{Request{Verb: VerbPost, URL: "https://mods.factorio.com/mod/m", Body: "x"}, CodeNoProvider},
		{Request{Verb: "delete", URL: "https://github.com/o/r/issues/7"}, CodeUnsupported},
		{Request{Verb: VerbRead, URL: "https://example.com/"}, CodeUnsupported},
	}
	for _, c := range cases {
		if _, err := s.Do(t.Context(), c.req); code(err) != c.code {
			t.Errorf("%+v: %v, want %s", c.req, err, c.code)
		}
	}
	if len(off.replies)+len(st.replies)+len(gh.replies) != 0 {
		t.Fatal("a refused write was sent")
	}
}

func TestWritePostsOnceAndMapsOutcome(t *testing.T) {
	gh := &fakeGitHub{platform: "github", caps: provider.Capabilities{Reply: true}}
	st := &fakeSteam{platform: "steam", caps: provider.Capabilities{Reply: true, MaxReply: steam.MaxReply}}
	fa := &fakeFactorio{platform: "factorio", caps: provider.Capabilities{Reply: true}, replyErr: fmt.Errorf("factorio: %w", modkit.ErrUnknownOutcome)}
	s := service(gh, st, fa)

	r, err := s.Do(t.Context(), Request{Verb: VerbPost, URL: "https://github.com/o/r/issues/7", Body: "Fixed in v1.2.3."})
	if err != nil || r.Code != CodePosted || r.URL != "https://example.test/c1" || r.Comment == nil || r.Comment.ID != "c1" ||
		len(gh.replies) != 1 || gh.replies[0] != "I_kw7|Fixed in v1.2.3." {
		t.Fatalf("github post %+v %v %v", r, err, gh.replies)
	}
	r, err = s.Do(t.Context(), Request{Verb: VerbPost, URL: "https://steamcommunity.com/sharedfiles/filedetails/?id=42", Body: "Update is out"})
	if err != nil || r.Code != CodePosted || !strings.HasSuffix(r.URL, "#comment_9") || len(st.posts) != 1 || len(st.replies) != 0 {
		t.Fatalf("steam post %+v %v", r, err)
	}
	r, err = s.Do(t.Context(), Request{Verb: VerbReply, URL: "https://steamcommunity.com/sharedfiles/filedetails/?id=42#comment_600", Body: "thx"})
	if err != nil || r.Code != CodePosted || len(st.replies) != 1 || st.replies[0] != "comment:42/600|thx" {
		t.Fatalf("steam reply %+v %v %v", r, err, st.replies)
	}
	_, err = s.Do(t.Context(), Request{Verb: VerbReply, URL: "https://mods.factorio.com/mod/m/discussion/65f0c1a2b3c4d5e6f7a8b9c0", Body: "hi"})
	if code(err) != CodeOutcomeUnknown || len(fa.replies) != 1 {
		t.Fatalf("factorio unknown outcome: %v, %d sends", err, len(fa.replies))
	}
	for _, c := range []struct {
		err  error
		code string
	}{
		{steam.ErrOutcomeUnknown, CodeOutcomeUnknown},
		{steam.ErrSessionExpired, CodeRelogin},
		{fmt.Errorf("x: %w", provider.ErrNotSignedIn), CodeNotSignedIn},
		{&provider.RateLimitError{Reset: time.Now()}, CodeRateLimited},
		{fmt.Errorf("%w: 2000", provider.ErrReplyTooLong), CodeTooLong},
		{errors.New("HTTP 500"), CodePlatformError},
	} {
		if got := code(mapErr(c.err)); got != c.code {
			t.Errorf("mapErr(%v) = %s, want %s", c.err, got, c.code)
		}
	}
}

func TestReadPerProvider(t *testing.T) {
	gh := &fakeGitHub{platform: "github"}
	nx := &fake{platform: "nexus", items: []provider.Item{
		{ExternalID: "comment:g/1/2", Body: "a", Comments: []provider.Comment{{ExternalID: "3", Author: "me", Body: "b"}}},
		{ExternalID: "bug:9", Body: "bug"},
	}}
	fa := &fakeFactorio{platform: "factorio"}
	s := service(gh, nx, fa)

	r, err := s.Do(t.Context(), Request{Verb: VerbRead, URL: "https://github.com/o/r/issues/7"})
	if err != nil || r.Code != CodeOK || r.Count != 1 || r.Threads[0].ID != "I_kw7" || r.Threads[0].Comments[0].Body != "same" {
		t.Fatalf("github read %+v %v", r, err)
	}
	r, err = s.Do(t.Context(), Request{Verb: VerbRead, URL: "https://www.nexusmods.com/g/mods/1"})
	if err != nil || r.Count != 2 {
		t.Fatalf("nexus read all %+v %v", r, err)
	}
	r, err = s.Do(t.Context(), Request{Verb: VerbDownload, URL: "https://www.nexusmods.com/g/mods/1", Parent: "2"})
	if err != nil || r.Verb != VerbDownload || r.Count != 1 || r.Threads[0].Comments[0].ID != "3" {
		t.Fatalf("nexus read one %+v %v", r, err)
	}
	if _, err := s.Do(t.Context(), Request{Verb: VerbRead, URL: "https://www.nexusmods.com/g/mods/1", Parent: "404"}); code(err) != CodeNotFound {
		t.Fatalf("nexus missing thread: %v", err)
	}
	r, err = s.Do(t.Context(), Request{Verb: VerbRead, URL: "https://mods.factorio.com/mod/m/discussion/65f0c1a2b3c4d5e6f7a8b9c0"})
	if err != nil || r.Count != 1 || fa.read[0] != "m/65f0c1a2b3c4d5e6f7a8b9c0" || fa.syncs != 0 {
		t.Fatalf("factorio read %+v %v %v", r, err, fa.read)
	}
	if _, err := s.Do(t.Context(), Request{Verb: VerbRead, URL: "https://github.com/o/r/issues/8"}); code(err) != CodeNotFound {
		t.Fatalf("github not an issue: %v", err)
	}
}
