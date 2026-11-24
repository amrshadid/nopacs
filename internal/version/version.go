package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Version is the version this build reports in place of the one in its build
// information, when it is not empty. A release sets it with -ldflags "-X",
// because a binary built from a source archive has no module version of its
// own.
var Version = ""

// Commit is the commit this build reports in place of the revision in its
// build information, when it is not empty. It is set with -ldflags "-X" for
// the same reason as Version: a build outside a git checkout records no
// revision.
var Commit = ""

// unknown stands for a value the build did not record, so a report says plainly
// that it is missing rather than printing an empty field.
const unknown = "unknown"

// commitLength is how many characters of a revision are shown. Twelve is what
// Go itself uses in pseudo-versions, and it is unique in any real repository.
const commitLength = 12

// Info is what a build knows about itself.
type Info struct {
	// Version is the release, a module pseudo-version, or "unknown".
	Version string `json:"version"`
	// Commit is the revision the build was made from, shortened, with
	// "-dirty" when the working tree had changes, or "unknown".
	Commit string `json:"commit"`
	// GoVersion is the Go release that compiled the binary, as go1.26.1.
	GoVersion string `json:"goVersion"`
}

// Get returns the running binary's Info: the -ldflags values where they are
// set, and the embedded build information otherwise.
func Get() Info {
	// Without build information (a binary built outside module mode) the
	// overrides and the runtime's Go version are all there is.
	if bi, ok := debug.ReadBuildInfo(); ok {
		return fromBuildInfo(bi, Version, Commit)
	}
	return fromBuildInfo(nil, Version, Commit)
}

// String is the one line "nopacs <version> (commit <commit>, <go version>)"
// that the version command prints.
func (i Info) String() string {
	return fmt.Sprintf("nopacs %s (commit %s, %s)", i.Version, i.Commit, i.GoVersion)
}

// fromBuildInfo builds Info from build information, which may be nil, and the
// two override values. It is separate from Get so the choice between them is
// tested without building a binary.
func fromBuildInfo(bi *debug.BuildInfo, version, commit string) Info {
	info := Info{Version: version, Commit: commit, GoVersion: runtime.Version()}
	if bi != nil {
		if bi.GoVersion != "" {
			info.GoVersion = bi.GoVersion
		}
		// "(devel)" is what Go records for a build with no version control
		// stamp; it says nothing a person can look up, so it counts as missing.
		if info.Version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			info.Version = bi.Main.Version
		}
		if info.Commit == "" {
			info.Commit = commitFromSettings(bi.Settings)
		}
	}
	if info.Version == "" {
		info.Version = unknown
	}
	if info.Commit == "" {
		info.Commit = unknown
	}
	return info
}

// commitFromSettings reads the revision the go command stamps from git
// (vcs.revision) and marks it "-dirty" when vcs.modified is true, because a
// build from a changed tree is not the commit it names.
func commitFromSettings(settings []debug.BuildSetting) string {
	var revision string
	var modified bool
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision == "" {
		return ""
	}
	if len(revision) > commitLength {
		revision = revision[:commitLength]
	}
	if modified {
		revision += "-dirty"
	}
	return revision
}
