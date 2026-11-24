package version

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// stamped is build information as the go command records it for a build from a
// git checkout with uncommitted changes.
func stamped() *debug.BuildInfo {
	return &debug.BuildInfo{
		GoVersion: "go1.26.1",
		Main:      debug.Module{Path: "github.com/amrshadid/nopacs", Version: "v0.0.0-20261117120000-0123456789ab+dirty"},
		Settings: []debug.BuildSetting{
			{Key: "vcs", Value: "git"},
			{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
}

// TestStringHasTheThreeParts guards the line the version command prints: it
// must name the version, the commit and the Go release, in that order, so a
// person can paste it into a report and a script can read it.
func TestStringHasTheThreeParts(t *testing.T) {
	got := fromBuildInfo(stamped(), "", "").String()
	want := "nopacs v0.0.0-20261117120000-0123456789ab+dirty (commit 0123456789ab-dirty, go1.26.1)"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestOverridesWin guards the -ldflags path: a release stamps its version and
// commit, and those must replace what the build information says, or a
// released binary would report a pseudo-version nobody can find.
func TestOverridesWin(t *testing.T) {
	info := fromBuildInfo(stamped(), "v1.2.3", "fedcba987654")
	if info.Version != "v1.2.3" {
		t.Errorf("Version = %q, want the override v1.2.3", info.Version)
	}
	if info.Commit != "fedcba987654" {
		t.Errorf("Commit = %q, want the override fedcba987654", info.Commit)
	}
	if info.GoVersion != "go1.26.1" {
		t.Errorf("GoVersion = %q, want go1.26.1 from the build information", info.GoVersion)
	}
}

// TestMissingValuesSayUnknown guards a build with no version control stamp and
// no overrides: each part must read "unknown", never be empty and never be
// Go's "(devel)", which a person cannot look up.
func TestMissingValuesSayUnknown(t *testing.T) {
	bare := &debug.BuildInfo{Main: debug.Module{Path: "github.com/amrshadid/nopacs", Version: "(devel)"}}
	for name, bi := range map[string]*debug.BuildInfo{"devel build": bare, "no build information": nil} {
		info := fromBuildInfo(bi, "", "")
		if info.Version != unknown || info.Commit != unknown {
			t.Errorf("%s: Version, Commit = %q, %q; want both %q", name, info.Version, info.Commit, unknown)
		}
		if info.GoVersion != runtime.Version() {
			t.Errorf("%s: GoVersion = %q, want the runtime's %q", name, info.GoVersion, runtime.Version())
		}
	}
}

// TestCleanTreeIsNotDirty guards the commit of a build from an unchanged tree:
// it must not carry the "-dirty" mark, which would send someone looking for
// changes that do not exist.
func TestCleanTreeIsNotDirty(t *testing.T) {
	bi := stamped()
	bi.Settings[2].Value = "false"
	if got := fromBuildInfo(bi, "", "").Commit; got != "0123456789ab" {
		t.Errorf("Commit = %q, want 0123456789ab", got)
	}
}

// TestGetReportsTheRunningBinary guards Get itself, on the test binary: it must
// fill every part, whatever the build recorded.
func TestGetReportsTheRunningBinary(t *testing.T) {
	info := Get()
	// Contains, not HasPrefix: a toolchain built from source reports
	// "devel go1.x-...".
	if info.Version == "" || info.Commit == "" || !strings.Contains(info.GoVersion, "go1") {
		t.Errorf("Get() = %+v; want every part filled and a Go version naming go1", info)
	}
}
