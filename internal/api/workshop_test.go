package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/provider/steamugc"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

type fakeWorkshop struct {
	item    uint64
	creates int
	pages   []steamugc.Page
	app     uint32
}

func (f *fakeWorkshop) Item(string) (uint64, bool, error) { return f.item, false, nil }
func (f *fakeWorkshop) Create(_ context.Context, _ string, app uint32, dry bool) (steamugc.Created, error) {
	if dry {
		return steamugc.Created{DryRun: true, Plan: "CreateItem"}, nil
	}
	if f.item != 0 {
		return steamugc.Created{Item: f.item, URL: steamugc.ItemURL(f.item)}, nil
	}
	f.creates++
	f.item = 3800000001
	return steamugc.Created{Item: f.item, URL: steamugc.ItemURL(f.item), Created: true}, nil
}
func (f *fakeWorkshop) ResetCreate(string) error { return nil }
func (f *fakeWorkshop) Status(_ context.Context, app uint32) (steamugc.Status, error) {
	return steamugc.Status{Running: true, LoggedOn: true, SteamID: "76561197996210591", AppID: app}, nil
}
func (f *fakeWorkshop) SetPage(_ context.Context, app uint32, item uint64, p steamugc.Page, _ string, dry bool) (steamugc.PageResult, error) {
	f.app = app
	if !dry {
		f.pages = append(f.pages, p)
	}
	return steamugc.PageResult{DryRun: dry, Item: item}, nil
}

func TestWorkshopCreateAndPage(t *testing.T) {
	ws := &fakeWorkshop{}
	e, id := releaseEnv(t, func(o *Options) { o.Workshop = ws })
	folder := t.TempDir()
	if err := os.MkdirAll(filepath.Join(folder, "workshop", "locale"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"description.english.txt": "[h1]Tool[/h1]", "description.russian.txt": "[h1]Инструмент[/h1]"} {
		if err := os.WriteFile(filepath.Join(folder, "workshop", "locale", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.s.opts.Store.SetLocalPath(t.Context(), id, folder); err != nil {
		t.Fatal(err)
	}
	base := "/api/projects/" + itoa(id) + "/steam"

	var out map[string]any
	if code := e.call(t, http.MethodPost, base+"/item", `{"appId": 839770, "dryRun": true}`, &out); code != http.StatusOK || out["dryRun"] != true || ws.creates != 0 {
		t.Fatalf("dry create %d %v", code, out)
	}
	if code := e.call(t, http.MethodPost, base+"/item", `{"appId": 839770}`, &out); code != http.StatusOK || out["item"] != float64(3800000001) ||
		out["created"] != true || out["target"] != "steam:3800000001" {
		t.Fatalf("create %d %v", code, out)
	}
	cur, _ := e.s.opts.Settings.Settings()
	if tp, ok := cur.Settings.Agents.Projects["github:octo/app"].PublishProfile.Targets["steam:3800000001"]; !ok || tp.AppID != 839770 {
		t.Fatalf("profile target %+v", cur.Settings.Agents.Projects["github:octo/app"].PublishProfile.Targets)
	}
	if code := e.call(t, http.MethodPost, base+"/item", `{"appId": 839770}`, &out); code != http.StatusOK || out["created"] != false || ws.creates != 1 {
		t.Fatalf("second create %d %v", code, out)
	}

	// The page: item and app id from the recorded item and the profile target; title = project name.
	var res steamugc.PageResult
	if code := e.call(t, http.MethodPost, base+"/page", `{"visibility": "public", "tags": ["Tools"]}`, &res); code != http.StatusOK || len(ws.pages) != 1 || ws.app != 839770 {
		t.Fatalf("page %d %+v", code, res)
	}
	p := ws.pages[0]
	if len(p.Texts) != 2 || p.Texts[0].Language != "english" || p.Texts[1].Language != "russian" || p.Texts[0].Title == "" || p.Visibility != 0 || p.Tags[0] != "Tools" {
		t.Fatalf("page sent %+v", p)
	}
	if code, out := e.callErr(t, http.MethodPost, base+"/page", `{"localeDir": "../x"}`); code != http.StatusBadRequest || out != "bad_request" {
		t.Fatalf("bad locale dir %d %q", code, out)
	}

	// Agent runs: dry runs only.
	e.s.inAgentJob = func(uint32) bool { return true }
	for _, c := range []struct{ path, body string }{{base + "/item", `{"appId": 839770}`}, {base + "/page", `{}`}} {
		if code, out := e.callErr(t, http.MethodPost, c.path, c.body); code != http.StatusForbidden || out != codeAgentCaller {
			t.Errorf("%s: %d %q", c.path, code, out)
		}
	}
	if code := e.call(t, http.MethodPost, base+"/page", `{"dryRun": true}`, &res); code != http.StatusOK || !res.DryRun {
		t.Fatalf("agent dry run %d", code)
	}
}

// TestSteamStatus: the app is appId or the project's Steam target; never a
// default app (Steam would show the owner playing it).
func TestSteamStatus(t *testing.T) {
	e, id := releaseEnv(t, func(o *Options) { o.Workshop = &fakeWorkshop{} })
	var out map[string]any
	for _, path := range []string{"/api/steam/status", "/api/steam/status?appId=x", "/api/steam/status?appId=0", "/api/steam/status?project=x",
		"/api/steam/status?project=" + itoa(id)} { // no target yet
		if code, c := e.callErr(t, http.MethodGet, path, ""); code != http.StatusBadRequest || c != "bad_request" {
			t.Fatalf("%s: %d %q", path, code, c)
		}
	}
	if code, _ := e.callErr(t, http.MethodGet, "/api/steam/status?project=999999", ""); code != http.StatusNotFound {
		t.Fatalf("unknown project %d", code)
	}
	if code := e.call(t, http.MethodGet, "/api/steam/status?appId=839770", "", &out); code != http.StatusOK || out["loggedOn"] != true || out["running"] != true ||
		out["steamId"] != "76561197996210591" || out["appId"] != float64(839770) {
		t.Fatalf("status app %d %v", code, out)
	}
	if err := e.s.addSteamTarget("github:octo/app", "steam:3800000001", 839770); err != nil {
		t.Fatal(err)
	}
	if code := e.call(t, http.MethodGet, "/api/steam/status?project="+itoa(id), "", &out); code != http.StatusOK || out["appId"] != float64(839770) {
		t.Fatalf("status project %d %v", code, out)
	}
	if code := e.call(t, http.MethodGet, "/api/steam/status?project="+itoa(id)+"&appId=294100", "", &out); code != http.StatusOK || out["appId"] != float64(294100) {
		t.Fatalf("appId overrides %d %v", code, out)
	}
	// The Steam page project (key steam:<id>) resolves through the profile target too.
	if got := e.s.steamAppID(store.Repo{Key: "steam:3800000001", Platform: "steam"}); got != 839770 {
		t.Fatalf("steam page app %d", got)
	}
	if got := e.s.steamAppID(store.Repo{Key: "steam:1", Platform: "steam"}); got != 0 {
		t.Fatalf("unknown steam page app %d", got)
	}
}
