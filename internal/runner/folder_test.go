package runner

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/store"
)

// A fix in a mapped folder that is not a clone of the project runs in place:
// outcome changed_folder with the changed files, no commit, push and PR refused.
func TestFixInFolderWithoutGit(t *testing.T) {
	mode(t, "ok")
	e := setup(t, 2, nil)
	other := e.proj[1] // octo/other: items e.items[2], e.items[3]

	// A missing folder is still refused up front.
	if err := e.st.SetLocalPath(t.Context(), other.ID, filepath.Join(t.TempDir(), "gone")); err != nil {
		t.Fatal(err)
	}
	if q, err := e.r.Enqueue(t.Context(), []int64{e.items[2]}, "fix", ""); !errors.Is(err, ErrNoFolder) || q[0].Error != CodeNoFolder {
		t.Fatalf("missing folder: %+v %v", q, err)
	}

	plain := t.TempDir()
	for name, body := range map[string]string{"obsolete.txt": "old\n", "keep.txt": "keep\n", "node_modules/dep/x.js": "x\n"} {
		p := filepath.Join(plain, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.st.SetLocalPath(t.Context(), other.ID, plain); err != nil {
		t.Fatal(err)
	}
	chunk, err := e.st.Issues(t.Context(), store.IssueFilter{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range chunk.Items {
		if it.ID == e.items[2] && (!it.Fixable || it.Git || it.Folder != plain) {
			t.Fatalf("fix target of a plain folder: %+v", it.Fix)
		}
		if it.ID == e.items[0] && (!it.Fixable || !it.Git) {
			t.Fatalf("fix target of a clone: %+v", it.Fix)
		}
	}

	j := e.wait(e.enqueue(flowFix, e.items[2])[0].ID, store.JobDone)
	res := result(t, j)
	if res.Mode != ModeFolder || res.Local == nil || res.Local.Outcome != OutcomeChangedFolder ||
		!slices.Equal(res.Local.Changed, []string{"A fixed.txt", "D obsolete.txt"}) || len(res.Local.Commits) != 0 {
		t.Fatalf("folder result: %+v %+v", res.Mode, res.Local)
	}
	if res.Agent == nil || res.Agent.Status != "fixed" || !slices.Equal(res.Agent.Files, []string{"fixed.txt"}) {
		t.Fatalf("agent: %+v", res.Agent)
	}
	if exists(filepath.Join(plain, ".git")) || j.Worktree != "" || j.Branch != "" {
		t.Fatalf("git used in a plain folder: %+v", j)
	}
	sys, err := os.ReadFile(filepath.Join(jobDir(e.data, j.ID), flowFixFolder+".system.md"))
	if err != nil || !strings.Contains(string(sys), "NOT a git clone") || strings.Contains(string(sys), "Fixes #") {
		t.Fatalf("folder prompt: %s %v", sys, err)
	}
	if _, err := e.r.Push(t.Context(), j.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("push of a folder fix: %v", err)
	}
	if _, err := e.r.CreatePR(t.Context(), j.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("PR of a folder fix: %v", err)
	}

	// A clone of another repository: in place too, and nothing is committed there.
	head := run(t, e.local, "rev-parse", "HEAD")
	if err := e.st.SetLocalPath(t.Context(), other.ID, e.local); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetLocalPath(t.Context(), e.proj[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	j = e.wait(e.enqueue(flowFix, e.items[3])[0].ID, store.JobDone)
	if res := result(t, j); res.Mode != ModeFolder || res.Local == nil || !slices.Equal(res.Local.Changed, []string{"A fixed.txt"}) {
		t.Fatalf("mismatch folder result: %+v", res.Local)
	}
	if now := run(t, e.local, "rev-parse", "HEAD"); now != head {
		t.Fatalf("a commit was made in a foreign clone: %s → %s", head, now)
	}
}

func TestChangedFilesBounded(t *testing.T) {
	before := folderSnapshot{files: map[string]fileStamp{"a": {1, 1}, "b": {1, 1}, "c": {1, 1}}}
	after := folderSnapshot{files: map[string]fileStamp{"a": {1, 1}, "b": {2, 1}, "d": {1, 1}}}
	if got := changedFiles(before, after); !slices.Equal(got, []string{"M b", "D c", "A d"}) {
		t.Fatalf("changed: %v", got)
	}
	after.truncated = true // a partial walk never reports deletions
	if got := changedFiles(before, after); !slices.Equal(got, []string{"M b", "A d"}) {
		t.Fatalf("truncated: %v", got)
	}
	big := folderSnapshot{files: map[string]fileStamp{}}
	for i := range maxChangedFiles + 3 {
		big.files[strings.Repeat("x", i+1)] = fileStamp{}
	}
	if got := changedFiles(folderSnapshot{files: map[string]fileStamp{}}, big); len(got) != maxChangedFiles+1 || got[maxChangedFiles] != "… 3 more" {
		t.Fatalf("bounded: %d %q", len(got), got[len(got)-1])
	}
}
