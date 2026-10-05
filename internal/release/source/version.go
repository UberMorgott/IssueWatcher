package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Version source kinds (VersionSource.Kind); the same strings as config.VersionProfile.
const (
	KindFactorioInfo = "factorio-info"
	KindJSON         = "json"
	KindRegex        = "regex"
	KindGitTag       = "git-tag"
)

// VersionSource is where a project's version lives; its fields match
// config.VersionProfile, so one converts to the other.
type VersionSource struct {
	Kind    string // factorio-info | json | regex | git-tag
	Path    string // file relative to the folder (factorio-info: default info.json)
	Key     string // json: dot path to the string value
	Pattern string // regex: exactly one capture group around the version
}

// ErrNoVersion means the source holds no version (no file value, no v* tag).
var ErrNoVersion = errors.New("source: no version found")

// DefaultInfoJSON is the factorio-info file when Path is empty.
const DefaultInfoJSON = "info.json"

// CurrentVersion reads the project's version from dir; the result is a valid
// semver without "v". git-tag: the highest v-prefixed semver tag.
func CurrentVersion(dir string, src VersionSource) (string, error) {
	var raw string
	switch src.Kind {
	case KindFactorioInfo, KindJSON:
		path, key := jsonTarget(src)
		body, err := readFile(dir, path)
		if err != nil {
			return "", err
		}
		_, _, raw, err = findJSONString(body, key)
		if err != nil {
			return "", fmt.Errorf("source: %s: %w", path, err)
		}
	case KindRegex:
		body, err := readFile(dir, src.Path)
		if err != nil {
			return "", err
		}
		re, err := versionPattern(src.Pattern)
		if err != nil {
			return "", err
		}
		m := re.FindSubmatch(body)
		if m == nil {
			return "", fmt.Errorf("%w: pattern does not match %s", ErrNoVersion, src.Path)
		}
		raw = string(m[1])
	case KindGitTag:
		return latestTag(dir)
	default:
		return "", fmt.Errorf("source: unknown version kind %q", src.Kind)
	}
	v, err := Parse(raw)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(strings.TrimSpace(raw), "v") {
		return v.String(), nil
	}
	return strings.TrimSpace(raw), nil
}

// WriteVersion stores version in dir's version source, changing only the value
// (formatting, key order and line endings kept). It returns the changed files
// (relative to dir); none when the value already matches or for git-tag.
func WriteVersion(dir string, src VersionSource, version string) ([]string, error) {
	if _, err := Parse(version); err != nil {
		return nil, err
	}
	var (
		path       string
		start, end int
		old        string
		body       []byte
		err        error
	)
	switch src.Kind {
	case KindFactorioInfo, KindJSON:
		var key []string
		path, key = jsonTarget(src)
		if body, err = readFile(dir, path); err != nil {
			return nil, err
		}
		if start, end, old, err = findJSONString(body, key); err != nil {
			return nil, fmt.Errorf("source: %s: %w", path, err)
		}
		start, end = start+1, end-1 // inside the quotes
	case KindRegex:
		path = src.Path
		if body, err = readFile(dir, path); err != nil {
			return nil, err
		}
		re, err := versionPattern(src.Pattern)
		if err != nil {
			return nil, err
		}
		m := re.FindSubmatchIndex(body)
		if m == nil {
			return nil, fmt.Errorf("%w: pattern does not match %s", ErrNoVersion, path)
		}
		start, end = m[2], m[3]
		old = string(body[start:end])
	case KindGitTag:
		return nil, nil
	default:
		return nil, fmt.Errorf("source: unknown version kind %q", src.Kind)
	}
	if old == version {
		return nil, nil
	}
	out := make([]byte, 0, len(body)+len(version))
	out = append(append(append(out, body[:start]...), version...), body[end:]...)
	if err := writeFile(dir, path, out); err != nil {
		return nil, err
	}
	return []string{filepath.ToSlash(path)}, nil
}

func jsonTarget(src VersionSource) (string, []string) {
	if src.Kind == KindFactorioInfo {
		p := src.Path
		if p == "" {
			p = DefaultInfoJSON
		}
		return p, []string{"version"}
	}
	return src.Path, strings.Split(src.Key, ".")
}

