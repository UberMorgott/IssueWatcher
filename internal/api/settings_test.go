package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/folders"
)

// cfgStore is api.SettingsStore over a bare config.Store (no side effects).
type cfgStore struct{ s *config.Store }

func (c cfgStore) doc(s config.Settings) SettingsDoc {
	return SettingsDoc{Revision: s.Revision, Settings: s, Info: SettingsInfo{SyncPresets: config.Presets(), Palettes: config.Palettes()}}
}
func (c cfgStore) Settings() (SettingsDoc, error) { return c.doc(c.s.Get()), nil }
func (c cfgStore) PatchSettings(rev int, p json.RawMessage) (SettingsDoc, error) {
	s, err := c.s.Patch(rev, p, nil)
	return c.doc(s), err
}
func (c cfgStore) ResetSettings(rev int, section string) (SettingsDoc, error) {
	s, err := c.s.Reset(rev, section, nil)
	return c.doc(s), err
}

func settingsEnv(t *testing.T) *env {
	t.Helper()
	cs, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return syncedEnv(t, func(o *Options) { o.Settings = cfgStore{cs} })
}

func TestSettingsPatchConflictValidation(t *testing.T) {
	e := settingsEnv(t)
	var doc SettingsDoc
	if code := e.call(t, http.MethodGet, "/api/settings", "", &doc); code != http.StatusOK || doc.Revision != 0 || doc.Settings.General.Language != "ru" {
		t.Fatalf("get: %d %+v", code, doc)
	}

	// Live event for other tabs.
	events := make(chan string, 4)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.s.BaseURL()+"/api/events", nil)
	resp, err := e.browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if name, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
				events <- name
			}
		}
	}()
	for e.s.Clients() == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	if code := e.call(t, http.MethodPatch, "/api/settings", `{"revision":0,"patch":{"general":{"language":"en"}}}`, &doc); code != http.StatusOK ||
		doc.Revision != 1 || doc.Settings.General.Language != "en" {
		t.Fatalf("patch: %d %+v", code, doc)
	}
	select {
	case name := <-events:
		if name != EventSettingsChanged {
			t.Fatalf("event %q", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no settings.changed event")
	}

	// A second tab still on revision 0 gets 409 with the current document.
	var conflict struct {
		Error   string      `json:"error"`
		Current SettingsDoc `json:"current"`
	}
	body := `{"revision":0,"patch":{"general":{"startMinimized":true}}}`
	req2, _ := http.NewRequestWithContext(t.Context(), http.MethodPatch, e.s.BaseURL()+"/api/settings", strings.NewReader(body))
	r2, err := e.browser.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(r2.Body).Decode(&conflict)
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusConflict || conflict.Current.Revision != 1 {
		t.Fatalf("stale revision: %d %+v", r2.StatusCode, conflict)
	}

	// Validation: 400 with field + code, nothing saved.
	req3, _ := http.NewRequestWithContext(t.Context(), http.MethodPatch, e.s.BaseURL()+"/api/settings",
		strings.NewReader(`{"revision":1,"patch":{"sync":{"activeDays":0}}}`))
	r3, err := e.browser.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	var ve config.ValidationError
	_ = json.NewDecoder(r3.Body).Decode(&ve)
	_ = r3.Body.Close()
	if r3.StatusCode != http.StatusBadRequest || ve.Field != "sync.activeDays" || ve.Code != "range" {
		t.Fatalf("invalid: %d %+v", r3.StatusCode, ve)
	}
	if code := e.call(t, http.MethodPost, "/api/settings/reset", `{"revision":1,"section":"general"}`, &doc); code != http.StatusOK ||
		doc.Settings.General.Language != "ru" || doc.Revision != 2 {
		t.Fatalf("reset: %d %+v", code, doc)
	}
}

func TestFoldersMapAndDiscover(t *testing.T) {
	e := settingsEnv(t)
	var rows []FolderRow
	if code := e.call(t, http.MethodGet, "/api/folders", "", &rows); code != http.StatusOK || len(rows) != 1 || rows[0].Status != folders.StatusNone {
		t.Fatalf("folders: %d %+v", code, rows)
	}
	id, url := rows[0].ProjectID, rows[0].URL

	root := t.TempDir()
	clone := filepath.Join(root, "work", "app")
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	cfg := "[remote \"origin\"]\n\turl = " + url + ".git\n"
	if err := os.WriteFile(filepath.Join(clone, ".git", "config"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var doc SettingsDoc
	rootsJSON, _ := json.Marshal(map[string]any{"revision": 0, "patch": map[string]any{"projects": map[string]any{"roots": []string{root}}}})
	if code := e.call(t, http.MethodPatch, "/api/settings", string(rootsJSON), &doc); code != http.StatusOK {
		t.Fatalf("roots: %d", code)
	}
	var found struct {
		Suggestions []folders.Suggestion `json:"suggestions"`
	}
	if code := e.call(t, http.MethodPost, "/api/folders/discover", "", &found); code != http.StatusOK ||
		len(found.Suggestions) != 1 || found.Suggestions[0].Path != clone || found.Suggestions[0].ProjectID != id {
		t.Fatalf("discover: %d %+v", code, found)
	}
	pathJSON, _ := json.Marshal(map[string]string{"path": clone})
	var row FolderRow
	if code := e.call(t, http.MethodPut, "/api/projects/"+jsonInt(id)+"/path", string(pathJSON), &row); code != http.StatusOK ||
		row.Status != folders.StatusOK || row.LocalPath != clone {
		t.Fatalf("map: %d %+v", code, row)
	}
	// Any existing folder is mapped; the status warns that it is not a clone.
	notClone, _ := json.Marshal(map[string]string{"path": root})
	if code := e.call(t, http.MethodPut, "/api/projects/"+jsonInt(id)+"/path", string(notClone), &row); code != http.StatusOK ||
		row.Status != folders.StatusNotGit || row.LocalPath != root {
		t.Fatalf("not a clone: %d %+v", code, row)
	}
	// A missing folder is refused; the mapping stays.
	missing, _ := json.Marshal(map[string]string{"path": filepath.Join(root, "gone")})
	var refused struct{ Error, Code string }
	if code := e.callAny(t, http.MethodPut, "/api/projects/"+jsonInt(id)+"/path", string(missing), &refused); code != http.StatusUnprocessableEntity ||
		refused.Code != string(folders.StatusMissing) {
		t.Fatalf("missing: %d %+v", code, refused)
	}
	if e.call(t, http.MethodGet, "/api/folders", "", &rows); rows[0].LocalPath != root || rows[0].Status != folders.StatusNotGit {
		t.Fatalf("refused path saved: %+v", rows)
	}
	if code := e.call(t, http.MethodPut, "/api/projects/"+jsonInt(id)+"/path", `{"path":""}`, &row); code != http.StatusOK || row.Status != folders.StatusNone || row.LocalPath != "" {
		t.Fatalf("unmap: %d %+v", code, row)
	}
	if code := e.call(t, http.MethodPut, "/api/projects/"+jsonInt(id)+"/path", `{"path":"relative\\dir"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("relative path: %d", code)
	}
	if code := e.call(t, http.MethodPut, "/api/projects/99999/path", `{"path":""}`, nil); code != http.StatusNotFound {
		t.Fatalf("unknown project: %d", code)
	}
}

// callAny is call that decodes the body whatever the status (error answers too).
func (e *env) callAny(t *testing.T, method, path, body string, out any) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, e.s.BaseURL()+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("%s %s: decode: %v", method, path, err)
	}
	return resp.StatusCode
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
