// Package source reads and writes a code project's release sources in its
// folder: the version (info.json, a JSON key, a regex, git tags) and the
// changelog entry of a release (docs/AUTOPILOT.md → Version rules). Pure
// functions on a directory: no network, no app state.
package source

import (
	"fmt"
	"strconv"
	"strings"
)

// Semver is a parsed version MAJOR.MINOR.PATCH[-PRE][+BUILD].
type Semver struct {
	Major, Minor, Patch int
	Pre                 string // without the leading '-'
	Build               string // without the leading '+'; ignored in comparisons
}

// Parse reads a semantic version; a leading "v" is accepted, leading zeros in
// the numbers too (Factorio allows them).
func Parse(s string) (Semver, error) {
	in := s
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	var v Semver
	if i := strings.IndexByte(s, '+'); i >= 0 {
		v.Build, s = s[i+1:], s[:i]
		if v.Build == "" {
			return Semver{}, fmt.Errorf("source: version %q: empty build", in)
		}
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		v.Pre, s = s[i+1:], s[:i]
		if v.Pre == "" {
			return Semver{}, fmt.Errorf("source: version %q: empty pre-release", in)
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Semver{}, fmt.Errorf("source: version %q: want MAJOR.MINOR.PATCH", in)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" || strings.ContainsAny(p, "+-") {
			return Semver{}, fmt.Errorf("source: version %q: bad number %q", in, p)
		}
		*[]*int{&v.Major, &v.Minor, &v.Patch}[i] = n
	}
	return v, nil
}

// String renders the canonical form (no "v").
func (v Semver) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	if v.Build != "" {
		s += "+" + v.Build
	}
	return s
}

// Compare orders by semver precedence: -1, 0, +1 (build metadata ignored).
func (v Semver) Compare(o Semver) int {
	for _, d := range [][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if c := cmpInt(d[0], d[1]); c != 0 {
			return c
		}
	}
	switch {
	case v.Pre == o.Pre:
		return 0
	case v.Pre == "":
		return 1
	case o.Pre == "":
		return -1
	}
	a, b := strings.Split(v.Pre, "."), strings.Split(o.Pre, ".")
	for i := 0; i < len(a) && i < len(b); i++ {
		na, ea := strconv.Atoi(a[i])
		nb, eb := strconv.Atoi(b[i])
		var c int
		switch {
		case ea == nil && eb == nil:
			c = cmpInt(na, nb)
		case ea == nil:
			c = -1 // numeric identifiers sort before alphanumeric ones
		case eb == nil:
			c = 1
		default:
			c = strings.Compare(a[i], b[i])
		}
		if c != 0 {
			return c
		}
	}
	return cmpInt(len(a), len(b))
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// NextPatch is the next patch release: X.Y.(Z+1); a pre-release X.Y.Z-pre becomes X.Y.Z.
func (v Semver) NextPatch() Semver {
	if v.Pre != "" {
		return Semver{Major: v.Major, Minor: v.Minor, Patch: v.Patch}
	}
	return Semver{Major: v.Major, Minor: v.Minor, Patch: v.Patch + 1}
}

// Compare parses and compares two version strings.
func Compare(a, b string) (int, error) {
	va, err := Parse(a)
	if err != nil {
		return 0, err
	}
	vb, err := Parse(b)
	if err != nil {
		return 0, err
	}
	return va.Compare(vb), nil
}

// NextPatch returns the next patch version of s.
func NextPatch(s string) (string, error) {
	v, err := Parse(s)
	if err != nil {
		return "", err
	}
	return v.NextPatch().String(), nil
}

// sameVersion reports whether a and b name the same version (semver-equal, or
// textually equal when either does not parse).
func sameVersion(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if c, err := Compare(a, b); err == nil {
		return c == 0
	}
	return a == b
}
