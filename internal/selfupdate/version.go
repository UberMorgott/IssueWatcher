package selfupdate

import (
	"regexp"
	"strconv"
	"strings"
)

// Versions are release tags "vX.Y.Z" (stable) and "vX.Y.Z-<pre>" (preview,
// e.g. v0.2.0-rc.1). A build made from an untagged commit carries
// `git describe` output ("v0.1.0-3-gabc1234", "-dirty"); such a build, and
// "dev", is a development build: it can check for updates but never installs
// one.
var releaseRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

// describeRe matches the "-<n>-g<hash>" part git describe appends past a tag.
var describeRe = regexp.MustCompile(`-\d+-g[0-9a-f]{4,}(-dirty)?$|-dirty$`)

type semver struct {
	nums [3]int
	pre  []string // dot-separated prerelease identifiers; nil = release
}

func parse(v string) (semver, bool) {
	m := releaseRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return semver{}, false
	}
	var s semver
	for i := range 3 {
		if len(m[i+1]) > 1 && m[i+1][0] == '0' { // no leading zeros
			return semver{}, false
		}
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return semver{}, false
		}
		s.nums[i] = n
	}
	if m[4] != "" {
		s.pre = strings.Split(m[4], ".")
	}
	return s, true
}

// IsRelease reports whether v is a release version this build can update from
// or to: a tag, not "dev" or a git describe string past a tag.
func IsRelease(v string) bool {
	if describeRe.MatchString(v) {
		return false
	}
	_, ok := parse(v)
	return ok
}

// IsPrerelease reports whether v has a prerelease part (preview channel).
func IsPrerelease(v string) bool {
	s, ok := parse(v)
	return ok && s.pre != nil
}

// Compare orders two versions by SemVer 2.0 precedence: -1, 0 or +1. An
// unparsable side compares equal, so Newer fails closed.
func Compare(a, b string) int {
	x, aok := parse(a)
	y, bok := parse(b)
	if !aok || !bok {
		return 0
	}
	for i := range 3 {
		if x.nums[i] != y.nums[i] {
			return sign(x.nums[i] - y.nums[i])
		}
	}
	switch {
	case x.pre == nil && y.pre == nil:
		return 0
	case x.pre == nil:
		return 1 // a release outranks its prereleases
	case y.pre == nil:
		return -1
	}
	for i := 0; i < len(x.pre) && i < len(y.pre); i++ {
		if c := compareIdent(x.pre[i], y.pre[i]); c != 0 {
			return c
		}
	}
	return sign(len(x.pre) - len(y.pre))
}

// Newer reports whether candidate is strictly newer than current; false when
// either is not a release version (fail closed: no downgrade, no dev builds).
func Newer(candidate, current string) bool {
	return IsRelease(candidate) && IsRelease(current) && Compare(candidate, current) > 0
}

func compareIdent(a, b string) int {
	an, aerr := strconv.Atoi(a)
	bn, berr := strconv.Atoi(b)
	switch {
	case aerr == nil && berr == nil:
		return sign(an - bn)
	case aerr == nil:
		return -1 // numeric identifiers sort before alphanumeric ones
	case berr == nil:
		return 1
	}
	return strings.Compare(a, b)
}

func sign(n int) int {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	}
	return 0
}
