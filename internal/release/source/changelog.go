package source

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Changelog source kinds (ChangelogSource.Kind); the same strings as config.ChangelogProfile.
const (
	ChangelogFactorio       = "factorio"
	ChangelogKeepAChangelog = "keepachangelog"
	ChangelogCommits        = "commits"
)

// Default changelog files when ChangelogSource.Path is empty.
const (
	DefaultFactorioChangelog = "changelog.txt"
	DefaultKeepAChangelog    = "CHANGELOG.md"
)

// ChangelogSource is where release notes come from; its fields match
// config.ChangelogProfile, so one converts to the other.
type ChangelogSource struct {
	Kind string // factorio | keepachangelog | commits
	Path string // file relative to the folder; default by kind
}

// ErrNoChanges means there is no entry for the version and nothing to write one from.
var ErrNoChanges = errors.New("source: no changelog entry and no commits to build one")

// factorioSeparator is the line Factorio's changelog parser requires between entries (99 dashes).
var factorioSeparator = strings.Repeat("-", 99)

// Entry returns the changelog text of version (the release body). An entry the
// file already has is used as is; otherwise one is written from the commit
// subjects (factorio: new entry at the top; keepachangelog: [Unreleased]
// promoted or a new section after the header). commits writes no file.
// changed lists the files written, relative to dir.
func Entry(dir string, src ChangelogSource, version string, subjects []string, date time.Time) (text string, changed []string, err error) {
	if _, err := Parse(version); err != nil {
		return "", nil, err
	}
	subjects = cleanSubjects(subjects)
	switch src.Kind {
	case ChangelogCommits:
		if len(subjects) == 0 {
			return "", nil, ErrNoChanges
		}
		return bullets("- ", subjects), nil, nil
	case ChangelogFactorio:
		return fileEntry(dir, orDefault(src.Path, DefaultFactorioChangelog), func(body string) (string, string, error) {
			return factorioEntry(body, version, subjects, date)
		})
	case ChangelogKeepAChangelog:
		return fileEntry(dir, orDefault(src.Path, DefaultKeepAChangelog), func(body string) (string, string, error) {
			return keepAChangelogEntry(body, version, subjects, date)
		})
	}
	return "", nil, fmt.Errorf("source: unknown changelog kind %q", src.Kind)
}

// fileEntry runs edit over the file's text with "\n" line ends (a missing file
// is empty) and writes the result back in the file's own line ends and BOM.
func fileEntry(dir, rel string, edit func(body string) (entry, updated string, err error)) (string, []string, error) {
	p, err := resolve(dir, rel)
	if err != nil {
		return "", nil, err
	}
	raw, err := os.ReadFile(p) //nolint:gosec // G304: a configured file inside the project folder
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", nil, fmt.Errorf("source: %w", err)
	}
	bom := bytes.HasPrefix(raw, utf8BOM)
	raw = bytes.TrimPrefix(raw, utf8BOM)
	crlf := bytes.Contains(raw, []byte("\r\n"))
	body := strings.ReplaceAll(string(raw), "\r\n", "\n")
	entry, updated, err := edit(body)
	if err != nil {
		return "", nil, err
	}
	if updated == body {
		return entry, nil, nil
	}
	if crlf {
		updated = strings.ReplaceAll(updated, "\n", "\r\n")
	}
	out := []byte(updated)
	if bom {
		out = append(append([]byte{}, utf8BOM...), out...)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return "", nil, fmt.Errorf("source: %w", err)
	}
	if err := writeFile(dir, rel, out); err != nil {
		return "", nil, err
	}
	return entry, []string{filepath.ToSlash(rel)}, nil
}

