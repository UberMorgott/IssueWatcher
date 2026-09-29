package runner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A git command ended by its context takes its whole tree down: a pre-push
// hook (and what it spawned) must not outlive a cancelled push, nor hold its
// output pipes open so the call hangs.
func TestGitCancelKillsHookTree(t *testing.T) {
	root := t.TempDir()
	repo, bare, rec := filepath.Join(root, "repo"), filepath.Join(root, "bare.git"), filepath.Join(root, "hook")
	run(t, root, "init", "-q", "--bare", "-b", "main", bare)
	run(t, root, "init", "-q", "-b", "main", repo)
	run(t, repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init")
	hook := "#!/bin/sh\nFAKECLI_MODE=hang FAKECLI_RECORD='" + filepath.ToSlash(rec) + "' '" + filepath.ToSlash(fakeExe) + "'\n"
	writeFile(t, filepath.Join(repo, ".git", "hooks", "pre-push"), hook)

	r := New(Options{})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	begin := time.Now()
	if _, err := r.git(ctx, repo, nil, "push", bare, "HEAD:refs/heads/main"); err == nil {
		t.Fatal("push with a hanging hook succeeded")
	}
	if d := time.Since(begin); d > 20*time.Second {
		t.Fatalf("cancelled git took %s: the hook tree held its pipes", d)
	}
	b, err := os.ReadFile(rec + ".pid") //nolint:gosec // G304: test temp file
	if err != nil {
		t.Fatal(err)
	}
	child, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	for end := time.Now().Add(5 * time.Second); child == 0 || alive(child); {
		if time.Now().After(end) {
			t.Fatalf("hook grandchild %d still alive after the push was cancelled", child)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
