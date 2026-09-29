package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/github/githubtest"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// closeIssue marks octo/demo issue n closed, as a sync would.
func (e *env) closeIssue(n int) {
	e.t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := e.st.ApplyItems(e.t.Context(), e.src, e.proj[0].ID, []provider.Item{{
		ExternalID: "I_0_" + strconv.Itoa(n), Kind: "issue", Number: n, Title: "Crash #" + strconv.Itoa(n) + " on start",
		Author: "alice", Open: false, CreatedAt: now, UpdatedAt: now.Add(time.Minute), ClosedAt: now.Add(time.Minute),
	}}, githubtest.Login); err != nil {
		e.t.Fatal(err)
	}
	e.r.Refresh()
}

// waitResult polls job id until ok(job).
func (e *env) waitResult(id int64, ok func(store.Job, Result) bool) store.Job {
	e.t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; {
		j, err := e.st.Job(e.t.Context(), id)
		if err != nil {
			e.t.Fatal(err)
		}
		if ok(j, result(e.t, j)) {
			return j
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("job %d: %s %s", id, j.State, j.Result)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Direct mode (the default): the agent works in the mapped folder, commits
// "Fixes #N"; the dispatcher checks git facts; Push publishes; the issue
// closing on the platform ends the job.
func TestDirectFixCommitPushClose(t *testing.T) {
	mode(t, "ok")
	e := setup(t, 1, func(s *config.Settings) {
		s.Agents.Projects["octo/demo"] = config.ProjectAgent{Prompt: "Project note {localPath}."}
	})
	// The owner's work in progress: an edited and an untracked file.
	writeFile(t, filepath.Join(e.local, "README.md"), "demo\nwip\n")
	writeFile(t, filepath.Join(e.local, "wip.txt"), "draft\n")
	start := run(t, e.local, "rev-parse", "HEAD")

	j := e.wait(e.enqueue("fix", e.items[0])[0].ID, store.JobNeedsReview)
	res := result(t, j)
	loc := res.Local
	if res.Mode != config.ModeDirect || loc == nil || j.Worktree != "" || j.Branch != "" || j.LocalPath != e.local {
		t.Fatalf("direct job: %+v %s", j, j.Result)
	}
	if loc.Outcome != OutcomeFixedLocal || len(loc.Commits) != 1 || !loc.FixesRef || !loc.Commits[0].Fixes ||
		loc.StartSHA != start || loc.Branch != "main" || loc.Pushed || loc.HeadSHA != run(t, e.local, "rev-parse", "HEAD") {
		t.Fatalf("facts: %+v", loc)
	}
	if res.Agent == nil || res.Agent.Status != "fixed" || len(res.Agent.Commits) != 1 || res.Agent.Commits[0] != loc.HeadSHA {
		t.Fatalf("agent claim: %+v", res.Agent)
	}
	joined := strings.Join(loc.DirtyBefore, "|")
	if !strings.Contains(joined, "README.md") || !strings.Contains(joined, "wip.txt") ||
		strings.Join(loc.DirtyAfter, "|") != joined {
		t.Fatalf("dirty before %v after %v", loc.DirtyBefore, loc.DirtyAfter)
	}
	if msg := run(t, e.local, "log", "-1", "--format=%B"); !strings.Contains(msg, "Fixes #1") {
		t.Fatalf("commit message %q", msg)
	}
	if diff, _ := ReadDiff(e.data, j.ID, 1); !strings.Contains(diff, "+fixed 1") || strings.Contains(diff, "wip") {
		t.Fatalf("stored diff: %q", diff)
	}
	// The user's own CLI settings stay on (no isolation flags); our prompt is appended.
	var rec struct {
		Args  []string `json:"args"`
		Stdin string   `json:"stdin"`
	}
	b, _ := os.ReadFile(e.record)
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(rec.Args, " ")
	for _, bad := range []string{"--setting-sources", "--safe-mode", "--bare"} {
		if strings.Contains(args, bad) {
			t.Fatalf("isolation flag %s in %v", bad, rec.Args)
		}
	}
	if !strings.Contains(args, "--append-system-prompt-file") || strings.Contains(rec.Stdin, "untrusted-issue-content") {
		t.Fatalf("args %v stdin %q", rec.Args, rec.Stdin)
	}
	sys, _ := os.ReadFile(filepath.Join(jobDir(e.data, j.ID), "fix-direct.system.md"))
	for _, want := range []string{"</untrusted-issue-content>", "[tag removed]", `"Fixes #1"`, "Do NOT push", "wip.txt", "Project note " + e.local} {
		if !strings.Contains(string(sys), want) {
			t.Fatalf("appended prompt misses %q:\n%s", want, sys)
		}
	}
	e.waitCards(1)

	// Push: the commit reaches the remote branch; the job is done, the folder kept.
	j, err := e.r.Push(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res := result(t, j); j.State != store.JobDone || !res.Local.Pushed || res.Local.Outcome != OutcomePushed {
		t.Fatalf("after push: %+v %s", j, j.Result)
	}
	if got := run(t, e.bare, "rev-parse", "main"); got != loc.HeadSHA {
		t.Fatalf("remote main %s, want %s", got, loc.HeadSHA)
	}
	if !exists(filepath.Join(e.local, "wip.txt")) {
		t.Fatal("push touched the working tree")
	}
	if _, err := e.r.Push(t.Context(), j.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("second push: %v", err)
	}
	// Sync sees the issue closed (the pushed "Fixes #1"): outcome closed.
	e.closeIssue(1)
	e.waitResult(j.ID, func(j store.Job, r Result) bool {
		return j.State == store.JobDone && r.Local.Closed && r.Local.Outcome == OutcomeClosed
	})
}

// Push goes to the project's own URL with the token, never to whatever origin
// points at now (it may have been re-pointed after the run).
func TestDirectPushIgnoresRepointedOrigin(t *testing.T) {
	mode(t, "ok")
	e := setup(t, 1, nil)
	j := e.wait(e.enqueue("fix", e.items[0])[0].ID, store.JobNeedsReview)
	head := result(t, j).Local.HeadSHA
	evil := filepath.Join(t.TempDir(), "evil.git")
	run(t, e.local, "init", "-q", "--bare", "-b", "main", evil)
	run(t, e.local, "remote", "set-url", "origin", evil)
	if _, err := e.r.Push(t.Context(), j.ID); err != nil {
		t.Fatal(err)
	}
	if got := run(t, e.bare, "rev-parse", "main"); got != head {
		t.Fatalf("project remote main %s, want %s", got, head)
	}
	if refs := run(t, evil, "for-each-ref"); refs != "" {
		t.Fatalf("push reached the re-pointed origin: %s", refs)
	}
}

// Facts win over the claim: a commit without "Fixes #N" is flagged. An issue
// closed while the commit is only local keeps the job under review (Push
// stays possible); pushing then ends it.
func TestDirectNoFixesRefAndClose(t *testing.T) {
	mode(t, "nofixes")
	e := setup(t, 1, nil)
	j := e.wait(e.enqueue("fix", e.items[0])[0].ID, store.JobNeedsReview)
	if loc := result(t, j).Local; loc.FixesRef || len(loc.Commits) != 1 || loc.Outcome != OutcomeFixedLocal || len(loc.DirtyBefore) != 0 {
		t.Fatalf("facts: %+v", loc)
	}
	e.closeIssue(1)
	j = e.waitResult(j.ID, func(j store.Job, r Result) bool { return r.Local.Closed })
	if r := result(t, j); j.State != store.JobNeedsReview || r.Local.Pushed || r.Local.Outcome != OutcomeFixedLocal {
		t.Fatalf("closed with a local-only commit: %s %+v", j.State, r.Local)
	}
	j, err := e.r.Push(t.Context(), j.ID)
	if err != nil || j.State != store.JobDone || !result(t, j).Local.Pushed {
		t.Fatalf("push after close: %+v %v", j, err)
	}
}

// An issue closed after the owner pushed the commit by hand ends the job.
func TestDirectPushedByHandThenClosed(t *testing.T) {
	mode(t, "ok")
	e := setup(t, 1, nil)
	j := e.wait(e.enqueue("fix", e.items[0])[0].ID, store.JobNeedsReview)
	run(t, e.local, "push", "-q", "origin", "main")
	e.closeIssue(1)
	j = e.waitResult(j.ID, func(j store.Job, r Result) bool { return j.State == store.JobDone })
	if r := result(t, j); r.Local.Outcome != OutcomeClosed || !r.Local.Pushed || !r.Local.Closed || j.FinishedAt == "" {
		t.Fatalf("closed: %+v", r.Local)
	}
}

func TestDirectNotReproduced(t *testing.T) {
	mode(t, "noop")
	e := setup(t, 1, nil)
	j := e.wait(e.enqueue("fix", e.items[0])[0].ID, store.JobNeedsReview)
	res := result(t, j)
	if res.Local.Outcome != OutcomeNotReproduced || len(res.Local.Commits) != 0 || res.Diff != nil {
		t.Fatalf("noop: %+v", res.Local)
	}
	if _, err := e.r.Push(t.Context(), j.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("push without commits: %v", err)
	}
	// Dismiss leaves the folder alone.
	head := run(t, e.local, "rev-parse", "HEAD")
	if j, err := e.r.Dismiss(t.Context(), j.ID); err != nil || j.State != store.JobCancelled {
		t.Fatalf("dismiss: %+v %v", j, err)
	}
	if run(t, e.local, "rev-parse", "HEAD") != head {
		t.Fatal("dismiss changed the folder")
	}
}

// One job per folder: a second project mapped to the same folder waits.
func TestDirectOneJobPerFolder(t *testing.T) {
	mode(t, "hang")
	e := setup(t, 1, func(s *config.Settings) { s.Agents.MaxParallel = 4; s.Agents.Profiles[0].MaxParallel = 4 })
	if err := e.st.SetLocalPath(t.Context(), e.proj[1].ID, e.local+string(filepath.Separator)); err != nil {
		t.Fatal(err)
	}
	jobs := e.enqueue("fix", e.items[0], e.items[1])
	e.waitRunning(1)
	if j, _ := e.st.Job(t.Context(), jobs[1].ID); j.State != store.JobQueued {
		t.Fatalf("second job in the same folder started: %+v", j)
	}
	if _, err := e.r.Cancel(t.Context(), jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	e.wait(jobs[0].ID, store.JobCancelled)
	// Now it runs (and fails: the folder's remote is octo/demo, not octo/other).
	if j := e.wait(jobs[1].ID, store.JobFailed); result(t, j).ErrorCode != CodeNoFolder {
		t.Fatalf("second job: %+v", j)
	}
}

// A CLI that exits normally but leaves a child running (an MCP server holding
// its stdout): the run ends at once and the child dies with the job.
func TestNormalExitKillsGrandchild(t *testing.T) {
	mode(t, "spawn")
	e := setup(t, 1, nil)
	begin := time.Now()
	j := e.wait(e.enqueue("fix", e.items[0])[0].ID, store.JobNeedsReview)
	if d := time.Since(begin); d > 20*time.Second {
		t.Fatalf("run took %s: the straggler held the pipes", d)
	}
	b, err := os.ReadFile(e.record + ".pid")
	if err != nil {
		t.Fatal(err)
	}
	child, _ := strconv.Atoi(string(b))
	if child == 0 || alive(child) {
		t.Fatalf("grandchild %d still alive after the job ended", child)
	}
	if exists(filepath.Join(jobDir(e.data, j.ID), procsFile)) {
		t.Fatal("process record kept after the tree ended")
	}
	if steps, _ := ReadLog(e.data, j.ID, 1); strings.Contains(stepsText(steps), "still alive") {
		t.Fatalf("leftover processes reported: %v", steps)
	}
}

func stepsText(steps []Step) string {
	var b strings.Builder
	for _, s := range steps {
		b.WriteString(s.Text + "\n")
	}
	return b.String()
}

// A crash left a recorded tree running: the next start kills it, but not a
// process whose pid is recorded with another creation time (pid reuse).
func TestStartKillsOrphans(t *testing.T) {
	mode(t, "ok")
	e := setup(t, 1, nil)
	start := func() *exec.Cmd {
		cmd := exec.Command(fakeExe) //nolint:noctx // test: the fake CLI as a sleeper
		cmd.Env = append(os.Environ(), "FAKECLI_MODE=sleep")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd
	}
	orphan, reused := start(), start()
	created := func(c *exec.Cmd) int64 {
		v, ok := processCreated(uint32(c.Process.Pid)) //nolint:gosec // G115: test pid
		if !ok {
			t.Fatal("creation time")
		}
		return v
	}
	dir := jobDir(e.data, 77)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	recs, _ := json.Marshal([]procRecord{
		{PID: uint32(orphan.Process.Pid), Created: created(orphan)},     //nolint:gosec // G115: test pid
		{PID: uint32(reused.Process.Pid), Created: created(reused) - 1}, //nolint:gosec // G115: test pid
	})
	writeFile(t, filepath.Join(dir, procsFile), string(recs))

	r2 := New(Options{Store: e.st, DataDir: e.data, Settings: config.Defaults})
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); r2.Wait() }()
	if err := r2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(5 * time.Second); alive(orphan.Process.Pid); {
		if time.Now().After(end) {
			t.Fatal("orphan survived the start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !alive(reused.Process.Pid) {
		t.Fatal("a process with a reused pid was killed")
	}
	if exists(filepath.Join(dir, procsFile)) {
		t.Fatal("orphan record kept")
	}
}
