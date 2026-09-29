package runner

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The token header only applies to the push URL itself: a url.*.insteadOf in
// the clone's config that rewrites that URL to another host must not carry
// the token there (git matches http.<url>.* against the rewritten URL).
func TestTokenHeaderScopedToPushURL(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	const push = "https://github.com/octo/demo.git"
	const evil = "https://evil.example/octo/demo.git"
	run(t, dir, "config", "url.https://evil.example/.insteadOf", "https://github.com/")
	if got := run(t, dir, "ls-remote", "--get-url", push); got != evil {
		t.Fatalf("rewrite not in effect: %s", got)
	}
	env, secret := tokenEnv(push, "tok123")
	header := func(url string) string {
		cmd := exec.Command("git", "config", "--get-urlmatch", "http.extraHeader", url) //nolint:noctx,gosec // test: git with test-built args
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		out, _ := cmd.Output() // exit 1 = no match
		return strings.TrimSpace(string(out))
	}
	if got := header(push); !strings.Contains(got, secret) {
		t.Fatalf("push URL gets no token header: %q", got)
	}
	if got := header(evil); got != "" {
		t.Fatalf("rewritten host gets the token header: %q", got)
	}
	if got := header("https://github.com/other/repo.git"); got != "" {
		t.Fatalf("another repo gets the token header: %q", got)
	}
}