func factorioEntry(body, version string, subjects []string, date time.Time) (string, string, error) {
	lines := strings.Split(body, "\n")
	isSep := func(l string) bool {
		t := strings.TrimRight(l, " \t")
		return len(t) >= 3 && strings.Trim(t, "-") == ""
	}
	sep := factorioSeparator
	for i := 0; i < len(lines); i++ {
		if !isSep(lines[i]) {
			continue
		}
		sep = strings.TrimRight(lines[i], " \t")
		j := i + 1
		for j < len(lines) && !isSep(lines[j]) {
			j++
		}
		block := lines[i+1 : j]
		for _, l := range block {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "Version:"); ok && sameVersion(v, version) {
				return trimBlock(block), body, nil
			}
		}
		i = j - 1
	}
	if len(subjects) == 0 {
		return "", "", ErrNoChanges
	}
	entry := "Version: " + version + "\nDate: " + date.Format(time.DateOnly) + "\n  Changes:\n" + bullets("    - ", subjects)
	rest := body
	if strings.TrimSpace(rest) == "" {
		rest = ""
	} else if first := strings.TrimLeft(rest, "\n"); !isSep(strings.SplitN(first, "\n", 2)[0]) {
		rest = sep + "\n" + rest // text above the first entry: keep it below ours, as an entry of its own
	} else {
		rest = first
	}
	return entry, sep + "\n" + entry + "\n" + rest, nil
}

var (
	kacHeading = regexp.MustCompile(`^##\s+\[?([^\]\s]+)\]?(.*)$`)
	linkRef    = regexp.MustCompile(`^\[[^\]]+\]:\s`)
)

func keepAChangelogEntry(body, version string, subjects []string, date time.Time) (string, string, error) {
	lines := strings.Split(body, "\n")
	unreleased, first := -1, -1
	for i, l := range lines {
		m := kacHeading.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if first < 0 {
			first = i
		}
		switch {
		case sameVersion(m[1], version):
			return sectionText(lines, i+1), body, nil
		case strings.EqualFold(m[1], "unreleased") && unreleased < 0:
			unreleased = i
		}
	}
	heading := "## [" + version + "] - " + date.Format(time.DateOnly)
	if unreleased >= 0 {
		text := sectionText(lines, unreleased+1)
		end := sectionEnd(lines, unreleased+1)
		out := append([]string{}, lines[:unreleased]...)
		out = append(out, "## [Unreleased]", "", heading)
		if text == "" {
			if len(subjects) == 0 {
				return "", "", ErrNoChanges
			}
			text = "### Changed\n\n" + bullets("- ", subjects)
			out = append(out, "", text, "")
			out = append(out, lines[end:]...)
		} else {
			out = append(out, lines[unreleased+1:]...)
		}
		return text, strings.Join(out, "\n"), nil
	}
	if len(subjects) == 0 {
		return "", "", ErrNoChanges
	}
	text := "### Changed\n\n" + bullets("- ", subjects)
	section := heading + "\n\n" + text + "\n"
	switch {
	case strings.TrimSpace(body) == "":
		return text, "# Changelog\n\n" + section, nil
	case first >= 0:
		out := append(append(append([]string{}, lines[:first]...), strings.Split(section, "\n")...), lines[first:]...)
		return text, strings.Join(out, "\n"), nil
	}
	return text, strings.TrimRight(body, "\n") + "\n\n" + section, nil
}

// sectionEnd is the index of the line after a section that starts at from: the
// next "## " heading, or the link references at the end of the file.
func sectionEnd(lines []string, from int) int {
	end := len(lines)
	for i := from; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			return i
		}
	}
	for end > from && (strings.TrimSpace(lines[end-1]) == "" || linkRef.MatchString(lines[end-1])) {
		end--
	}
	return end
}

func sectionText(lines []string, from int) string {
	return trimBlock(lines[from:sectionEnd(lines, from)])
}

// trimBlock joins lines without the blank lines around them.
func trimBlock(lines []string) string {
	return strings.Trim(strings.Join(lines, "\n"), "\n \t")
}

func bullets(prefix string, items []string) string {
	var b strings.Builder
	for _, s := range items {
		b.WriteString(prefix + s + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// cleanSubjects drops empty subjects and flattens line breaks.
func cleanSubjects(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.Join(strings.Fields(s), " "); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
