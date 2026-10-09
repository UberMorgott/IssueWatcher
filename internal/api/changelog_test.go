package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// fakeChangelogEditor is a page editor that also edits changelogs.
type fakeChangelogEditor struct {
	fakeEditor
	sets    []provider.ChangelogEdit
	deletes []string
}

func (f *fakeChangelogEditor) Changelogs(context.Context, provider.Project) ([]provider.ChangelogVersion, error) {
	return []provider.ChangelogVersion{{Version: "0.2.4", Entries: []provider.ChangelogEntry{{ID: 1, Text: "a"}}}}, nil
}

func (f *fakeChangelogEditor) SetChangelog(_ context.Context, _ provider.Project, e provider.ChangelogEdit) (provider.ChangelogSave, error) {
	f.sets = append(f.sets, e)
	if len(e.EditLines()) == 0 {
		return provider.ChangelogSave{}, fmt.Errorf("%w: no lines", provider.ErrBadChangelog)
	}
	return provider.ChangelogSave{DryRun: e.DryRun, Version: e.Version, Action: provider.ChangelogActionEdit, Changed: true, Saved: !e.DryRun}, nil
}

func (f *fakeChangelogEditor) DeleteChangelog(_ context.Context, _ provider.Project, v string, dry bool) (provider.ChangelogSave, error) {
	f.deletes = append(f.deletes, fmt.Sprintf("%s:%v", v, dry))
	return provider.ChangelogSave{DryRun: dry, Version: v, Action: provider.ChangelogActionDelete, Changed: true, Saved: !dry}, nil
}

func TestChangelogEndpoints(t *testing.T) {
	var res apiErr
	e, id := pageEnv(t, &fakeEditor{})
	base := fmt.Sprintf("/api/projects/%d/changelogs", id)
	if code := e.callAny(t, http.MethodGet, base, "", &res); code != http.StatusConflict || res.Code != "unavailable" {
		t.Fatalf("page editor without changelogs: %d %+v", code, res)
	}

	fe := &fakeChangelogEditor{}
	e, id = pageEnv(t, fe)
	base = fmt.Sprintf("/api/projects/%d/changelogs", id)
	var list []provider.ChangelogVersion
	if code := e.call(t, http.MethodGet, base, "", &list); code != http.StatusOK || len(list) != 1 || list[0].Entries[0].Text != "a" {
		t.Fatalf("list: %d %+v", code, list)
	}
	var sv provider.ChangelogSave
	if code := e.call(t, http.MethodPut, base+"/0.2.4", `{"lines":["x"],"dryRun":true,"version":"ignored"}`, &sv); code != http.StatusOK ||
		!sv.DryRun || sv.Version != "0.2.4" || len(fe.sets) != 1 || fe.sets[0].Version != "0.2.4" {
		t.Fatalf("set: %d %+v %+v", code, sv, fe.sets)
	}
	if code := e.callAny(t, http.MethodPut, base+"/0.2.4", `{"lines":[]}`, &res); code != http.StatusBadRequest || res.Code != "bad_request" {
		t.Fatalf("empty set: %d %+v", code, res)
	}
	if code := e.call(t, http.MethodDelete, base+"/0.2.4?dryRun=1", "", &sv); code != http.StatusOK || !sv.DryRun {
		t.Fatalf("delete dry: %d %+v", code, sv)
	}
	if code := e.call(t, http.MethodDelete, base+"/0.2.3", "", &sv); code != http.StatusOK || !sv.Saved {
		t.Fatalf("delete: %d %+v", code, sv)
	}
	if got := strings.Join(fe.deletes, ","); got != "0.2.4:true,0.2.3:false" {
		t.Fatalf("deletes %s", got)
	}
}
