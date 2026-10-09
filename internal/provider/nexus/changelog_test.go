package nexus

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/browser"
	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// documentation202 is GET mods/documentation of wartales/202: positions out of
// order, a <br /> in an entry, and versions in no particular order.
const documentation202 = `{"readme":null,"changelog":{` +
	`"0.2.4":[{"change":"old b","id":42,"position":2,"version":"0.2.4"},{"change":"old a","id":41,"position":1,"version":"0.2.4"}],` +
	`"0.2.9":[{"change":"Added: x<br />more","id":90,"position":1,"version":"0.2.9"}],` +
	`"0.2.10":[{"change":"y","id":100,"position":1,"version":"0.2.10"}]}}`

// newChangelogNative is the editor with the documentation GET and the changelog POSTs.
func newChangelogNative(t *testing.T, doc string, post func(browser.Request) browser.Response) (*Provider, *editorSite) {
	t.Helper()
	p, es, _ := newEditorNative(t, editableSettings, saveOK)
	es.extra = func(req browser.Request) (browser.Response, bool) {
		switch {
		case req.URL == "https://www.nexusmods.com/api/flamework/mods/documentation?gameId=5108&modId=202" && req.Method == "":
			return browser.Response{Status: 200, ContentType: "application/json", Body: doc}, true
		case strings.HasPrefix(req.URL, "https://www.nexusmods.com/api/flamework/mods/changelogs/") && req.Method == http.MethodPost:
			return post(req), true
		}
		return browser.Response{}, false
	}
	return p, es
}

func postOK(browser.Request) browser.Response {
	return browser.Response{Status: 200, ContentType: "application/json", Body: `{"success":true}`}
}

