package nexus

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/browser"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/signin"
	"github.com/UberMorgott/issuewatcher/internal/websession"
)

// editorSite is a fake www for the mod editor: the settings GET and the save POST.
type editorSite struct {
	mu       sync.Mutex
	settings string
	save     func(req browser.Request) browser.Response
	reqs     []browser.Request
}

func (f *editorSite) Fetch(_ context.Context, req browser.Request) (browser.Response, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	switch {
	case strings.HasPrefix(req.URL, "https://www.nexusmods.com/api/flamework/mods/settings?") && req.Method == "":
		return browser.Response{Status: 200, ContentType: "application/json", Body: f.settings}, nil
	case req.URL == "https://www.nexusmods.com/api/flamework/mods/save" && req.Method == http.MethodPost:
		return f.save(req), nil
	}
	return browser.Response{Status: 404, Body: "unexpected " + req.Method + " " + req.URL}, nil
}

func (f *editorSite) posts() []browser.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []browser.Request
	for _, r := range f.reqs {
		if r.Method == http.MethodPost {
			out = append(out, r)
		}
	}
	return out
}

// editorMod202 is the api-router Mod answer of wartales/202 (escaped BBCode as the site stores it).
const editorMod202 = `{"data":{"mod":{"author":"UberMorgott","description":"[b]Hi[/b]&lt;br /&gt;line2 &amp; more<br />\nthird&nbsp;x &#39;q&#39;",` +
	`"game":{"domainName":"wartales","id":"5108","name":"Wartales","supportsVortex":true},"gameId":5108,"legacyModRequirementsEnabled":false,"mirrors":[],` +
	`"modCategory":{"categoryId":"5","name":"Gameplay"},"modId":202,"name":"Wartales MP","summary":"Short &amp; sweet<br />second",` +
	`"tags":[{"id":101,"name":"Multiplayer"},{"id":"7","name":"Co-op"}],"uid":"868632436938","uploader":{"avatar":"","memberId":1234,"name":"UberMorgott"},"version":"0.2.0"}}}`

func newEditorNative(t *testing.T, settings string, save func(browser.Request) browser.Response) (*Provider, *editorSite, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var queries []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			OperationName string         `json:"operationName"`
			Query         string         `json:"query"`
			Variables     map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		switch {
		case strings.Contains(q.Query, "game(domainName"):
			_, _ = w.Write([]byte(`{"data":{"game":{"id":5108}}}`))
		case q.OperationName == "Mod" && r.URL.Path == "/graphql" && r.Header.Get("X-GraphQL-OperationName") == "Mod":
			mu.Lock()
			queries = append(queries, q.Query)
			mu.Unlock()
			if q.Variables["gameId"] != "5108" || q.Variables["modId"] != "202" {
				http.Error(w, "bad variables", 400)
				return
			}
			_, _ = w.Write([]byte(editorMod202))
		default:
			http.Error(w, "unknown query", 400)
		}
	}))
	t.Cleanup(srv.Close)
	hc := srv.Client()
	if tr, ok := hc.Transport.(*http.Transport); ok {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // G402: httptest certificate
	}
	jar, _ := websession.Open(filepath.Join(t.TempDir(), "nexus.json"))
	_ = jar.Replace([]websession.Cookie{{Name: "nexusmods_session", Value: "x", Domain: ".nexusmods.com", Path: "/"}}, "UA", "UberMorgott", signin.SourceWindow)
	es := &editorSite{settings: settings, save: save}
	p := New(Options{Native: &NativeOptions{Browser: es, HTTP: hc, GraphQL: srv.URL + "/v2/graphql", APIRouter: srv.URL + "/graphql",
		Session: signin.New(signin.Spec{Platform: Platform}, jar, nil)}})
	return p, es, &queries
}

const editableSettings = `{"permissions":{"canEdit":true},"translation":{"type":1,"language":0,"translationOf":0}}`

func saveOK(browser.Request) browser.Response {
	return browser.Response{Status: 200, ContentType: "application/json", Body: `{"success":true,"modId":202}`}
}

var project202 = provider.Project{ExternalID: "wartales/202", Name: "Wartales MP"}

func TestDecodeBBCode(t *testing.T) {
	for in, want := range map[string]string{
		"":                               "",
		"a&lt;br /&gt;b":                 "a\nb",
		"a  <br />\n  b":                 "a\nb",
		"a<BR>b&nbsp;c":                  "a\nb c",
		"&amp;&lt;&gt;&quot;&#39;&#x41;": `&<>"'A`,
		"&unknownentity; &amp":           "&unknownentity; &amp",
		"[url=https://x.y/?a=1&amp;b=2]": "[url=https://x.y/?a=1&b=2]",
	} {
		if got := decodeBBCode(in); got != want {
			t.Errorf("decodeBBCode(%q) = %q, want %q", in, got, want)
		}
	}
}

// The read is the editor's: the verbatim Mod query with the session, the
// settings GET in the browser, BBCode decoded as decodeBBCode does.
func TestModPageRead(t *testing.T) {
	p, es, queries := newEditorNative(t, editableSettings, saveOK)
	if !p.Capabilities().EditPage {
		t.Fatal("EditPage capability off")
	}
	pg, err := p.ModPage(context.Background(), project202)
	if err != nil {
		t.Fatal(err)
	}
	want := provider.ModPage{Name: "Wartales MP", Summary: "Short & sweet\nsecond", Description: "[b]Hi[/b]\nline2 & more\nthird x 'q'",
		Version: "0.2.0", Category: "Gameplay", Author: "UberMorgott", Tags: []string{"Multiplayer", "Co-op"},
		URL: "https://www.nexusmods.com/wartales/mods/202", MaxSummary: 350}
	if got, _ := json.Marshal(pg); string(got) != string(must(json.Marshal(want))) {
		t.Fatalf("page = %s", got)
	}
	if len(*queries) != 1 || (*queries)[0] != modEditQuery {
		t.Fatalf("Mod query not the editor's verbatim query: %q", *queries)
	}
	if len(es.reqs) != 1 || es.reqs[0].URL != "https://www.nexusmods.com/api/flamework/mods/settings?gameId=5108&modId=202" ||
		es.reqs[0].Page != "https://www.nexusmods.com/wartales/mods/202" {
		t.Fatalf("settings request = %+v", es.reqs)
	}
}

