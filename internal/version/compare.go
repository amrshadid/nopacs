package version

import (
	"strconv"
	"strings"
)

// release reads a clean release version, vMAJOR.MINOR.PATCH with nothing after
// the patch number, and returns its major and minor numbers. Anything else
// (a pseudo-version made from a commit, a pre-release, a build with changes,
// "unknown") is a development build and reports false.
func release(v string) (major, minor int, ok bool) {
	rest, found := strings.CutPrefix(v, "v")
	if !found {
		return 0, 0, false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return 0, 0, false
	}
	nums := [3]int{}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p != strconv.Itoa(n) {
			return 0, 0, false
		}
		nums[i] = n
	}
	return nums[0], nums[1], true
}

// Differ says whether a client of build local should warn that the server it
// talks to is build remote. Two releases are alike when their major and minor
// numbers agree: a patch release changes no contract, so warning on it would
// teach people to ignore the warning. When either side is a development build
// there is no number to trust, so the two are alike only when they are the
// same build: the same version and, when both name one, the same commit. A
// remote that reports no version at all (a server older than the field) is
// never a difference, since nothing can be compared.
func Differ(local, remote Info) bool {
	if remote.Version == "" {
		return false
	}
	lmaj, lmin, lok := release(local.Version)
	rmaj, rmin, rok := release(remote.Version)
	if lok && rok {
		return lmaj != rmaj || lmin != rmin
	}
	if local.Version != remote.Version {
		return true
	}
	return local.Commit != unknown && remote.Commit != unknown &&
		remote.Commit != "" && local.Commit != remote.Commit
}
