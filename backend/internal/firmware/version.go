// Package firmware computes RouterOS update availability without asking the
// devices: it reads MikroTik's public release feed (one small text file per
// update channel, NEWESTa7.<channel>) and compares the newest version of a
// device's channel with the version the device reports installed.
package firmware

import (
	"regexp"
	"strconv"
	"strings"
)

// Version is a parsed RouterOS version: dotted numbers plus an optional
// pre-release (beta < rc < release).
type Version struct {
	Nums []int
	Pre  int // 0 beta, 1 rc, 2 release
	PreN int
}

var versionRe = regexp.MustCompile(`^(\d+(?:\.\d+)*)(?:(beta|rc)(\d+))?$`)

// ParseVersion parses "7.16.2", "7.17rc3", "7.17beta2" (and tolerates a
// trailing " (stable)" as /system/resource prints it).
func ParseVersion(s string) (Version, bool) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ' '); i >= 0 {
		s = s[:i]
	}
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return Version{}, false
	}
	var v Version
	for _, p := range strings.Split(m[1], ".") {
		n, err := strconv.Atoi(p)
		if err != nil || n > 1_000_000 {
			return Version{}, false
		}
		v.Nums = append(v.Nums, n)
	}
	switch m[2] {
	case "beta":
		v.Pre = 0
	case "rc":
		v.Pre = 1
	default:
		v.Pre = 2
	}
	if m[3] != "" {
		n, err := strconv.Atoi(m[3])
		if err != nil || n > 1_000_000 {
			return Version{}, false
		}
		v.PreN = n
	}
	return v, true
}

// Compare returns -1, 0 or 1 as a is older than, equal to or newer than b.
// Missing trailing numbers count as 0 (7.17 == 7.17.0).
func (a Version) Compare(b Version) int {
	n := max(len(a.Nums), len(b.Nums))
	for i := range n {
		var x, y int
		if i < len(a.Nums) {
			x = a.Nums[i]
		}
		if i < len(b.Nums) {
			y = b.Nums[i]
		}
		if x != y {
			return sign(x - y)
		}
	}
	if a.Pre != b.Pre {
		return sign(a.Pre - b.Pre)
	}
	return sign(a.PreN - b.PreN)
}

func sign(d int) int {
	switch {
	case d < 0:
		return -1
	case d > 0:
		return 1
	}
	return 0
}

// UpdateAvailable reports whether newest is a newer version than installed.
// Unparseable input is never an update: a wrong "update available" would
// send an admin into an upgrade job for nothing.
func UpdateAvailable(installed, newest string) bool {
	a, ok := ParseVersion(installed)
	if !ok {
		return false
	}
	b, ok := ParseVersion(newest)
	if !ok {
		return false
	}
	return b.Compare(a) > 0
}
