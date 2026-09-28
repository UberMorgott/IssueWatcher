// Package folders links projects to local git clones: it reads a clone's
// remotes from .git/config, checks a mapped folder (status) and scans root
// folders for clones whose remote matches a synced project (discovery). It only
// reads the file system; the user confirms every mapping.
package folders

import (
	"bufio"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Status of a mapped folder.
type Status string

// Folder states.
const (
	StatusNone     Status = "none"     // not mapped
	StatusOK       Status = "ok"       // git clone whose remote is the project
	StatusMissing  Status = "missing"  // folder does not exist
	StatusNotGit   Status = "notGit"   // exists, no .git
	StatusMismatch Status = "mismatch" // git clone of something else
)

// RepoKey normalises a git remote or web URL to host/owner/repo (lower case):
// https://github.com/O/R(.git), git@github.com:O/R.git, ssh://git@github.com/O/R.
// "" when it is not recognisable.
func RepoKey(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	} else if at := strings.Index(u, "@"); at >= 0 && strings.Contains(u[at:], ":") {
		u = strings.Replace(u[at+1:], ":", "/", 1) // scp-like git@host:owner/repo
	}
	if at := strings.LastIndex(u, "@"); at >= 0 && at < strings.Index(u+"/", "/") {
		u = u[at+1:] // user@host
	}
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	parts := strings.Split(u, "/")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ""
	}
	host := parts[0]
	if h, _, ok := strings.Cut(host, ":"); ok { // host:port
		host = h
	}
	return strings.ToLower(host + "/" + parts[1] + "/" + parts[2])
}

// Remotes returns the remote URLs of the git clone at dir (nil when dir is not one).
func Remotes(dir string) ([]string, bool) {
	gitDir := filepath.Join(dir, ".git")
	st, err := os.Stat(gitDir)
	if err != nil {
		return nil, false
	}
	if !st.IsDir() { // worktree / submodule: "gitdir: <path>"
		b, err := os.ReadFile(gitDir) //nolint:gosec // G304: reading the user's own clone
		if err != nil {
			return nil, false
		}
		p, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
		if !ok {
			return nil, false
		}
		gitDir = strings.TrimSpace(p)
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(dir, gitDir)
		}
		if common, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil { //nolint:gosec // G304: user's clone
			c := strings.TrimSpace(string(common))
			if !filepath.IsAbs(c) {
				c = filepath.Join(gitDir, c)
			}
			gitDir = c
		}
	}
	f, err := os.Open(filepath.Join(gitDir, "config")) //nolint:gosec // G304: user's clone
	if err != nil {
		return nil, true
	}
	defer func() { _ = f.Close() }()
	var urls []string
	inRemote := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			inRemote = strings.HasPrefix(line, "[remote ")
			continue
		}
		if !inRemote {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), "url") {
			urls = append(urls, strings.Trim(strings.TrimSpace(v), `"`))
		}
	}
	return urls, true
}

// Check reports the state of folder dir mapped to the project at projectURL.
func Check(dir, projectURL string) Status {
	if dir == "" {
		return StatusNone
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return StatusMissing
	}
	remotes, isGit := Remotes(dir)
	if !isGit {
		return StatusNotGit
	}
	want := RepoKey(projectURL)
	for _, r := range remotes {
		if RepoKey(r) == want && want != "" {
			return StatusOK
		}
	}
	return StatusMismatch
}

// Project is what discovery matches against.
type Project struct {
	ID   int64
	Name string
	URL  string
}

// Suggestion proposes folder Path for project ProjectID.
type Suggestion struct {
	ProjectID int64  `json:"projectId"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Remote    string `json:"remote"`
}

// maxDirs bounds one scan (a root pointed at C:\ must not run for minutes).
const maxDirs = 20000

// Discover walks roots (depth levels deep, skipping exclude names and hidden
// folders) and returns clones whose remote matches a project, one per project
// (the shortest path wins), plus how many folders it visited.
func Discover(ctx context.Context, roots []string, depth int, exclude []string, projects []Project) ([]Suggestion, int, error) {
	byKey := map[string]Project{}
	for _, p := range projects {
		if k := RepoKey(p.URL); k != "" {
			byKey[k] = p
		}
	}
	best := map[int64]Suggestion{}
	visited := 0
	errStop := errors.New("stop")
	for _, root := range roots {
		root = filepath.Clean(root)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if path == root {
					return nil // missing root: nothing to scan
				}
				return fs.SkipDir
			}
			if !d.IsDir() {
				return nil
			}
			if ctx.Err() != nil || visited >= maxDirs {
				return errStop
			}
			visited++
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || slices.ContainsFunc(exclude, func(x string) bool { return strings.EqualFold(x, name) })) {
				return fs.SkipDir
			}
			if remotes, ok := Remotes(path); ok {
				for _, r := range remotes {
					p, hit := byKey[RepoKey(r)]
					if !hit {
						continue
					}
					if cur, seen := best[p.ID]; !seen || len(path) < len(cur.Path) {
						best[p.ID] = Suggestion{ProjectID: p.ID, Name: p.Name, Path: path, Remote: r}
					}
				}
				return fs.SkipDir // a clone's subfolders are its own files
			}
			if rel, err := filepath.Rel(root, path); err == nil && rel != "." && strings.Count(rel, string(filepath.Separator))+1 >= depth {
				return fs.SkipDir
			}
			return nil
		})
		if err != nil && !errors.Is(err, errStop) {
			return nil, visited, err
		}
		if ctx.Err() != nil {
			return nil, visited, ctx.Err()
		}
	}
	out := make([]Suggestion, 0, len(best))
	for _, s := range best {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Suggestion) int { return strings.Compare(a.Name, b.Name) })
	return out, visited, nil
}
