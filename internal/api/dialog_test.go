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
