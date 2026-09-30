package nexus

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/browser"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/signin"
	"github.com/UberMorgott/issuewatcher/internal/websession"
)

type postTile struct{ id, author, body string }

func li(id, author, body string, kids string) string {
	return fmt.Sprintf(`<li class="comment" id="comment-%s"><div class="comment-head"><span class="comment-name"><a href="#">%s</a></span></div>`+
		`<div class="comment-content"><time data-date="%d"></time><div class="comment-content-text">%s</div></div>%s</li>`,
		id, author, time.Now().Unix(), body, kids)
}

func commentsHTML(replies []postTile) string {
	var kids strings.Builder
	for _, r := range replies {
		kids.WriteString(li(r.id, r.author, r.body, ""))
	}
	return `<input id="current-page-number" value="1"><span id="comment-count" data-comment-count="1"></span><div data-csrf-token="tok123"></div>` +
		`<ol>` + li("500", "alice", "Crash on load", `<ol class="comment-kids">`+kids.String()+`</ol>`) + `</ol>`
}

func bugHTML(replies []postTile) string {
	var b strings.Builder
	b.WriteString(`<ul><li class="comment" id="bug-issue-tile-900"><div class="comment-head"><span class="comment-name"><a>alice</a></span></div><div class="comment-content"><time datetime="2026-09-01 10:00"></time>Ship UI missing</div></li>`)
	for _, r := range replies {
		fmt.Fprintf(&b, `<li class="comment" id="bug-reply-tile-%s"><div class="comment-head"><span class="comment-name"><a>%s</a></span></div><div class="comment-content"><time datetime="2026-09-01 11:00"></time>%s</div></li>`, r.id, r.author, r.body)
	}
	b.WriteString(`</ul><a class="add-bug-reply" data-csrf-token="bugtok"></a>`)
	return b.String()
}

// replySite is a fake www with one Posts thread (500) and one bug (900).
type replySite struct {
	*fakeSite
	mu      sync.Mutex
	replies []postTile
	bugReps []postTile
	posts   []browser.Request
}

func newReplyNative(t *testing.T, answer func(req browser.Request, save func()) (browser.Response, error)) (*Provider, *replySite) {
	t.Helper()
	rs := &replySite{fakeSite: &fakeSite{t: t, comments: map[int]string{}, bugPosts: map[string]string{}}}
	refresh := func() {
		rs.fakeSite.mu.Lock()
		rs.comments[1] = commentsHTML(rs.replies)
		rs.bugPosts["900"] = bugHTML(rs.bugReps)
		rs.fakeSite.mu.Unlock()
	}
	refresh()
	rs.onPost = func(req browser.Request) (browser.Response, error) {
		rs.mu.Lock()
		rs.posts = append(rs.posts, req)
		rs.mu.Unlock()
		save := func() {
			form, _ := url.ParseQuery(req.Body)
			rs.mu.Lock()
			if strings.HasPrefix(req.URL, "https://www.nexusmods.com/mod_bug/") {
				rs.bugReps = append(rs.bugReps, postTile{fmt.Sprint(7000 + len(rs.bugReps)), "UberMorgott", form.Get("content")})
			} else {
				text, _ := url.PathUnescape(form.Get("post"))
				rs.replies = append(rs.replies, postTile{fmt.Sprint(600 + len(rs.replies)), "UberMorgott", text})
			}
			rs.mu.Unlock()
			refresh()
		}
		return answer(req, save)
	}
	srv, hc := gqlServer(t, nil)
	jar, _ := websession.Open(filepath.Join(t.TempDir(), "nexus.json"))
	_ = jar.Replace([]websession.Cookie{{Name: "nexusmods_session", Value: "x", Domain: ".nexusmods.com", Path: "/"}}, "UA", "UberMorgott", signin.SourceWindow)
	mgr := signin.New(signin.Spec{Platform: Platform}, jar, nil)
	p := New(Options{Native: &NativeOptions{Browser: rs, HTTP: hc, GraphQL: srv.URL + "/v2/graphql", APIRouter: srv.URL + "/graphql", Session: mgr},
		Author: func() string { return "UberMorgott" }, ReadBackWaits: []time.Duration{time.Millisecond, time.Millisecond}})
	return p, rs
}

func ok1(save func()) (browser.Response, error) {
	save()
	return browser.Response{Status: 200, Body: "1"}, nil
}

