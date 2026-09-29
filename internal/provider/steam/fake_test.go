package steam

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	ownerID = "76561197996210591"
	fileID  = "3739613434"
)

// fakeSteam serves saved steamcommunity.com / api.steampowered.com answers
// (testdata: real responses of item 3739613434, other users anonymised).
type fakeSteam struct {
	t   *testing.T
	srv *httptest.Server

	mu           sync.Mutex
	blocks       []string // comment HTML blocks, newest first
	timeLastPost int64
	requests     map[string]int // path → count
	renders      [][2]int       // start, count of each render call
	rateLimit    int            // answer 429 this many times
	profilePage  string
	posts        []*http.Request
	postForms    []map[string]string
	postReply    func(r *http.Request) (int, string) // POST .../post/ answer
}

var blockStart = regexp.MustCompile(`<div data-panel="[^"]*" class="commentthread_comment `)

func loadBlocks(t *testing.T) ([]string, int64) {
	t.Helper()
	var (
		out []string
		tlp int64
	)
	for _, name := range []string{"render_0.json", "render_10.json", "render_20.json"} {
		raw, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: fixed fixture names
		if err != nil {
			t.Fatal(err)
		}
		var r renderPage
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		if tlp == 0 {
			tlp = r.TimeLastPost
		}
		idx := blockStart.FindAllStringIndex(r.HTML, -1)
		for i, m := range idx {
			end := len(r.HTML)
			if i+1 < len(idx) {
				end = idx[i+1][0]
			}
			out = append(out, r.HTML[m[0]:end])
		}
	}
	return out, tlp
}

func newFake(t *testing.T) *fakeSteam {
	t.Helper()
	f := &fakeSteam{t: t, requests: map[string]int{}}
	f.blocks, f.timeLastPost = loadBlocks(t)
	page, err := os.ReadFile(filepath.Join("testdata", "myworkshopfiles.html"))
	if err != nil {
		t.Fatal(err)
	}
	f.profilePage = string(page)
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSteam) provider(t *testing.T) *Provider {
	t.Helper()
	p := New(Options{Dir: t.TempDir(), CommunityURL: f.srv.URL, APIURL: f.srv.URL, MinGap: time.Millisecond, PageSize: 10})
	id := ownerID
	if _, err := p.Save(Update{SteamID: &id}); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *fakeSteam) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[path]
}

func (f *fakeSteam) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests[r.URL.Path]++
	if f.rateLimit > 0 {
		f.rateLimit--
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	_ = r.ParseForm()
	thread := "/" + ownerID + "/" + fileID + "/"
	switch r.URL.Path {
	case "/profiles/" + ownerID + "/myworkshopfiles/":
		if r.Form.Get("p") != "1" {
			_, _ = w.Write([]byte("<html><body>Showing 31-30 of 3 entries</body></html>"))
			return
		}
		_, _ = w.Write([]byte(f.profilePage))
	case "/ISteamRemoteStorage/GetPublishedFileDetails/v1/":
		n, _ := strconv.Atoi(r.Form.Get("itemcount"))
		var ds []map[string]any
		for i := range n {
			ds = append(ds, map[string]any{"publishedfileid": r.Form.Get("publishedfileids[" + strconv.Itoa(i) + "]"), "result": 1, "creator": ownerID})
		}
		writeJSON(w, map[string]any{"response": map[string]any{"result": 1, "resultcount": n, "publishedfiledetails": ds}})
	case "/comment/PublishedFile_Public/render" + thread:
		start, _ := strconv.Atoi(r.Form.Get("start"))
		count, _ := strconv.Atoi(r.Form.Get("count"))
		f.renders = append(f.renders, [2]int{start, count})
		end := min(start+count, len(f.blocks))
		start = min(start, end)
		writeJSON(w, map[string]any{
			"success": true, "name": "PublishedFile_Public_" + ownerID + "_" + fileID, "start": start, "pagesize": count,
			"total_count": len(f.blocks), "upvotes": 0, "has_upvoted": 0, "timelastpost": f.timeLastPost,
			"comments_html": strings.Join(f.blocks[start:end], ""),
		})
	case "/comment/PublishedFile_Public/post" + thread:
		form := map[string]string{}
		for k := range r.PostForm {
			form[k] = r.PostForm.Get(k)
		}
		f.posts = append(f.posts, r)
		f.postForms = append(f.postForms, form)
		code, body := http.StatusOK, `{"success":false}`
		if f.postReply != nil {
			code, body = f.postReply(r)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
