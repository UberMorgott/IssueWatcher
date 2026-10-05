package release

import (
	"archive/zip"
	"context"
	"crypto/sha1" //nolint:gosec // the Factorio portal's archive hash
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/provider/github/githubtest"
	"github.com/UberMorgott/issuewatcher/internal/runner"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

const (
	codeKey     = "github:octo/mod"
	nexusKey    = "nexus:wartales/202"
	factorioKey = "factorio:my-mod"
)

// fakePub is a mod platform: PublishTargets lists what Publish added.
type fakePub struct {
	mu        sync.Mutex
	platform  string
	fileID    string
	versions  []provider.PublishVersion
	publishes int
	probeErrs int   // the next n PublishTargets calls fail (HTTP 500)
	checkErr  error // CheckPublish refuses
}

func (f *fakePub) PublishTargets(_ context.Context, _ provider.Project) (provider.PublishTargets, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.probeErrs > 0 {
		f.probeErrs--
		return provider.PublishTargets{}, errors.New("HTTP 500: internal error")
	}
	return provider.PublishTargets{Files: []provider.PublishFile{{ID: f.fileID, Name: "Main file", Versions: slices.Clone(f.versions)}}}, nil
}

func (f *fakePub) CheckPublish(_ provider.Project, req provider.PublishRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.checkErr != nil {
		return f.checkErr
	}
	if req.Version == "" || (req.Path == "" && req.UploadID == "") {
		return provider.ErrBadPublish
	}
	return nil
}

func (f *fakePub) Publish(_ context.Context, _ provider.Project, req provider.PublishRequest, _ func(provider.PublishProgress)) (provider.PublishResult, error) {
	b, err := os.ReadFile(req.Path)
	if err != nil {
		return provider.PublishResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishes++
	v := provider.PublishVersion{ID: fmt.Sprintf("v%d", len(f.versions)+1), Version: req.Version}
	if f.platform == "factorio" {
		s := sha1.Sum(b) //nolint:gosec // portal hash
		v.SHA1 = hex.EncodeToString(s[:])
	}
	f.versions = append(f.versions, v)
	return provider.PublishResult{VersionID: v.ID}, nil
}

func (f *fakePub) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.publishes
}

// countGit counts pushes per destination ref.
type countGit struct {
	ExecGit
	mu     sync.Mutex
	pushes map[string]int
}

func (g *countGit) Run(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	out, err := g.ExecGit.Run(ctx, dir, env, args...)
	if slices.Contains(args, "push") && err == nil {
		dst := args[len(args)-1]
		if _, after, ok := strings.Cut(dst, ":"); ok {
			dst = after
		}
		g.mu.Lock()
		g.pushes[dst]++
		g.mu.Unlock()
	}
	return out, err
}

func (g *countGit) count(ref string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pushes[ref]
}

type tenv struct {
	t        *testing.T
	st       *store.Store
	gh       *githubtest.Server
	git      *countGit
	nexus    *fakePub
	factorio *fakePub
	deps     Deps
	folder   string
	bare     string
	codeID   int64
	head     string // folder HEAD before the release

	mu       sync.Mutex
	cfg      config.Settings
	gateOK   bool
	gates    int
	builds   int
	locks    int
	crashAt  string // step name (step or step:target)
	crashPnt string // before | after
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:noctx,gosec // test helper
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const infoJSON = `{
  "name": "my-mod",
  "version": "1.0.0",
  "title": "My mod",
  "factorio_version": "2.0"
}
`

const changelogTxt = `---------------------------------------------------------------------------------------------------
Version: 1.0.0
Date: 2026-09-01
  Changes:
    - First release.
`

func newEnv(t *testing.T) *tenv {
	t.Helper()
	root := t.TempDir()
	e := &tenv{t: t, folder: filepath.Join(root, "mod"), bare: filepath.Join(root, "origin.git"), gateOK: true,
		nexus:    &fakePub{platform: "nexus", fileID: "file-main", versions: []provider.PublishVersion{{ID: "v0", Version: "1.0.0"}}},
		factorio: &fakePub{platform: "factorio", fileID: "my-mod", versions: []provider.PublishVersion{{ID: "1.0.0", Version: "1.0.0"}}},
		git:      &countGit{pushes: map[string]int{}}}
	git(t, root, "init", "-q", "--bare", "-b", "main", e.bare)
	git(t, root, "init", "-q", "-b", "main", e.folder)
	for _, kv := range [][2]string{{"user.name", "Maintainer"}, {"user.email", "m@example.com"}, {"core.autocrlf", "false"}} {
		git(t, e.folder, "config", kv[0], kv[1])
	}
	write(t, filepath.Join(e.folder, "info.json"), infoJSON)
	write(t, filepath.Join(e.folder, "changelog.txt"), changelogTxt)
	write(t, filepath.Join(e.folder, "control.lua"), "-- v1\n")
	write(t, filepath.Join(e.folder, ".gitignore"), "")
	git(t, e.folder, "add", ".")
	git(t, e.folder, "commit", "-q", "-m", "init")
	git(t, e.folder, "tag", "v1.0.0")
	write(t, filepath.Join(e.folder, "control.lua"), "-- v1 fixed\n")
	git(t, e.folder, "commit", "-q", "-am", "Fix the crash on load")
	git(t, e.folder, "remote", "add", "origin", e.bare)
	git(t, e.folder, "push", "-q", "origin", "main", "v1.0.0")
	e.head = git(t, e.folder, "rev-parse", "HEAD")

	e.gh = githubtest.New(t)
	a := github.NewAuth(filepath.Join(root, "secrets"))
	a.WebURL, a.APIURL, a.HTTP = e.gh.URL, e.gh.URL, e.gh.Client()
	if _, err := a.ConvertManifest(t.Context(), githubtest.ManifestCode, 1); err != nil {
		t.Fatal(err)
	}
	v, c := github.PKCE()
	if _, err := a.Exchange(t.Context(), e.gh.IssueCode(c), v, "r"); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(t.Context(), filepath.Join(root, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e.st = store.New(db)
	ctx := t.Context()
	project := func(platform, ext, name, url string) int64 {
		src, err := e.st.UpsertSource(ctx, platform, "me")
		if err != nil {
			t.Fatal(err)
		}
		ps, err := e.st.SyncProjects(ctx, src, []provider.Project{{ExternalID: ext, Name: name, URL: url}})
		if err != nil {
			t.Fatal(err)
		}
		return ps[0].ID
	}
	e.codeID = project("github", "octo/mod", "octo/mod", "https://github.com/octo/mod")
	nx := project("nexus", "wartales/202", "My mod", "https://www.nexusmods.com/wartales/mods/202")
	fa := project("factorio", "my-mod", "My mod", "https://mods.factorio.com/mod/my-mod")
	if err := e.st.SetProjectLinks(ctx, e.codeID, []int64{nx, fa}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetLocalPath(ctx, e.codeID, e.folder); err != nil {
		t.Fatal(err)
	}

	e.cfg = config.Defaults()
	ap := config.DefaultProjectAutopilot()
	ap.Enabled = true
	ap.Publish = map[string]bool{nexusKey: true, factorioKey: true}
	if e.cfg.Agents.Projects == nil {
		e.cfg.Agents.Projects = map[string]config.ProjectAgent{}
	}
	e.cfg.Agents.Projects[codeKey] = config.ProjectAgent{Autopilot: ap, PublishProfile: config.PublishProfile{
		Build:     config.BuildProfile{Command: "build.cmd", Output: "dist/{name}_{version}.zip"},
		Version:   config.VersionProfile{Kind: config.VersionFactorioInfo},
		Changelog: config.ChangelogProfile{Kind: config.ChangelogFactorio},
		Targets:   map[string]config.TargetProfile{nexusKey: {FileID: "file-main", Category: "main"}, factorioKey: {}},
	}}

	gp := github.NewProvider(a)
	e.deps = Deps{
		Store: e.st, DataDir: filepath.Join(root, "data"), Git: e.git, Releaser: gp,
		Settings: func() config.Settings { e.mu.Lock(); defer e.mu.Unlock(); return e.cfg },
		Publishers: func(platform string) provider.Publisher {
			switch platform {
			case "nexus":
				return e.nexus
			case "factorio":
				return e.factorio
			}
			return nil
		},
		DefaultBranch: func(context.Context, string) (string, error) { return "main", nil },
		GitToken:      func(context.Context) (string, error) { return "tok-secret", nil },
		TokenEnv:      runner.TokenEnv,
		GitURL:        func(string) string { return e.bare },
		Folders:       e,
		Gate: func(_ context.Context, project, _, dir, _ string) (CmdResult, bool) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.gates++
			if project != codeKey || dir != e.folder {
				return CmdResult{Command: "verify", ExitCode: 2, Output: "wrong folder " + dir}, true
			}
			if !e.gateOK {
				return CmdResult{Command: "verify", ExitCode: 1, Output: "tests failed"}, true
			}
			return CmdResult{Command: "verify", OK: true}, true
		},
		Command:   e.build,
		PollEvery: time.Millisecond,
	}
	return e
}

// AcquireFolder implements FolderLocker (counts uses).
func (e *tenv) AcquireFolder(string) (func(), error) {
	e.mu.Lock()
	e.locks++
	e.mu.Unlock()
	return func() {}, nil
}

// build is the profile's build command: zips the worktree's mod as Factorio expects.
func (e *tenv) build(_ context.Context, dir, command, _ string, _ time.Duration) CmdResult {
	e.mu.Lock()
	e.builds++
	e.mu.Unlock()
	info, err := os.ReadFile(filepath.Join(dir, "info.json")) //nolint:gosec // test worktree
	if err != nil {
		return CmdResult{Command: command, ExitCode: 1, Output: err.Error()}
	}
	var v struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	_ = json.Unmarshal(info, &v)
	top := v.Name + "_" + v.Version
	out := filepath.Join(dir, "dist", top+".zip")
	if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
		return CmdResult{Command: command, ExitCode: 1, Output: err.Error()}
	}
	f, err := os.Create(out) //nolint:gosec // test worktree
	if err != nil {
		return CmdResult{Command: command, ExitCode: 1, Output: err.Error()}
	}
	zw := zip.NewWriter(f)
	for _, name := range []string{"info.json", "changelog.txt", "control.lua"} {
		w, _ := zw.Create(top + "/" + name)
		b, _ := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // test worktree
		_, _ = w.Write(b)
	}
	_ = zw.Close()
	_ = f.Close()
	return CmdResult{Command: command, OK: true}
}

// engine builds an engine over the env (a "process"); crash makes the hook abort at e.crashAt/e.crashPnt.
func (e *tenv) engine(crash bool) *Engine {
	en := New(e.deps)
	if crash {
		en.hook = func(point string, st store.Step) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			if point == e.crashPnt && stepName(st) == e.crashAt {
				return errors.New("simulated crash")
			}
			return nil
		}
	}
	return en
}

func (e *tenv) steps(id int64) map[string]store.Step {
	e.t.Helper()
	steps, err := e.st.RunSteps(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]store.Step{}
	for _, s := range steps {
		out[stepName(s)] = s
	}
	return out
}

func (e *tenv) run(id int64) store.Run {
	e.t.Helper()
	r, err := e.st.Run(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// ghCalls counts fake GitHub requests by method + path prefix.
func (e *tenv) ghCalls(method, substr string) int {
	n := 0
	for _, c := range e.gh.Calls() {
		if c.Method == method && strings.Contains(c.Path, substr) {
			n++
		}
	}
	return n
}

// assertSentOnce checks every external action happened exactly once.
func (e *tenv) assertSentOnce(t *testing.T) {
	t.Helper()
	got := map[string]int{
		"push":             e.git.count("refs/heads/main"),
		"tag":              e.git.count("refs/tags/v1.0.1"),
		"gh_release":       e.ghCalls("POST", "/repos/octo/mod/releases") - e.ghCalls("POST", "/assets"),
		"gh_asset":         e.ghCalls("POST", "/assets"),
		"publish:nexus":    e.nexus.count(),
		"publish:factorio": e.factorio.count(),
	}
	for k, n := range got {
		if n != 1 {
			t.Errorf("%s sent %d times, want exactly 1 (all: %v)", k, n, got)
		}
	}
}

func (e *tenv) release(en *Engine, req Request) store.Run {
	e.t.Helper()
	r, p, err := en.Release(context.Background(), e.codeID, req)
	if err != nil {
		e.t.Fatalf("release: %v (refusals %+v)", err, p.Refusals)
	}
	en.Wait()
	return r
}

func TestReleaseHappyPath(t *testing.T) {
	e := newEnv(t)
	en := e.engine(false)
	p, err := en.Plan(t.Context(), e.codeID, Request{})
	if err != nil || !p.OK {
		t.Fatalf("plan %v %+v", err, p.Refusals)
	}
	if p.Version != "1.0.1" || p.CurrentVersion != "1.0.0" || p.BaseTag != "v1.0.0" || p.Asset != "my-mod_1.0.1.zip" ||
		!strings.Contains(p.Changelog, "Fix the crash on load") || !slices.Equal(p.WritesVersion, []string{"info.json"}) ||
		len(p.Targets) != 2 || p.Targets[0].LatestVersion != "1.0.0" || p.Targets[0].Auth != "ok" {
		t.Fatalf("plan %+v", p)
	}
	if st, _ := os.ReadFile(filepath.Join(e.folder, "info.json")); string(st) != infoJSON {
		t.Fatal("the plan changed the folder")
	}

	r := e.release(en, Request{Items: nil})
	r = e.run(r.ID)
	if r.State != store.RunDone || r.Version != "1.0.1" || r.ArtifactSHA256 == "" {
		t.Fatalf("run %+v", r)
	}
	steps := e.steps(r.ID)
	want := []string{"bump", "build", "archive_check", "gate", "smoke", "push", "tag", "gh_release", "gh_asset:my-mod_1.0.1.zip",
		"publish:" + nexusKey, "publish:" + factorioKey, "available:" + nexusKey, "available:" + factorioKey}
	for _, n := range want {
		s, ok := steps[n]
		wantState := store.StepSent
		if n == "smoke" {
			wantState = store.StepSkipped
		}
		if !ok || s.State != wantState {
			t.Errorf("step %s: %+v", n, s)
		}
	}
	m, _ := manifestOf(r)
	if steps["push"].IdemKey != "main:"+m.BumpSHA || steps["publish:"+nexusKey].IdemKey != nexusKey+":1.0.1:"+r.ArtifactSHA256 {
		t.Errorf("idem keys %q %q", steps["push"].IdemKey, steps["publish:"+nexusKey].IdemKey)
	}
	if subj := git(t, e.folder, "log", "-1", "--format=%s"); subj != "chore(release): v1.0.1" || git(t, e.folder, "rev-parse", "HEAD^") != e.head {
		t.Fatalf("bump commit %q", subj)
	}
	if git(t, e.bare, "rev-parse", "main") != m.BumpSHA || git(t, e.bare, "rev-parse", "v1.0.1^{commit}") != m.BumpSHA ||
		git(t, e.bare, "cat-file", "-t", "v1.0.1") != "tag" {
		t.Fatal("remote main / annotated tag not at the bump")
	}
	if !strings.Contains(git(t, e.folder, "show", "HEAD:info.json"), `"version": "1.0.1"`) ||
		!strings.Contains(git(t, e.folder, "show", "HEAD:changelog.txt"), "Version: 1.0.1") {
		t.Fatal("bump content")
	}
	e.gh.Mu.Lock()
	rels := len(e.gh.Releases)
	body, target, assets := e.gh.Releases[0].Body, e.gh.Releases[0].TargetCommitish, len(e.gh.Releases[0].Assets)
	e.gh.Mu.Unlock()
	if rels != 1 || target != m.BumpSHA || assets != 1 || !strings.Contains(body, "Fix the crash on load") {
		t.Fatalf("gh release %d %s %d %q", rels, target, assets, body)
	}
	e.assertSentOnce(t)
	if e.gates != 1 || e.builds != 1 {
		t.Fatalf("gates %d builds %d", e.gates, e.builds)
	}
	evs, _ := e.st.Events(t.Context(), false, 10)
	if len(evs) != 1 || evs[0].Kind != "release.done" {
		t.Fatalf("events %+v", evs)
	}
	if _, err := os.Stat(filepath.Join(e.deps.DataDir, "release", fmt.Sprint(r.ID), "src")); err == nil {
		t.Fatal("build worktree left behind")
	}
}

// Crash right before and right after every external action: a fresh engine
// (app restart) reconciles with probes and finishes; each action is sent once.
func TestReleaseCrashResumeSendsOnce(t *testing.T) {
	actions := []string{"push", "tag", "gh_release", "gh_asset:my-mod_1.0.1.zip", "publish:" + nexusKey, "publish:" + factorioKey}
	for _, action := range actions {
		for _, point := range []string{"before", "after"} {
			t.Run(action+"/"+point, func(t *testing.T) {
				e := newEnv(t)
				e.crashAt, e.crashPnt = action, point
				r := e.release(e.engine(true), Request{})
				if st := e.steps(r.ID)[action]; st.State != store.StepSending {
					t.Fatalf("after the crash %s is %s, want sending", action, st.State)
				}
				if got := e.run(r.ID).State; got != store.RunRunning {
					t.Fatalf("run %s after the crash", got)
				}
				restarted := e.engine(false)
				if err := restarted.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
				restarted.Wait()
				if got := e.run(r.ID); got.State != store.RunDone {
					t.Fatalf("run after restart: %+v steps %+v", got, e.steps(r.ID))
				}
				e.assertSentOnce(t)
				if st := e.steps(r.ID)[action]; st.Attempt != 1 && point == "after" {
					t.Errorf("%s attempts %d after a crash after the send", action, st.Attempt)
				}
			})
		}
	}
}

// An ambiguous probe (platform answers 500) holds the run; nothing is resent.
func TestReleaseAmbiguousProbeHolds(t *testing.T) {
	e := newEnv(t)
	e.crashAt, e.crashPnt = "publish:"+nexusKey, "after"
	r := e.release(e.engine(true), Request{})
	e.nexus.mu.Lock()
	e.nexus.probeErrs = 100
	e.nexus.mu.Unlock()
	restarted := e.engine(false)
	if err := restarted.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted.Wait()
	run := e.run(r.ID)
	if run.State != store.RunHeld || run.HeldReason != "check:publish:"+nexusKey {
		t.Fatalf("run %+v", run)
	}
	if st := e.steps(r.ID)["publish:"+nexusKey]; st.State != store.StepUnknown || !strings.Contains(st.Error, "500") {
		t.Fatalf("step %+v", st)
	}
	if _, err := restarted.Resume(t.Context(), r.ID); err != nil { // still unclear: held again
		t.Fatal(err)
	}
	restarted.Wait()
	if got := e.run(r.ID); got.State != store.RunHeld || e.nexus.count() != 1 {
		t.Fatalf("resume while unclear: %+v, publishes %d", got, e.nexus.count())
	}
	e.nexus.mu.Lock()
	e.nexus.probeErrs = 0
	e.nexus.mu.Unlock()
	if _, err := restarted.Resume(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	restarted.Wait()
	if got := e.run(r.ID); got.State != store.RunDone {
		t.Fatalf("after the platform answers: %+v", got)
	}
	e.assertSentOnce(t)
	evs, _ := e.st.Events(t.Context(), true, 10)
	if len(evs) < 2 || evs[len(evs)-1].Severity != store.SeverityAttention {
		t.Fatalf("events %+v", evs)
	}
}

// GitHub answers 500 to the release probe after a crash: held, not resent.
func TestReleaseGitHub500Holds(t *testing.T) {
	e := newEnv(t)
	e.crashAt, e.crashPnt = "gh_release", "after"
	r := e.release(e.engine(true), Request{})
	e.gh.FailNext(1, 500, nil, "server error")
	restarted := e.engine(false)
	if err := restarted.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted.Wait()
	if got := e.run(r.ID); got.State != store.RunHeld || got.HeldReason != "check:gh_release" {
		t.Fatalf("run %+v", got)
	}
	if n := e.ghCalls("POST", "/repos/octo/mod/releases"); n != 1 {
		t.Fatalf("release created %d times", n)
	}
	if _, err := restarted.Resume(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	restarted.Wait()
	if got := e.run(r.ID); got.State != store.RunDone {
		t.Fatalf("run %+v", got)
	}
	e.assertSentOnce(t)
}

func TestReleaseOneUnfinishedRunPerProject(t *testing.T) {
	e := newEnv(t)
	e.crashAt, e.crashPnt = "push", "before"
	r := e.release(e.engine(true), Request{})
	_, p, err := e.engine(false).Release(t.Context(), e.codeID, Request{})
	var re *RefusedError
	if !errors.As(err, &re) || !slices.ContainsFunc(p.Refusals, func(x Refusal) bool { return x.Code == CodeBusy }) {
		t.Fatalf("second release: %v %+v", err, p.Refusals)
	}
	// The store refuses too, whatever the plan said.
	b, _ := json.Marshal(Manifest{})
	if _, err := e.st.CreateReleaseRun(t.Context(), store.NewReleaseRun{ProjectID: e.codeID, Manifest: b}); !errors.Is(err, store.ErrRunBusy) {
		t.Fatalf("store: %v", err)
	}
	if e.run(r.ID).State != store.RunRunning {
		t.Fatal("first run changed")
	}
}

// A failed gate holds the run before any public step; cancel drops the bump commit.
func TestReleaseCancelBeforePushDropsBump(t *testing.T) {
	e := newEnv(t)
	e.gateOK = false
	en := e.engine(false)
	r := e.release(en, Request{})
	run := e.run(r.ID)
	if run.State != store.RunHeld || run.HeldReason != HeldGate {
		t.Fatalf("run %+v", run)
	}
	if git(t, e.folder, "log", "-1", "--format=%s") != "chore(release): v1.0.1" {
		t.Fatal("no bump commit to drop")
	}
	res, err := en.Cancel(t.Context(), r.ID)
	if err != nil || !res.BumpDropped || res.Run.State != store.RunCancelled {
		t.Fatalf("cancel %+v %v", res, err)
	}
	if git(t, e.folder, "rev-parse", "HEAD") != e.head || git(t, e.folder, "status", "--porcelain") != "" {
		t.Fatal("folder not back at the release head")
	}
	if e.git.count("refs/heads/main") != 0 || e.nexus.count() != 0 {
		t.Fatal("something was published")
	}
	if _, err := en.Cancel(t.Context(), r.ID); !errors.Is(err, ErrState) {
		t.Fatalf("second cancel %v", err)
	}
	// A new release can start (the slot is free) and plans the same version.
	p, err := en.Plan(t.Context(), e.codeID, Request{})
	if err != nil || !p.OK || p.Version != "1.0.1" {
		t.Fatalf("plan after cancel %v %+v", err, p.Refusals)
	}
}

// Cancel after a public step stops the rest and keeps the bump.
func TestReleaseCancelAfterPushKeepsBump(t *testing.T) {
	e := newEnv(t)
	e.crashAt, e.crashPnt = "tag", "before"
	en := e.engine(true)
	r := e.release(en, Request{})
	res, err := en.Cancel(t.Context(), r.ID)
	if err != nil || res.BumpDropped || res.Run.State != store.RunCancelled {
		t.Fatalf("cancel %+v %v", res, err)
	}
	if git(t, e.folder, "log", "-1", "--format=%s") != "chore(release): v1.0.1" {
		t.Fatal("bump dropped after push")
	}
}

// A refused target fails alone; skip it and resume: the rest finishes.
func TestReleaseSkipTarget(t *testing.T) {
	e := newEnv(t)
	e.factorio.checkErr = fmt.Errorf("%w: portal says no", provider.ErrBadPublish)
	en := e.engine(false)
	r := e.release(en, Request{})
	run := e.run(r.ID)
	if run.State != store.RunHeld || run.HeldReason != "failed:publish:"+factorioKey {
		t.Fatalf("run %+v", run)
	}
	steps := e.steps(r.ID)
	if steps["publish:"+nexusKey].State != store.StepSent || steps["available:"+nexusKey].State != store.StepSent ||
		steps["publish:"+factorioKey].State != store.StepFailed {
		t.Fatalf("steps %+v", steps)
	}
	if _, err := en.Skip(t.Context(), r.ID, StepGate, ""); !errors.Is(err, ErrBadStep) {
		t.Fatalf("skip gate: %v", err)
	}
	if _, err := en.Skip(t.Context(), r.ID, StepPublish, nexusKey); !errors.Is(err, ErrState) {
		t.Fatalf("skip a sent publish: %v", err)
	}
	if _, err := en.Skip(t.Context(), r.ID, StepPublish, factorioKey); err != nil {
		t.Fatal(err)
	}
	if _, err := en.Resume(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	en.Wait()
	steps = e.steps(r.ID)
	if got := e.run(r.ID); got.State != store.RunDone || steps["available:"+factorioKey].State != store.StepSkipped ||
		e.factorio.count() != 0 || e.nexus.count() != 1 {
		t.Fatalf("run %+v steps %+v", got, steps)
	}
}

func TestReleasePlanRefusals(t *testing.T) {
	e := newEnv(t)
	write(t, filepath.Join(e.folder, "control.lua"), "-- dirty\n")
	e.cfg.Agents.Autopilot.Paused = true
	p, err := e.engine(false).Plan(t.Context(), e.codeID, Request{Version: "1.0.0", Targets: []string{"steam:1"}})
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, r := range p.Refusals {
		codes = append(codes, r.Code)
	}
	for _, c := range []string{CodePaused, CodeDirtyFolder, CodeTagExists, CodeVersionConflict, CodeBadRequest} {
		if !slices.Contains(codes, c) {
			t.Errorf("refusal %s missing in %v", c, codes)
		}
	}
	if p.OK {
		t.Fatal("plan ok")
	}
	// Remote ahead of the folder → head_not_remote.
	e2 := newEnv(t)
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", "-q", e2.bare, other)
	git(t, other, "-c", "user.name=x", "-c", "user.email=x@x", "commit", "-q", "--allow-empty", "-m", "remote work")
	git(t, other, "push", "-q", "origin", "main")
	p, _ = e2.engine(false).Plan(t.Context(), e2.codeID, Request{})
	if !slices.ContainsFunc(p.Refusals, func(r Refusal) bool { return r.Code == CodeHeadNotRemote }) {
		t.Fatalf("refusals %+v", p.Refusals)
	}
}