func versionPattern(p string) (*regexp.Regexp, error) {
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, fmt.Errorf("source: pattern: %w", err)
	}
	if re.NumSubexp() != 1 {
		return nil, errors.New("source: pattern must have exactly one capture group")
	}
	return re, nil
}

// resolve joins a relative path to dir, refusing paths that leave it.
func resolve(dir, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", fmt.Errorf("source: path %q must be relative to the project folder", rel)
	}
	c := filepath.Clean(rel)
	if c == ".." || strings.HasPrefix(filepath.ToSlash(c), "../") {
		return "", fmt.Errorf("source: path %q leaves the project folder", rel)
	}
	return filepath.Join(dir, c), nil
}

func readFile(dir, rel string) ([]byte, error) {
	p, err := resolve(dir, rel)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p) //nolint:gosec // G304: a configured file inside the project folder
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	return b, nil
}

func writeFile(dir, rel string, body []byte) error {
	p, err := resolve(dir, rel)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(p); err == nil {
		mode = st.Mode().Perm()
	}
	if err := os.WriteFile(p, body, mode); err != nil {
		return fmt.Errorf("source: %w", err)
	}
	return nil
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// findJSONString locates the string value at key (object keys from the root)
// and returns its byte span in body including the quotes, and its value.
func findJSONString(body []byte, key []string) (start, end int, val string, err error) {
	skip := 0
	if bytes.HasPrefix(body, utf8BOM) {
		skip = len(utf8BOM)
	}
	dec := json.NewDecoder(bytes.NewReader(body[skip:]))
	type frame struct {
		obj       bool
		expectKey bool
		key       string
	}
	var stack []frame
	path := func() ([]string, bool) {
		out := make([]string, 0, len(stack))
		for _, f := range stack {
			if !f.obj {
				return nil, false
			}
			out = append(out, f.key)
		}
		return out, true
	}
	valueDone := func() {
		if n := len(stack); n > 0 && stack[n-1].obj {
			stack[n-1].expectKey = true
		}
	}
	for {
		before := int(dec.InputOffset())
		tok, terr := dec.Token()
		if errors.Is(terr, io.EOF) {
			return 0, 0, "", fmt.Errorf("%w: key %q", ErrNoVersion, strings.Join(key, "."))
		}
		if terr != nil {
			return 0, 0, "", terr
		}
		after := int(dec.InputOffset())
		if n := len(stack); n > 0 && stack[n-1].obj && stack[n-1].expectKey {
			if d, ok := tok.(json.Delim); ok && d == '}' {
				stack = stack[:n-1]
				valueDone()
				continue
			}
			k, _ := tok.(string)
			stack[n-1].key, stack[n-1].expectKey = k, false
			continue
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, frame{obj: true, expectKey: true})
			case '[':
				stack = append(stack, frame{})
			case ']', '}':
				stack = stack[:len(stack)-1]
				valueDone()
			}
			continue
		case string:
			if p, ok := path(); ok && slices.Equal(p, key) {
				q := bytes.IndexByte(body[skip+before:skip+after], '"')
				if q < 0 {
					return 0, 0, "", errors.New("source: malformed JSON string")
				}
				return skip + before + q, skip + after, t, nil
			}
		default:
			if p, ok := path(); ok && slices.Equal(p, key) {
				return 0, 0, "", fmt.Errorf("source: key %q is not a string", strings.Join(key, "."))
			}
		}
		valueDone()
	}
}

// latestTag is the highest v-prefixed semver tag of the repository in dir.
func latestTag(dir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "tag", "--list", "v*") //nolint:gosec // G204: fixed git arguments
	prepare(cmd)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", fmt.Errorf("source: git tag: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("source: git tag: %w", err)
	}
	var best *Semver
	for line := range strings.SplitSeq(string(out), "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "v") {
			continue
		}
		v, err := Parse(t)
		if err != nil {
			continue
		}
		if best == nil || v.Compare(*best) > 0 {
			best = &v
		}
	}
	if best == nil {
		return "", fmt.Errorf("%w: no v* semver tag", ErrNoVersion)
	}
	return best.String(), nil
}
