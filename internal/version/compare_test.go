package version

import "testing"

// TestDiffer guards when a client warns about the server's build: releases
// differ only by major or minor number, a development build only when it is
// not the same build, and a server that reports nothing never warns.
func TestDiffer(t *testing.T) {
	cases := []struct {
		name          string
		local, remote Info
		want          bool
	}{
		{"same release", Info{Version: "v1.2.0"}, Info{Version: "v1.2.0"}, false},
		{"patch differs", Info{Version: "v1.2.0"}, Info{Version: "v1.2.7"}, false},
		{"minor differs", Info{Version: "v1.2.0"}, Info{Version: "v1.3.0"}, true},
		{"major differs", Info{Version: "v1.2.0"}, Info{Version: "v2.2.0"}, true},
		{"development against release", Info{Version: "v0.0.0-20261117120000-0123456789ab"}, Info{Version: "v0.0.0"}, true},
		{"same development build", Info{Version: "v0.0.0-20261117120000-0123456789ab", Commit: "0123456789ab"}, Info{Version: "v0.0.0-20261117120000-0123456789ab", Commit: "0123456789ab"}, false},
		{"development builds of two commits", Info{Version: "unknown", Commit: "aaaaaaaaaaaa"}, Info{Version: "unknown", Commit: "bbbbbbbbbbbb"}, true},
		{"unknown builds, no commit known", Info{Version: "unknown", Commit: "unknown"}, Info{Version: "unknown", Commit: "unknown"}, false},
		{"a release with a leading zero is no release", Info{Version: "v01.2.0"}, Info{Version: "v1.2.0"}, true},
		{"server reports nothing", Info{Version: "v1.2.0"}, Info{}, false},
	}
	for _, c := range cases {
		if got := Differ(c.local, c.remote); got != c.want {
			t.Errorf("%s: Differ(%+v, %+v) = %v, want %v", c.name, c.local, c.remote, got, c.want)
		}
	}
}
