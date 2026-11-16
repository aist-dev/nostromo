// Package update checks for, installs and rolls back nostromo releases.
package update

import (
	"fmt"
	"strconv"
	"strings"
)

// Semver is a parsed semantic version.
type Semver struct {
	Major, Minor, Patch int
	Pre                 string
}

// Normalize strips a leading "v".
func Normalize(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }

// ParseSemver parses "1.2.3", "v1.2.3" or "1.2.3-rc.1". Build metadata is ignored.
func ParseSemver(v string) (Semver, error) {
	s := Normalize(v)
	s, _, _ = strings.Cut(s, "+")
	core, pre, _ := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Semver{}, fmt.Errorf("invalid version %q", v)
	}
	var n [3]int
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return Semver{}, fmt.Errorf("invalid version %q", v)
		}
		n[i] = x
	}
	return Semver{Major: n[0], Minor: n[1], Patch: n[2], Pre: pre}, nil
}

// Compare returns -1, 0 or 1. A pre-release sorts before its release.
func (a Semver) Compare(b Semver) int {
	for _, d := range [][2]int{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case a.Pre == b.Pre:
		return 0
	case a.Pre == "":
		return 1
	case b.Pre == "":
		return -1
	case a.Pre < b.Pre:
		return -1
	default:
		return 1
	}
}

// Newer reports whether candidate is newer than current. A current version that
// does not parse (a development build) is older than any release.
func Newer(candidate, current string) bool {
	c, err := ParseSemver(candidate)
	if err != nil {
		return false
	}
	cur, err := ParseSemver(current)
	if err != nil {
		return true
	}
	return c.Compare(cur) > 0
}
