package folders

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRepoKey(t *testing.T) {
	want := "github.com/ubermorgott/issuewatcher"
	for _, u := range []string{
		"https://github.com/UberMorgott/IssueWatcher",
		"https://github.com/UberMorgott/IssueWatcher.git",
		"https://github.com/UberMorgott/IssueWatcher/",
		"git@github.com:UberMorgott/IssueWatcher.git",
		"ssh://git@github.com/UberMorgott/IssueWatcher.git",
		"ssh://git@github.com:22/UberMorgott/IssueWatcher",
		"https://token@github.com/UberMorgott/IssueWatcher",
	} {
		if got := RepoKey(u); got != want {
			t.Errorf("RepoKey(%q) = %q", u, got)
		}
	}
	for _, u := range []string{"", "github.com", "https://github.com/owner"} {
		if got := RepoKey(u); got != "" {
			t.Errorf("RepoKey(%q) = %q, want empty", u, got)
		}
	}
}

// clone makes dir look like a git clone with the given remote.
func clone(t *testing.T, dir, remote string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	cfg := "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = " + remote + "\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheck(t *testing.T) {
	root := t.TempDir()
	const url = "https://github.com/o/r"
	ok := filepath.Join(root, "r")
	clone(t, ok, "git@github.com:o/r.git")
	other := filepath.Join(root, "x")
	clone(t, other, "https://github.com/o/x")
	plain := filepath.Join(root, "plain")
	if err := os.MkdirAll(plain, 0o750); err != nil {
		t.Fatal(err)
	}
	// Linked worktree: .git is a file pointing into the main clone.
	wt := filepath.Join(root, "wt")
	gitdir := filepath.Join(ok, ".git", "worktrees", "wt")
	if err := os.MkdirAll(gitdir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitdir, "commondir"), []byte("../.."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]Status{
		"":                             StatusNone,
		ok:                             StatusOK,
		wt:                             StatusOK,
		other:                          StatusMismatch,
		plain:                          StatusNotGit,
		filepath.Join(root, "missing"): StatusMissing,
	} {
		if got := Check(dir, url); got != want {
			t.Errorf("Check(%q) = %s, want %s", dir, got, want)
		}
	}
}

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	clone(t, filepath.Join(root, "a", "r1"), "https://github.com/o/r1.git")
	clone(t, filepath.Join(root, "deep", "x", "y", "z", "r2"), "git@github.com:o/r2.git") // below depth 3
	clone(t, filepath.Join(root, "node_modules", "r3"), "https://github.com/o/r3")        // excluded
	clone(t, filepath.Join(root, "r3"), "https://github.com/o/r3")
	clone(t, filepath.Join(root, "r3", "sub", "r1"), "https://github.com/o/r1") // inside a clone: not entered
	clone(t, filepath.Join(root, "unrelated"), "https://github.com/o/other")
	projects := []Project{{1, "o/r1", "https://github.com/o/r1"}, {2, "o/r2", "https://github.com/o/r2"}, {3, "o/r3", "https://github.com/o/r3"}}
	got, visited, err := Discover(context.Background(), []string{root, filepath.Join(root, "nope")}, 3, []string{"node_modules"}, projects)
	if err != nil {
		t.Fatal(err)
	}
	if visited == 0 {
		t.Fatal("nothing visited")
	}
	byID := map[int64]string{}
	for _, s := range got {
		byID[s.ProjectID] = s.Path
	}
	if byID[1] != filepath.Join(root, "a", "r1") || byID[3] != filepath.Join(root, "r3") || byID[2] != "" || len(got) != 2 {
		t.Fatalf("suggestions: %+v", got)
	}
}
