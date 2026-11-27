package tui

import (
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/cli/download"
)

// TestOverallProgressLine guards the line the download screen shows for the
// whole set: files settled of the total, bytes, the rate and the time left
// when they are known, and only what is known before the first byte.
func TestOverallProgressLine(t *testing.T) {
	for _, c := range []struct {
		name string
		p    download.Progress
		want string
	}{
		{"listing", download.Progress{Phase: download.PhaseListing, FilesTotal: 120}, "listing the files: 120 found"},
		{"before the first byte", download.Progress{Phase: download.PhaseDownloading, FilesTotal: 10, Skipped: 2, BytesTotal: 2048, Remaining: -1}, "2 of 10 files, 0 B of 2.0 KiB"},
		{"under way", download.Progress{
			Phase: download.PhaseDownloading, FilesTotal: 10, Files: 3, Skipped: 1, Failed: 1,
			Bytes: 1 << 20, BytesTotal: 10 << 20, Rate: 1 << 20, Remaining: 9 * time.Second,
		}, "5 of 10 files, 1.0 MiB of 10.0 MiB, 1.0 MiB/s, 9s left"},
	} {
		if got := overallProgress(c.p); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
