package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Git runs git commands. dir is the working directory; env entries are added
// to a scrubbed environment (no inherited GIT_* overrides).
type Git interface {
	Run(ctx context.Context, dir string, env []string, args ...string) (string, error)
}

// ExecGit is the default Git: the git executable on PATH (Path overrides).
type ExecGit struct {
	Path    string
	Timeout time.Duration // per command; 0 = 5 min
}

// Run implements Git; it returns trimmed stdout or an error carrying stderr.
func (g ExecGit) Run(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	exe := g.Path
	if exe == "" {
		exe = "git"
	}
	to := g.Timeout
	if to <= 0 {
		to = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	full := append([]string{"-c", "core.longpaths=true", "-c", "core.quotepath=false", "-c", "gc.auto=0",
		"-c", "maintenance.auto=false"}, args...)
	cmd := exec.CommandContext(ctx, exe, full...) //nolint:gosec // G204: fixed git binary, engine-built args
	cmd.Dir = dir
	cmd.Env = append(gitBaseEnv(), env...)
	cmd.WaitDelay = 5 * time.Second
	hide(cmd)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if len(msg) > 1500 {
			msg = msg[:1500] + "…"
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return strings.TrimRight(out.String(), "\r\n"), nil
}

// gitBaseEnv is the process environment minus git overrides and tokens.
func gitBaseEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		ku := strings.ToUpper(k)
		if strings.HasPrefix(ku, "GIT_") || ku == "GH_TOKEN" || ku == "GITHUB_TOKEN" || ku == "SSH_ASKPASS" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
}

// mirror is the app-owned bare repository a release pushes, tags and builds
// from (data\mirrors\<owner>_<repo>.git), never the mapped folder with its
// hooks and config.
type mirror struct {
	git   Git
	dir   string // bare repository
	hooks string // empty hooks directory
	cfg   string // app-owned global git config
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func newMirror(g Git, dataDir, repo string) *mirror {
	name := unsafeName.ReplaceAllString(strings.ReplaceAll(repo, "/", "_"), "-")
	base := filepath.Join(dataDir, "mirrors")
	return &mirror{git: g, dir: filepath.Join(base, name+".git"), hooks: filepath.Join(base, "empty-hooks"), cfg: filepath.Join(base, "gitconfig")}
}

// env isolates git from the system / global config and credential helpers.
func (m *mirror) env(extra ...string) []string {
	return append([]string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + m.cfg}, extra...)
}

// run runs git in the mirror with hooks off; token env (GIT_CONFIG_COUNT…) may be in extra.
func (m *mirror) run(ctx context.Context, extra []string, args ...string) (string, error) {
	full := append([]string{"-c", "core.hooksPath=" + m.hooks, "-c", "credential.helper=", "-c", "protocol.file.allow=always"}, args...)
	return m.git.Run(ctx, m.dir, m.env(extra...), full...)
}

func (m *mirror) ensure(ctx context.Context) error {
	if err := os.MkdirAll(m.hooks, 0o750); err != nil {
		return err
	}
	if _, err := os.Stat(m.cfg); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(m.cfg, []byte("# IssueWatcher release mirror: isolated git config\n"), 0o600); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(m.dir, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(m.dir, 0o750); err != nil {
		return err
	}
	_, err := m.git.Run(ctx, m.dir, m.env(), "init", "-q", "--bare", m.dir)
	return err
}

// fetchFolder copies branch of the mapped folder into refs/iw/folder and
// checks that sha arrived and is on it (objects verified by fsck on receive).
func (m *mirror) fetchFolder(ctx context.Context, folder, branch, sha string) error {
	if _, err := m.run(ctx, nil, "-c", "transfer.fsckObjects=true", "fetch", "-q", "--no-tags", folder,
		"+refs/heads/"+branch+":refs/iw/folder"); err != nil {
		return err
	}
	if _, err := m.run(ctx, nil, "cat-file", "-e", sha+"^{commit}"); err != nil {
		return fmt.Errorf("commit %s is not in the folder's %s any more", short(sha), branch)
	}
	return nil
}

// fetchRemote updates refs/remotes/origin/<branch> from url; it returns the remote head.
func (m *mirror) fetchRemote(ctx context.Context, url string, tokEnv []string, branch string) (string, error) {
	if _, err := m.run(ctx, tokEnv, "fetch", "-q", "--no-tags", url, "+refs/heads/"+branch+":refs/remotes/origin/"+branch); err != nil {
		return "", err
	}
	return m.run(ctx, nil, "rev-parse", "refs/remotes/origin/"+branch)
}

// isAncestor reports whether a is an ancestor of b (or equal); err when git
// could not tell (missing objects).
func (m *mirror) isAncestor(ctx context.Context, a, b string) (bool, error) {
	_, err := m.run(ctx, nil, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// remoteTag returns the sha the remote tag points at, peeled to its commit; "" = absent.
func (m *mirror) remoteTag(ctx context.Context, url string, tokEnv []string, tag string) (string, error) {
	out, err := m.run(ctx, tokEnv, "ls-remote", url, "refs/tags/"+tag, "refs/tags/"+tag+"^{}")
	if err != nil {
		return "", err
	}
	direct, peeled := "", ""
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		switch f[1] {
		case "refs/tags/" + tag + "^{}":
			peeled = f[0]
		case "refs/tags/" + tag:
			direct = f[0]
		}
	}
	if peeled != "" {
		return peeled, nil
	}
	return direct, nil // lightweight tag (or absent: "")
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}