func TestNativeReplyCommentExactForm(t *testing.T) {
	p, rs := newReplyNative(t, func(_ browser.Request, save func()) (browser.Response, error) { return ok1(save) })
	body := "Fixed in 1.2 — thanks & see #3"
	c, err := p.Reply(context.Background(), "comment:windrose/147/500", body)
	if err != nil || c.ExternalID != "600" || c.Author != "UberMorgott" {
		t.Fatalf("reply = %+v, %v", c, err)
	}
	if len(rs.posts) != 1 {
		t.Fatalf("posts = %d", len(rs.posts))
	}
	req := rs.posts[0]
	form, _ := url.ParseQuery(req.Body)
	want := map[string]string{"game_id": "8851", "object_id": "147", "thread_id": "16796543", "use_emo": "0", "parent_id": "500",
		"_token": "tok123", "post": encodeURIComponent(body)}
	for k, v := range want {
		if form.Get(k) != v {
			t.Fatalf("form %s = %q, want %q (all %v)", k, form.Get(k), v, form)
		}
	}
	if req.URL != "https://www.nexusmods.com/mod/comment" || req.Headers["X-Requested-With"] != "XMLHttpRequest" ||
		!strings.HasPrefix(req.Headers["Content-Type"], "application/x-www-form-urlencoded") {
		t.Fatalf("request = %+v", req)
	}
	if encodeURIComponent("a b&c/é") != "a%20b%26c%2F%C3%A9" {
		t.Fatal(encodeURIComponent("a b&c/é"))
	}
}

func TestNativeReply500ButSaved(t *testing.T) {
	p, rs := newReplyNative(t, func(_ browser.Request, save func()) (browser.Response, error) {
		save()
		return browser.Response{Status: 500, Body: "Whoops"}, nil
	})
	c, err := p.Reply(context.Background(), "comment:windrose/147/500", "hello")
	if err != nil || c.ExternalID != "600" || len(rs.posts) != 1 {
		t.Fatalf("reply = %+v, %v, posts %d", c, err, len(rs.posts))
	}
}

func TestNativeReplyLostAnswer(t *testing.T) {
	for _, saved := range []bool{true, false} {
		p, rs := newReplyNative(t, func(_ browser.Request, save func()) (browser.Response, error) {
			if saved {
				save()
			}
			return browser.Response{}, context.DeadlineExceeded
		})
		c, err := p.Reply(context.Background(), "comment:windrose/147/500", "hello")
		switch {
		case saved && (err != nil || c.ExternalID != "600"):
			t.Fatalf("saved: %+v, %v", c, err)
		case !saved && !errors.Is(err, ErrUnknownOutcome):
			t.Fatalf("lost: err = %v", err)
		}
		if len(rs.posts) != 1 {
			t.Fatalf("posts = %d (a write is sent once)", len(rs.posts))
		}
	}
}

func TestNativeReplyChallengeNoRepost(t *testing.T) {
	p, rs := newReplyNative(t, func(browser.Request, func()) (browser.Response, error) {
		return browser.Response{Status: 403}, browser.ErrChallenge
	})
	if _, err := p.Reply(context.Background(), "comment:windrose/147/500", "hello"); !errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("err = %v", err)
	}
	if len(rs.posts) != 1 {
		t.Fatalf("posts = %d (never re-posted after a challenge)", len(rs.posts))
	}
}

func TestNativeReplyRefused(t *testing.T) {
	p, rs := newReplyNative(t, func(browser.Request, func()) (browser.Response, error) {
		return browser.Response{Status: 403, Body: "Forbidden"}, nil
	})
	if _, err := p.Reply(context.Background(), "comment:windrose/147/500", "hello"); err == nil || errors.Is(err, ErrUnknownOutcome) {
		t.Fatalf("err = %v (a 4xx is a refusal)", err)
	}
	if len(rs.posts) != 1 {
		t.Fatalf("posts = %d", len(rs.posts))
	}
}

func TestNativeReplyBug(t *testing.T) {
	p, rs := newReplyNative(t, func(req browser.Request, save func()) (browser.Response, error) {
		save()
		return browser.Response{Status: 500, Body: "tile render failed"}, nil
	})
	c, err := p.Reply(context.Background(), "bug:900", "Can you attach the log?")
	if err != nil || c.ExternalID != "7000" {
		t.Fatalf("bug reply = %+v, %v", c, err)
	}
	form, _ := url.ParseQuery(rs.posts[0].Body)
	if rs.posts[0].URL != "https://www.nexusmods.com/mod_bug/900/reply" || form.Get("content") != "Can you attach the log?" || form.Get("_token") != "bugtok" {
		t.Fatalf("bug post = %+v", rs.posts[0])
	}
}

func TestNativeReplyNeedsSession(t *testing.T) {
	fs := &fakeSite{t: t, comments: map[int]string{1: commentsHTML(nil)}}
	posted := 0
	fs.onPost = func(browser.Request) (browser.Response, error) { posted++; return browser.Response{Status: 200, Body: "1"}, nil }
	srv, hc := gqlServer(t, nil)
	p := New(Options{Native: &NativeOptions{Browser: fs, HTTP: hc, GraphQL: srv.URL + "/v2/graphql", APIRouter: srv.URL + "/graphql"},
		Author: func() string { return "UberMorgott" }})
	if _, err := p.Reply(context.Background(), "comment:windrose/147/500", "x"); !errors.Is(err, provider.ErrNotSignedIn) || posted != 0 {
		t.Fatalf("err = %v, posted %d", err, posted)
	}
}
