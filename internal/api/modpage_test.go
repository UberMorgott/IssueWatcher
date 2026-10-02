package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/nexus"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// fakeEditor stands in for a platform's PageEditor (provider tests cover Nexus).
type fakeEditor struct {
	project provider.Project
	edits   []provider.ModPageEdit
	err     error
}

func (f *fakeEditor) ModPage(_ context.Context, p provider.Project) (provider.ModPage, error) {
	f.project = p
	if f.err != nil {
		return provider.ModPage{}, f.err
	}
	return provider.ModPage{Name: "Mod", Summary: "Short", Description: "[b]x[/b]", Version: "1.0", Tags: []string{}, URL: "https://x/mods/1"}, nil
}

func (f *fakeEditor) SaveModPage(_ context.Context, _ provider.Project, e provider.ModPageEdit) (provider.ModPageSave, error) {
	f.edits = append(f.edits, e)
	if f.err != nil {
		return provider.ModPageSave{}, f.err
	}
	return provider.ModPageSave{DryRun: e.DryRun, Changed: []string{"version"}, Saved: !e.DryRun,
		Request: provider.PublishStep{Method: "POST", URL: "https://x/save", Body: map[string]any{"version": *e.Version}}}, nil
}

func pageEnv(t *testing.T, ed provider.PageEditor) (*env, int64) {
	t.Helper()
	e := syncedEnv(t, func(o *Options) {
		o.PageEditors = func(platform string) provider.PageEditor {
			if platform == "github" && ed != nil {
				return ed
			}
			return nil
		}
	})
	var repos []store.Repo
	if code := e.call(t, http.MethodGet, "/api/projects", "", &repos); code != http.StatusOK || len(repos) != 1 {
		t.Fatalf("repos %d %+v", code, repos)
	}
	return e, repos[0].ID
}

func TestModPageEndpoints(t *testing.T) {
	var res apiErr
	e, id := pageEnv(t, nil)
	if code := e.callAny(t, http.MethodGet, fmt.Sprintf("/api/projects/%d/page", id), "", &res); code != http.StatusConflict || res.Code != "unavailable" {
		t.Fatalf("unavailable: %d %+v", code, res)
	}

	fe := &fakeEditor{}
	e, id = pageEnv(t, fe)
	url := fmt.Sprintf("/api/projects/%d/page", id)
	var pg provider.ModPage
	if code := e.call(t, http.MethodGet, url, "", &pg); code != http.StatusOK || pg.Name != "Mod" || pg.Description != "[b]x[/b]" || fe.project.ExternalID == "" {
		t.Fatalf("read: %d %+v %+v", code, pg, fe.project)
	}
	var sv provider.ModPageSave
	if code := e.call(t, http.MethodPut, url, `{"version":"1.1","dryRun":true}`, &sv); code != http.StatusOK || !sv.DryRun || sv.Saved || len(fe.edits) != 1 ||
		*fe.edits[0].Version != "1.1" || fe.edits[0].Name != nil {
		t.Fatalf("dry run: %d %+v %+v", code, sv, fe.edits)
	}
	if code := e.call(t, http.MethodPut, url, `{"version":"1.1"}`, &sv); code != http.StatusOK || !sv.Saved {
		t.Fatalf("save: %d %+v", code, sv)
	}

	for err, want := range map[error]struct {
		status int
		code   string
	}{
		fmt.Errorf("%w: version", provider.ErrBadPageEdit): {http.StatusBadRequest, "bad_request"},
		fmt.Errorf("%w: x", provider.ErrCannotEdit):        {http.StatusForbidden, "cannot_edit"},
		fmt.Errorf("%w: x", provider.ErrNotSignedIn):       {http.StatusConflict, CodeNotSignedIn},
		fmt.Errorf("%w: x", provider.ErrRelogin):           {http.StatusConflict, CodeRelogin},
		fmt.Errorf("%w: HTTP 502", nexus.ErrWriteUnsure):   {http.StatusBadGateway, "save_unsure"},
		fmt.Errorf("boom"): {http.StatusBadGateway, "platform_error"},
	} {
		fe.err = err
		if code := e.callAny(t, http.MethodPut, url, `{"version":"1.1"}`, &res); code != want.status || res.Code != want.code {
			t.Fatalf("%v: %d %+v", err, code, res)
		}
	}
	if code := e.callAny(t, http.MethodPut, "/api/projects/99999/page", `{}`, &res); code != http.StatusNotFound {
		t.Fatalf("unknown project: %d", code)
	}
}
