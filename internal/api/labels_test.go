package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/runner"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// TestLabelsAPI: project labels, «Добавить метки» on a label job under review
// against the fake GitHub (unknown → 400, add only, repeat adds nothing), signed out → 409.
func TestLabelsAPI(t *testing.T) {
	var r *runner.Runner
	e := syncedEnv(t, func(o *Options) {
		r = runner.New(runner.Options{Store: o.Store, DataDir: filepath.Join(t.TempDir(), "data"), Settings: config.Defaults,
			Labels: github.NewProvider(o.GitHub), LookPath: func(string) (string, error) { return "", context.Canceled }})
		o.Runner = r
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); r.Wait() })
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	e.gh.Mu.Lock()
	e.gh.Labels = map[string][]string{"octo/app": {"bug", "documentation", "enhancement"}}
	e.gh.Mu.Unlock()

	var repos []store.Repo
	e.call(t, http.MethodGet, "/api/projects", "", &repos)
	pid := strconv.FormatInt(repos[0].ID, 10)
	// Labels come from SQLite: the first GET finds none and starts a
	// background fetch; its result lands with data.changed{reason:labels}.
	br := openStream(t, e.s)
	var pl ProjectLabels
	if code := e.call(t, http.MethodGet, "/api/projects/"+pid+"/labels", "", &pl); code != http.StatusOK || len(pl.Labels) != 0 || !pl.Refreshing || pl.FetchedAt != "" {
		t.Fatalf("first labels read %d %+v", code, pl)
	}
	for {
		name, data := readEvent(t, br)
		if name == EventDataChanged && data == `{"reason":"labels","repo":"octo/app"}` {
			break
		}
	}
	listCalls := func() int {
		n := 0
		for _, c := range e.gh.Calls() {
			if c.Method == http.MethodGet && c.Path == "/repos/octo/app/labels" {
				n++
			}
		}
		return n
	}
	before := listCalls()
	for range 3 {
		if code := e.call(t, http.MethodGet, "/api/projects/"+pid+"/labels", "", &pl); code != http.StatusOK || len(pl.Labels) != 3 ||
			pl.Labels[1].Name != "documentation" || pl.Refreshing || pl.FetchedAt == "" {
			t.Fatalf("project labels %d %+v", code, pl)
		}
	}
	if n := listCalls(); n != before || before != 1 {
		t.Fatalf("label list calls: %d before, %d after three reads; GET must not call GitHub", before, n)
	}
	if code := e.call(t, http.MethodGet, "/api/projects/99999/labels", "", nil); code != http.StatusNotFound {
		t.Fatalf("labels of a missing project: %d", code)
	}

	var items store.IssueChunk
	e.call(t, http.MethodGet, "/api/items?q=%231", "", &items)
	item := items.Items[0].ID // #1 has bug on GitHub and locally
	// A label job under review (as the runner leaves it after the agent).
	review := func() string {
		t.Helper()
		j, err := e.store.CreateJob(t.Context(), item, "label", "codex", store.OriginManual, "")
		if err != nil {
			t.Fatal(err)
		}
		res, _ := json.Marshal(runner.Result{Labels: []string{"documentation"}})
		if _, err := e.store.UpdateJob(t.Context(), j.ID, nil, store.JobChange{State: new(store.JobNeedsReview), Result: res}); err != nil {
			t.Fatal(err)
		}
		return "/api/jobs/" + strconv.FormatInt(j.ID, 10) + "/labels"
	}
	path := review()
	for body, want := range map[string]int{
		`{"labels":["made-up"]}`: http.StatusBadRequest,
		`{"labels":[]}`:          http.StatusBadRequest,
		`nope`:                   http.StatusBadRequest,
	} {
		if code := e.call(t, http.MethodPost, path, body, nil); code != want {
			t.Errorf("POST %s: %d, want %d", body, code, want)
		}
	}
	var j store.Job
	if code := e.call(t, http.MethodPost, path, `{"labels":["documentation","BUG"]}`, &j); code != http.StatusOK || j.State != store.JobDone {
		t.Fatalf("apply %d %+v", code, j)
	}
	e.gh.Mu.Lock()
	gh, repoLabels, adds := slices.Clone(e.gh.Issues[0].Labels), len(e.gh.Labels["octo/app"]), e.gh.LabelAdds
	e.gh.Mu.Unlock()
	if !slices.Equal(gh, []string{"bug", "documentation"}) || repoLabels != 3 || adds != 1 {
		t.Fatalf("GitHub: issue %v, repo labels %d, adds %d", gh, repoLabels, adds)
	}
	var d store.IssueDetail
	if e.call(t, http.MethodGet, "/api/items/"+strconv.FormatInt(item, 10), "", &d); !slices.Equal(d.Labels, []string{"bug", "documentation"}) {
		t.Fatalf("local labels %v", d.Labels)
	}
	if code := e.call(t, http.MethodPost, path, `{"labels":["bug"]}`, nil); code != http.StatusConflict {
		t.Fatalf("apply to a done job: %d", code)
	}
	// Everything already on the issue: done, nothing sent.
	if code := e.call(t, http.MethodPost, review(), `{"labels":["bug","documentation"]}`, &j); code != http.StatusOK || j.State != store.JobDone {
		t.Fatalf("re-apply %d %+v", code, j)
	}
	e.gh.Mu.Lock()
	adds = e.gh.LabelAdds
	e.gh.Mu.Unlock()
	if adds != 1 {
		t.Fatalf("re-apply sent labels: %d", adds)
	}

	if err := e.auth.Logout(); err != nil {
		t.Fatal(err)
	}
	// Signed out: the stored labels are still served (no GitHub call).
	if code := e.call(t, http.MethodGet, "/api/projects/"+pid+"/labels", "", &pl); code != http.StatusOK || len(pl.Labels) != 3 {
		t.Fatalf("labels signed out: %d %+v", code, pl)
	}
	if code := e.call(t, http.MethodPost, review(), `{"labels":["enhancement"]}`, nil); code != http.StatusConflict {
		t.Fatalf("apply signed out: %d", code)
	}
}
