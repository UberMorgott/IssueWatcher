package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/config"
)

// fakePicker stands in for the desktop dialog.
type fakePicker struct {
	mu             sync.Mutex
	title, initial string
	path           string
	ok             bool
	block          chan struct{} // non-nil: PickFolder waits for it
	entered        chan struct{}
}

func (f *fakePicker) PickFolder(_ context.Context, title, initial string) (string, bool, error) {
	f.mu.Lock()
	f.title, f.initial = title, initial
	path, ok, block, entered := f.path, f.ok, f.block, f.entered
	f.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if block != nil {
		<-block
	}
	return path, ok, nil
}

type pickResult struct {
	Path      string `json:"path"`
	Cancelled bool   `json:"cancelled"`
	Code      string `json:"code"`
}

func pickerEnv(t *testing.T, p FolderPicker) *env {
	t.Helper()
	cs, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return syncedEnv(t, func(o *Options) {
		o.Settings = cfgStore{cs}
		if p != nil {
			o.Picker = p
		}
	})
}

// fakeFilePicker also picks files.
type fakeFilePicker struct {
	fakePicker
}

func (f *fakeFilePicker) PickFile(ctx context.Context, title, initial string) (string, bool, error) {
	return f.PickFolder(ctx, title, initial)
}

// POST /api/dialog/file returns the full path; a file as initial opens its
// folder; a folder-only picker answers unavailable.
func TestFileDialogPicksArchive(t *testing.T) {
	var res pickResult
	if code := pickerEnv(t, &fakePicker{}).callAny(t, http.MethodPost, "/api/dialog/file", `{}`, &res); code != http.StatusConflict || res.Code != "unavailable" {
		t.Fatalf("folder-only picker: %d %+v", code, res)
	}
	fp := &fakeFilePicker{fakePicker{path: `D:\builds\wartales-mp-0.3.0.zip`, ok: true}}
	e := pickerEnv(t, fp)
	dir := t.TempDir()
	body, _ := json.Marshal(map[string]any{"title": "Archive", "initial": dir + `\old.zip`})
	if code := e.call(t, http.MethodPost, "/api/dialog/file", string(body), &res); code != http.StatusOK ||
		res.Path != `D:\builds\wartales-mp-0.3.0.zip` || res.Cancelled || fp.initial != dir || fp.title != "Archive" {
		t.Fatalf("pick: %d %+v initial %q", code, res, fp.initial)
	}
}

func TestFolderDialogUnavailableHeadless(t *testing.T) {
	e := pickerEnv(t, nil)
	var info struct{ Available bool }
	if code := e.call(t, http.MethodGet, "/api/dialog/folder", "", &info); code != http.StatusOK || info.Available {
		t.Fatalf("info: %d %+v", code, info)
	}
	var res pickResult
	if code := e.callAny(t, http.MethodPost, "/api/dialog/folder", `{}`, &res); code != http.StatusConflict || res.Code != "unavailable" {
		t.Fatalf("pick: %d %+v", code, res)
	}
}

func TestFolderDialogPicksFromProjectFolder(t *testing.T) {
	fp := &fakePicker{path: `D:\work\app`, ok: true}
	e := pickerEnv(t, fp)
	var info struct{ Available bool }
	if code := e.call(t, http.MethodGet, "/api/dialog/folder", "", &info); code != http.StatusOK || !info.Available {
		t.Fatalf("info: %d %+v", code, info)
	}
	var rows []FolderRow
	if code := e.call(t, http.MethodGet, "/api/folders", "", &rows); code != http.StatusOK || len(rows) != 1 {
		t.Fatalf("folders: %d %+v", code, rows)
	}
	id := rows[0].ProjectID
	mapped := t.TempDir()
	if err := e.store.SetLocalPath(t.Context(), id, mapped); err != nil {
		t.Fatal(err)
	}

	// Starts in the project's mapped folder; the picked path comes back unsaved.
	var res pickResult
	body, _ := json.Marshal(map[string]any{"projectId": id, "title": "Folder · app"})
	if code := e.call(t, http.MethodPost, "/api/dialog/folder", string(body), &res); code != http.StatusOK ||
		res.Path != `D:\work\app` || res.Cancelled || fp.initial != mapped || fp.title != "Folder · app" {
		t.Fatalf("pick: %d %+v initial=%q title=%q", code, res, fp.initial, fp.title)
	}
	// A typed existing path wins; a cancel returns no path.
	typed := t.TempDir()
	fp.mu.Lock()
	fp.ok = false
	fp.mu.Unlock()
	body, _ = json.Marshal(map[string]any{"projectId": id, "initial": typed})
	res = pickResult{}
	if code := e.call(t, http.MethodPost, "/api/dialog/folder", string(body), &res); code != http.StatusOK ||
		!res.Cancelled || res.Path != "" || fp.initial != typed {
		t.Fatalf("cancel: %d %+v initial=%q", code, res, fp.initial)
	}
}

func TestFolderDialogOneAtATime(t *testing.T) {
	fp := &fakePicker{ok: true, path: `C:\x`, block: make(chan struct{}), entered: make(chan struct{}, 1)}
	e := pickerEnv(t, fp)
	first := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, e.s.BaseURL()+"/api/dialog/folder", nil)
		resp, err := e.browser.Do(req)
		if err != nil {
			first <- 0
			return
		}
		_ = resp.Body.Close()
		first <- resp.StatusCode
	}()
	<-fp.entered
	var res pickResult
	if code := e.callAny(t, http.MethodPost, "/api/dialog/folder", "", &res); code != http.StatusConflict || res.Code != "busy" {
		t.Fatalf("second: %d %+v", code, res)
	}
	close(fp.block)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first: %d", code)
	}
}

// A network start folder is dropped before any stat (a dead share would block the dialog).
func TestPickerStartSkipsNetworkPaths(t *testing.T) {
	for _, p := range []string{`\\iw-no-such-host\share`, `//iw-no-such-host/share`, `\\?\UNC\iw-no-such-host\share`} {
		if got := existingDir(p); got != "" {
			t.Errorf("existingDir(%q) = %q, want \"\"", p, got)
		}
	}
	if got := existingDir(t.TempDir()); got == "" {
		t.Error("existingDir(local temp dir) = \"\"")
	}
}
