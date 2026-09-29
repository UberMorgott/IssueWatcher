package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/runner"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

func TestProjectLinksAPI(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	gh, _ := e.store.UpsertSource(ctx, "github", "me")
	code, err := e.store.SyncProjects(ctx, gh, []provider.Project{{ExternalID: "o/app", Name: "o/app"}, {ExternalID: "o/lib", Name: "o/lib"}})
	if err != nil {
		t.Fatal(err)
	}
	nx, _ := e.store.UpsertSource(ctx, "nexus", "me")
	mods, err := e.store.SyncProjects(ctx, nx, []provider.Project{{ExternalID: "skyrim/1", Name: "Mod One"}, {ExternalID: "skyrim/2", Name: "Mod Two"}})
	if err != nil {
		t.Fatal(err)
	}
	path := func(id int64) string { return "/api/projects/" + strconv.FormatInt(id, 10) + "/links" }
	put := func(code int64, mods ...int64) int {
		b, _ := json.Marshal(map[string]any{"mods": mods})
		return e.call(t, http.MethodPut, path(code), string(b), nil)
	}

	var l store.ProjectLinks
	if c := e.call(t, http.MethodGet, path(code[0].ID), "", &l); c != http.StatusOK || l.LinkedTo != nil || len(l.Links) != 0 {
		t.Fatalf("empty links %d %+v", c, l)
	}
	if c := put(code[0].ID, mods[0].ID, mods[1].ID); c != http.StatusOK {
		t.Fatalf("link %d", c)
	}
	for _, bad := range [][2]int64{{mods[0].ID, mods[1].ID}, {code[0].ID, code[1].ID}} { // mod → mod, code → code
		if c := put(bad[0], bad[1]); c != http.StatusBadRequest {
			t.Fatalf("bad link %v: %d", bad, c)
		}
	}
	if c := put(9999, mods[0].ID); c != http.StatusNotFound {
		t.Fatalf("link of no project: %d", c)
	}
	// One code project per mod page: Mod One moves to o/lib.
	if c := put(code[1].ID, mods[0].ID); c != http.StatusOK {
		t.Fatalf("move %d", c)
	}
	var projects []store.Repo
	if c := e.call(t, http.MethodGet, "/api/projects", "", &projects); c != http.StatusOK {
		t.Fatalf("projects %d", c)
	}
	got := map[int64]string{}
	for _, p := range projects {
		got[p.ID] = fmt.Sprint(p.LinkedTo, p.Links)
	}
	if got[code[0].ID] != fmt.Sprint(0, []int64{mods[1].ID}) || got[code[1].ID] != fmt.Sprint(0, []int64{mods[0].ID}) ||
		got[mods[0].ID] != fmt.Sprint(code[1].ID, []int64(nil)) {
		t.Fatalf("project rows %v", got)
	}
	if c := e.call(t, http.MethodGet, path(mods[1].ID), "", &l); c != http.StatusOK || l.LinkedTo == nil || l.LinkedTo.Name != "o/app" {
		t.Fatalf("mod links %d %+v", c, l)
	}
	if c := e.call(t, http.MethodDelete, path(mods[1].ID), "", nil); c != http.StatusNoContent {
		t.Fatalf("unlink %d", c)
	}
	if c := e.call(t, http.MethodGet, path(code[0].ID), "", &l); c != http.StatusOK || len(l.Links) != 0 {
		t.Fatalf("after unlink %d %+v", c, l)
	}
	if c := e.call(t, http.MethodDelete, path(9999), "", nil); c != http.StatusNotFound {
		t.Fatalf("unlink no project %d", c)
	}
}

// A mod-page fix's push / PR while agents.modPush is off → 409 code mod_item.
func TestModItemPushConflict(t *testing.T) {
	e := newEnv(t)
	w := httptest.NewRecorder()
	if !e.s.jobError(w, fmt.Errorf("push: %w", runner.ErrModItem)) {
		t.Fatal("not handled")
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusConflict || body.Code != runner.CodeModItem || body.Error == "" {
		t.Fatalf("%d %s %v", w.Code, w.Body, err)
	}
}