func TestChangelogsRead(t *testing.T) {
	p, es := newChangelogNative(t, documentation202, postOK)
	got, err := p.Changelogs(context.Background(), project202)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"version":"0.2.10","entries":[{"id":100,"text":"y"}]},{"version":"0.2.9","entries":[{"id":90,"text":"Added: x\nmore"}]},` +
		`{"version":"0.2.4","entries":[{"id":41,"text":"old a"},{"id":42,"text":"old b"}]}]`
	if b, _ := json.Marshal(got); string(b) != want {
		t.Fatalf("changelogs = %s", b)
	}
	if len(es.posts()) != 0 || es.reqs[0].Page != "https://www.nexusmods.com/wartales/mods/202" {
		t.Fatalf("requests = %+v", es.reqs)
	}
	// No changelog at all is [] in the documentation.
	p, _ = newChangelogNative(t, `{"readme":null,"changelog":[]}`, postOK)
	if got, err := p.Changelogs(context.Background(), project202); err != nil || len(got) != 0 || got == nil {
		t.Fatalf("empty = %#v %v", got, err)
	}
}

// set = the editor's edit with every id of the version (no duplicates), the
// exact body; a missing version is added; equal lines send nothing.
func TestSetChangelogExactRequests(t *testing.T) {
	p, es := newChangelogNative(t, documentation202, postOK)
	res, err := p.SetChangelog(context.Background(), project202, provider.ChangelogEdit{Version: "0.2.4", Text: "- Fixed: one\n\n- Changed: two & <three>\n"})
	if err != nil {
		t.Fatal(err)
	}
	posts := es.posts()
	want := `{"changeIds":[41,42],"changelogText":"Fixed: one\nChanged: two & <three>","gameId":5108,"modId":202,"version":"0.2.4"}`
	if len(posts) != 1 || posts[0].URL != "https://www.nexusmods.com/api/flamework/mods/changelogs/edit" || posts[0].Body != want ||
		posts[0].Headers["Content-Type"] != "application/json" || posts[0].Page != "https://www.nexusmods.com/wartales/mods/202" {
		t.Fatalf("posts = %+v", posts)
	}
	if !res.Saved || !res.Changed || res.Action != provider.ChangelogActionEdit || strings.Join(res.Before, "|") != "old a|old b" ||
		strings.Join(res.After, "|") != "Fixed: one|Changed: two & <three>" || res.Request == nil || stepBody(res.Request) != want {
		t.Fatalf("result = %+v", res)
	}

	res, err = p.SetChangelog(context.Background(), project202, provider.ChangelogEdit{Version: "0.2.3", Lines: []string{"Added: a"}})
	if err != nil {
		t.Fatal(err)
	}
	posts = es.posts()
	if want := `{"changelogText":"Added: a","gameId":5108,"modId":202,"version":"0.2.3"}`; len(posts) != 2 ||
		posts[1].URL != "https://www.nexusmods.com/api/flamework/mods/changelogs/add" || posts[1].Body != want || res.Action != provider.ChangelogActionAdd {
		t.Fatalf("add posts = %+v, %+v", posts, res)
	}

	res, err = p.SetChangelog(context.Background(), project202, provider.ChangelogEdit{Version: "0.2.4", Lines: []string{"old a", "- old b"}})
	if err != nil || res.Changed || res.Action != provider.ChangelogActionNone || res.Request != nil || len(es.posts()) != 2 {
		t.Fatalf("equal set = %+v %v, posts %d", res, err, len(es.posts()))
	}
}

func TestSetChangelogDryRunAndDelete(t *testing.T) {
	p, es := newChangelogNative(t, documentation202, func(browser.Request) browser.Response {
		t.Error("POST sent in a dry run")
		return browser.Response{Status: 500}
	})
	res, err := p.SetChangelog(context.Background(), project202, provider.ChangelogEdit{Version: "0.2.4", Lines: []string{"n"}, DryRun: true})
	if err != nil || !res.DryRun || res.Saved || !res.Changed || res.Request == nil || !strings.HasSuffix(res.Request.URL, "/changelogs/edit") {
		t.Fatalf("dry set = %+v %v", res, err)
	}
	res, err = p.DeleteChangelog(context.Background(), project202, "0.2.4", true)
	if want := `{"changeIds":[41,42],"gameId":5108,"modId":202}`; err != nil || res.Action != provider.ChangelogActionDelete ||
		!strings.HasSuffix(res.Request.URL, "/changelogs/delete-version") || stepBody(res.Request) != want || len(res.After) != 0 {
		t.Fatalf("dry delete = %+v %v", res, err)
	}
	if res, err := p.DeleteChangelog(context.Background(), project202, "0.2.3", false); err != nil || res.Action != provider.ChangelogActionNone || res.Saved {
		t.Fatalf("delete missing = %+v %v", res, err)
	}
	if len(es.posts()) != 0 {
		t.Fatalf("posts = %d", len(es.posts()))
	}
}

func TestChangelogRefusals(t *testing.T) {
	p, es := newChangelogNative(t, documentation202, func(browser.Request) browser.Response { return browser.Response{Status: 502, Body: "gateway"} })
	for _, e := range []provider.ChangelogEdit{{Version: "", Lines: []string{"a"}}, {Version: "1.0 beta", Lines: []string{"a"}},
		{Version: strings.Repeat("1", 51), Lines: []string{"a"}}, {Version: "1.0", Lines: []string{" ", "-"}}} {
		if _, err := p.SetChangelog(context.Background(), project202, e); !errors.Is(err, provider.ErrBadChangelog) {
			t.Fatalf("edit %+v: %v", e, err)
		}
	}
	if _, err := p.SetChangelog(context.Background(), project202, provider.ChangelogEdit{Version: "0.2.4", Lines: []string{"n"}}); !errors.Is(err, errWriteUnsure) {
		t.Fatalf("502: %v", err)
	}
	if n := len(es.posts()); n != 1 {
		t.Fatalf("posts = %d, want the one unsure edit (never re-sent)", n)
	}
	p, _, _ = newEditorNative(t, `{"permissions":{"canEdit":false}}`, saveOK)
	if _, err := p.Changelogs(context.Background(), project202); !errors.Is(err, provider.ErrCannotEdit) {
		t.Fatalf("canEdit false: %v", err)
	}
}

// stepBody is a planned request's raw body.
func stepBody(s *provider.PublishStep) string {
	b, _ := s.Body.(json.RawMessage)
	return string(b)
}