// rawBody is the planned save body.
func rawBody(res provider.ModPageSave) string {
	b, _ := res.Request.Body.(json.RawMessage)
	return string(b)
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// The exact save payload of web-mod.ts:578-593: edited fields replaced,
// everything else resent as loaded; sent once, as JSON, in the browser.
func TestModPageSaveExactPayload(t *testing.T) {
	p, es, _ := newEditorNative(t, editableSettings, saveOK)
	edit := provider.ModPageEdit{Summary: new("New summary\nline two"), Version: new("0.3.0"), Name: new("Wartales MP")}
	res, err := p.SaveModPage(context.Background(), project202, edit)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"modId":202,"gameId":5108,"name":"Wartales MP","summary":"New summary<br />line two",` +
		`"description":"[b]Hi[/b]\nline2 & more\nthird x 'q'","categoryId":5,"author":"UberMorgott","version":"0.3.0",` +
		`"type":"1","languageId":0,"tags":[{"id":"101","selected":true},{"id":"7","selected":true}],"classtags":["101","7"],"saveAllTags":true}`
	posts := es.posts()
	if len(posts) != 1 {
		t.Fatalf("posts = %d", len(posts))
	}
	got := posts[0]
	if got.Body != want {
		t.Fatalf("save body\n got %s\nwant %s", got.Body, want)
	}
	if got.Headers["Content-Type"] != "application/json" || len(got.Headers) != 1 || got.Page != "https://www.nexusmods.com/wartales/mods/202" {
		t.Fatalf("save request = %+v", got)
	}
	if !res.Saved || res.DryRun || strings.Join(res.Changed, ",") != "summary,version" ||
		res.Request.Method != "POST" || res.Request.URL != "https://www.nexusmods.com/api/flamework/mods/save" ||
		rawBody(res) != want {
		t.Fatalf("result = %+v", res)
	}
}

// A dry run builds the same body and sends nothing; an identical save changes nothing.
func TestModPageSaveDryRunSendsNothing(t *testing.T) {
	p, es, _ := newEditorNative(t, editableSettings, func(browser.Request) browser.Response {
		t.Error("save POST sent in a dry run")
		return browser.Response{Status: 500}
	})
	res, err := p.SaveModPage(context.Background(), project202, provider.ModPageEdit{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(es.posts()) != 0 || !res.DryRun || res.Saved || len(res.Changed) != 0 {
		t.Fatalf("dry run = %+v, posts %d", res, len(es.posts()))
	}
	body := rawBody(res)
	if !strings.Contains(body, `"summary":"Short & sweet<br />second"`) || !strings.Contains(body, `"version":"0.2.0"`) {
		t.Fatalf("identical body = %s", body)
	}
}

// A translation resends type "2", its language and originalModId.
func TestModPageSaveTranslation(t *testing.T) {
	p, es, _ := newEditorNative(t, `{"permissions":{"canEdit":true},"translation":{"type":2,"language":"7","translationOf":150}}`, saveOK)
	if _, err := p.SaveModPage(context.Background(), project202, provider.ModPageEdit{}); err != nil {
		t.Fatal(err)
	}
	body := es.posts()[0].Body
	if !strings.Contains(body, `"type":"2","languageId":7,"originalModId":150,"tags":`) {
		t.Fatalf("translation body = %s", body)
	}
}

func TestModPageRefusals(t *testing.T) {
	p, _, _ := newEditorNative(t, `{"permissions":{"canEdit":false}}`, saveOK)
	if _, err := p.ModPage(context.Background(), project202); !errors.Is(err, provider.ErrCannotEdit) {
		t.Fatalf("canEdit false: %v", err)
	}
	p, es, _ := newEditorNative(t, editableSettings, func(browser.Request) browser.Response {
		return browser.Response{Status: 200, Body: `{"success":false,"message":"nope"}`}
	})
	if _, err := p.SaveModPage(context.Background(), project202, provider.ModPageEdit{Version: new("0.3.0")}); err == nil || errors.Is(err, errWriteUnsure) {
		t.Fatalf("success=false: %v", err)
	}
	p, _, _ = newEditorNative(t, editableSettings, func(browser.Request) browser.Response { return browser.Response{Status: 502, Body: "gateway"} })
	if _, err := p.SaveModPage(context.Background(), project202, provider.ModPageEdit{Version: new("0.3.0")}); !errors.Is(err, errWriteUnsure) {
		t.Fatalf("502: %v", err)
	}
	for _, e := range []provider.ModPageEdit{
		{Name: new(" ")}, {Summary: new(strings.Repeat("x", 351))}, {Description: new("")}, {Version: new("1.0 beta")},
	} {
		if _, err := p.SaveModPage(context.Background(), project202, e); !errors.Is(err, provider.ErrBadPageEdit) {
			t.Fatalf("edit %+v: %v", e, err)
		}
	}
	if n := len(es.posts()); n != 1 {
		t.Fatalf("posts = %d, want the one refused save", n)
	}
}
