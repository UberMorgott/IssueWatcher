package source

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fixture copies testdata/name into a temp dir as file (LF line ends) and returns the dir.
func fixture(t *testing.T, name, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: test fixture
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	put(t, dir, file, strings.ReplaceAll(string(b), "\r\n", "\n"))
	return dir
}

func put(t *testing.T, dir, file, body string) {
	t.Helper()
	p := filepath.Join(dir, file)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, dir, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, file)) //nolint:gosec // G304: test temp dir
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSemver(t *testing.T) {
	ordered := []string{"0.9.9", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.2.0", "1.10.0", "2.0.0"}
	for i := range len(ordered) - 1 {
		if c, err := Compare(ordered[i], ordered[i+1]); err != nil || c != -1 {
			t.Errorf("%s < %s: %d %v", ordered[i], ordered[i+1], c, err)
		}
		if c, _ := Compare(ordered[i+1], ordered[i]); c != 1 {
			t.Errorf("%s > %s: %d", ordered[i+1], ordered[i], c)
		}
	}
	if c, err := Compare("v1.2.3+build.5", "1.2.3"); err != nil || c != 0 {
		t.Errorf("build ignored: %d %v", c, err)
	}
	if c, err := Compare("0.18.01", "0.18.1"); err != nil || c != 0 {
		t.Errorf("leading zero: %d %v", c, err)
	}
	for in, want := range map[string]string{"1.2.3": "1.2.4", "v0.9.99": "0.9.100", "2.0.0-rc.1": "2.0.0"} {
		if got, err := NextPatch(in); err != nil || got != want {
			t.Errorf("NextPatch(%s) = %s %v, want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "1.2", "1.2.3.4", "1.x.3", "1.2.-3", "1.2.3-", "1.2.3+", "a.b.c"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
	if v, _ := Parse("v1.2.3-rc.1+b"); v.String() != "1.2.3-rc.1+b" {
		t.Errorf("String: %s", v)
	}
}

func TestFactorioInfoVersion(t *testing.T) {
	dir := fixture(t, "info.json", "info.json")
	src := VersionSource{Kind: KindFactorioInfo}
	before := get(t, dir, "info.json")
	if v, err := CurrentVersion(dir, src); err != nil || v != "1.2.3" {
		t.Fatalf("current: %q %v", v, err)
	}
	if files, err := WriteVersion(dir, src, "1.2.3"); err != nil || files != nil {
		t.Fatalf("same version written: %v %v", files, err)
	}
	files, err := WriteVersion(dir, src, "1.2.4")
	if err != nil || !slices.Equal(files, []string{"info.json"}) {
		t.Fatalf("write: %v %v", files, err)
	}
	// Only the value changed: indentation, key order, other keys as before.
	if want := strings.Replace(before, `"version": "1.2.3"`, `"version": "1.2.4"`, 1); get(t, dir, "info.json") != want {
		t.Fatalf("formatting changed:\n%s", get(t, dir, "info.json"))
	}
	if v, _ := CurrentVersion(dir, src); v != "1.2.4" {
		t.Fatalf("read back: %s", v)
	}
	// CRLF, a BOM and a custom path survive.
	put(t, dir, "mod/info.json", "\xEF\xBB\xBF{\r\n\t\"version\" :\t\"0.1.0\",\r\n\t\"name\": \"x\"\r\n}\r\n")
	src.Path = "mod/info.json"
	if _, err := WriteVersion(dir, src, "0.1.1"); err != nil {
		t.Fatal(err)
	}
	if got := get(t, dir, "mod/info.json"); got != "\xEF\xBB\xBF{\r\n\t\"version\" :\t\"0.1.1\",\r\n\t\"name\": \"x\"\r\n}\r\n" {
		t.Fatalf("crlf/bom: %q", got)
	}
}

func TestJSONVersion(t *testing.T) {
	dir := fixture(t, "package.json", "package.json")
	src := VersionSource{Kind: KindJSON, Path: "package.json", Key: "meta.version"}
	before := get(t, dir, "package.json")
	if v, err := CurrentVersion(dir, src); err != nil || v != "0.4.1" {
		t.Fatalf("current: %q %v", v, err)
	}
	if _, err := WriteVersion(dir, src, "0.5.0"); err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(before, `"version": "0.4.1"`, `"version": "0.5.0"`, 1); get(t, dir, "package.json") != want {
		t.Fatalf("formatting changed:\n%s", get(t, dir, "package.json"))
	}
	for key, wantErr := range map[string]error{"meta.missing": ErrNoVersion, "meta.build": nil} {
		_, err := CurrentVersion(dir, VersionSource{Kind: KindJSON, Path: "package.json", Key: key})
		if err == nil || (wantErr != nil && !errors.Is(err, wantErr)) {
			t.Errorf("%s: %v", key, err)
		}
	}
	if _, err := CurrentVersion(dir, VersionSource{Kind: KindJSON, Path: "../outside.json", Key: "v"}); err == nil {
		t.Error("path outside the folder accepted")
	}
}

func TestRegexVersion(t *testing.T) {
	dir := fixture(t, "version_go.txt", "version.go")
	src := VersionSource{Kind: KindRegex, Path: "version.go", Pattern: `Version = "([^"]+)"`}
	before := get(t, dir, "version.go")
	if v, err := CurrentVersion(dir, src); err != nil || v != "2.0.9" {
		t.Fatalf("current: %q %v", v, err)
	}
	if _, err := WriteVersion(dir, src, "2.0.10"); err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(before, `"2.0.9"`, `"2.0.10"`, 1); get(t, dir, "version.go") != want {
		t.Fatalf("file:\n%s", get(t, dir, "version.go"))
	}
	if _, err := CurrentVersion(dir, VersionSource{Kind: KindRegex, Path: "version.go", Pattern: `(V)(x)`}); err == nil {
		t.Error("two groups accepted")
	}
	if _, err := CurrentVersion(dir, VersionSource{Kind: KindRegex, Path: "version.go", Pattern: `Nope = "(.+)"`}); !errors.Is(err, ErrNoVersion) {
		t.Errorf("no match: %v", err)
	}
}

func TestGitTagVersion(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", //nolint:gosec // G204: fixed test git call
			"-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	src := VersionSource{Kind: KindGitTag}
	git("commit", "-q", "--allow-empty", "-m", "init")
	if _, err := CurrentVersion(dir, src); !errors.Is(err, ErrNoVersion) {
		t.Fatalf("no tags: %v", err)
	}
	for _, tag := range []string{"v1.2.3", "v1.10.0", "v1.10.1-rc.1", "release-9.0.0", "vnext", "2.0.0"} {
		git("tag", tag)
	}
	if v, err := CurrentVersion(dir, src); err != nil || v != "1.10.1-rc.1" {
		t.Fatalf("latest: %q %v", v, err)
	}
	if files, err := WriteVersion(dir, src, "1.10.1"); err != nil || files != nil {
		t.Fatalf("git-tag writes nothing: %v %v", files, err)
	}
}

var day = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func TestFactorioChangelog(t *testing.T) {
	dir := fixture(t, "changelog.txt", "changelog.txt")
	before := get(t, dir, "changelog.txt")
	src := ChangelogSource{Kind: ChangelogFactorio}
	// An existing entry is used as is, the file untouched.
	text, files, err := Entry(dir, src, "1.2.2", []string{"ignored"}, day)
	if err != nil || files != nil || text != "Version: 1.2.2\nDate: 2026-08-01\n  Changes:\n    - First release." {
		t.Fatalf("existing: %q %v %v", text, files, err)
	}
	text, files, err = Entry(dir, src, "1.2.4", []string{"Fix the inserter crash", "  ", "Faster\nload"}, day)
	if err != nil || !slices.Equal(files, []string{"changelog.txt"}) {
		t.Fatalf("new: %v %v", files, err)
	}
	wantEntry := "Version: 1.2.4\nDate: 2026-10-05\n  Changes:\n    - Fix the inserter crash\n    - Faster load"
	if text != wantEntry || get(t, dir, "changelog.txt") != factorioSeparator+"\n"+wantEntry+"\n"+before {
		t.Fatalf("new entry: %q\n%s", text, get(t, dir, "changelog.txt"))
	}
	// Re-running for the same version reuses the entry it wrote.
	if again, files, err := Entry(dir, src, "1.2.4", nil, day); err != nil || files != nil || again != wantEntry {
		t.Fatalf("rerun: %q %v %v", again, files, err)
	}
	if _, _, err := Entry(dir, src, "1.2.5", nil, day); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("nothing to write: %v", err)
	}
	// A missing file is created; CRLF files keep CRLF.
	empty := t.TempDir()
	if _, files, err := Entry(empty, ChangelogSource{Kind: ChangelogFactorio, Path: "mod/changelog.txt"}, "0.1.0", []string{"First"}, day); err != nil ||
		!slices.Equal(files, []string{"mod/changelog.txt"}) ||
		get(t, empty, "mod/changelog.txt") != factorioSeparator+"\nVersion: 0.1.0\nDate: 2026-10-05\n  Changes:\n    - First\n" {
		t.Fatalf("created: %v %v %q", files, err, get(t, empty, "mod/changelog.txt"))
	}
	put(t, empty, "changelog.txt", strings.ReplaceAll(before, "\n", "\r\n"))
	if _, _, err := Entry(empty, src, "1.2.4", []string{"x"}, day); err != nil {
		t.Fatal(err)
	}
	if got := get(t, empty, "changelog.txt"); strings.Count(got, "\r\n") != strings.Count(got, "\n") || !strings.HasSuffix(got, strings.ReplaceAll(before, "\n", "\r\n")) {
		t.Fatalf("crlf: %q", got)
	}
}

func TestKeepAChangelogReuse(t *testing.T) {
	dir := fixture(t, "keepachangelog_plain.md", "CHANGELOG.md")
	text, files, err := Entry(dir, ChangelogSource{Kind: ChangelogKeepAChangelog}, "1.0.0", nil, day)
	if err != nil || files != nil || text != "- First release." {
		t.Fatalf("last section: %q %v %v", text, files, err)
	}
	if text, _, _ := Entry(dir, ChangelogSource{Kind: ChangelogKeepAChangelog}, "1.0.1", nil, day); text != "### Fixed\n\n- Typo." {
		t.Fatalf("middle section: %q", text)
	}
}

func TestKeepAChangelogInsert(t *testing.T) {
	dir := fixture(t, "keepachangelog_plain.md", "CHANGELOG.md")
	before := get(t, dir, "CHANGELOG.md")
	text, files, err := Entry(dir, ChangelogSource{Kind: ChangelogKeepAChangelog}, "1.0.2", []string{"Fix A", "Fix B"}, day)
	if err != nil || !slices.Equal(files, []string{"CHANGELOG.md"}) || text != "### Changed\n\n- Fix A\n- Fix B" {
		t.Fatalf("insert: %q %v %v", text, files, err)
	}
	want := strings.Replace(before, "## [1.0.1]", "## [1.0.2] - 2026-10-05\n\n### Changed\n\n- Fix A\n- Fix B\n\n## [1.0.1]", 1)
	if got := get(t, dir, "CHANGELOG.md"); got != want {
		t.Fatalf("file:\n%s", got)
	}
	// No file yet: created with a header.
	empty := t.TempDir()
	if _, _, err := Entry(empty, ChangelogSource{Kind: ChangelogKeepAChangelog, Path: "docs/CHANGES.md"}, "0.1.0", []string{"First"}, day); err != nil ||
		get(t, empty, "docs/CHANGES.md") != "# Changelog\n\n## [0.1.0] - 2026-10-05\n\n### Changed\n\n- First\n" {
		t.Fatalf("created: %v %q", err, get(t, empty, "docs/CHANGES.md"))
	}
}

func TestKeepAChangelogPromoteUnreleased(t *testing.T) {
	dir := fixture(t, "keepachangelog_unreleased.md", "CHANGELOG.md")
	before := get(t, dir, "CHANGELOG.md")
	text, files, err := Entry(dir, ChangelogSource{Kind: ChangelogKeepAChangelog}, "1.0.1", []string{"commit subjects are not used"}, day)
	if err != nil || !slices.Equal(files, []string{"CHANGELOG.md"}) || text != "### Fixed\n\n- Crash when the save is empty." {
		t.Fatalf("promote: %q %v %v", text, files, err)
	}
	want := strings.Replace(before, "## [Unreleased]\n", "## [Unreleased]\n\n## [1.0.1] - 2026-10-05\n", 1)
	if got := get(t, dir, "CHANGELOG.md"); got != want {
		t.Fatalf("file:\n%s", got)
	}
	// Now 1.0.1 exists: reused; the empty [Unreleased] is filled from commits for the next one.
	if again, files, _ := Entry(dir, ChangelogSource{Kind: ChangelogKeepAChangelog}, "1.0.1", nil, day); again != text || files != nil {
		t.Fatalf("reuse: %q %v", again, files)
	}
	text, _, err = Entry(dir, ChangelogSource{Kind: ChangelogKeepAChangelog}, "1.0.2", []string{"Fix C"}, day)
	if err != nil || text != "### Changed\n\n- Fix C" {
		t.Fatalf("empty unreleased: %q %v", text, err)
	}
	got := get(t, dir, "CHANGELOG.md")
	if !strings.Contains(got, "## [Unreleased]\n\n## [1.0.2] - 2026-10-05\n\n### Changed\n\n- Fix C\n\n## [1.0.1] - 2026-10-05\n") ||
		!strings.HasSuffix(got, "[1.0.0]: https://github.com/o/r/releases/tag/v1.0.0\n") {
		t.Fatalf("file:\n%s", got)
	}
}

func TestCommitsChangelog(t *testing.T) {
	dir := t.TempDir()
	text, files, err := Entry(dir, ChangelogSource{Kind: ChangelogCommits}, "1.0.0", []string{"fix: a", "", "feat: b"}, day)
	if err != nil || files != nil || text != "- fix: a\n- feat: b" {
		t.Fatalf("commits: %q %v %v", text, files, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("files written: %v", entries)
	}
	if _, _, err := Entry(dir, ChangelogSource{Kind: ChangelogCommits}, "1.0.0", nil, day); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("empty: %v", err)
	}
	if _, _, err := Entry(dir, ChangelogSource{Kind: "news"}, "1.0.0", []string{"x"}, day); err == nil {
		t.Fatal("unknown kind accepted")
	}
}
