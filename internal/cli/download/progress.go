package download

import "time"

// The phases of a download, in order.
const (
	// PhaseListing is the time spent asking the archive what the set holds
	// and checking what is already on disk.
	PhaseListing = "listing"
	// PhaseDownloading is the time spent fetching files.
	PhaseDownloading = "downloading"
	// PhaseDone is the state after the last file.
	PhaseDone = "done"
)

// rateWindow is how far back the transfer rate looks: long enough to smooth
// the pauses between files, short enough to follow a change of speed.
const rateWindow = 10 * time.Second

// Progress is how far a download is, as a whole, across every stream.
type Progress struct {
	// Phase is PhaseListing, PhaseDownloading or PhaseDone.
	Phase string
	// Files is how many files are whole on disk after this run fetched them.
	Files int64
	// Skipped is how many files were already whole and were not fetched.
	Skipped int64
	// Failed is how many files were removed for not matching what the
	// server announced.
	Failed int64
	// FilesTotal is how many files the set holds, known once listing ends;
	// it grows while listing runs.
	FilesTotal int64
	// Bytes is how many bytes this run received.
	Bytes int64
	// BytesTotal is how many bytes this run has to receive for the files
	// that were not whole at the start, less what partial copies already hold.
	BytesTotal int64
	// Rate is the bytes per second received over the last ten seconds.
	Rate float64
	// Remaining is the time left at that rate, or a negative duration when
	// it is not known (nothing received yet, or the rate is zero).
	Remaining time.Duration
	// Elapsed is the time since the download started.
	Elapsed time.Duration
	// Current is the path of the file last written to, or empty.
	Current string
}

// Finished is how many files of the set are settled: whole, skipped or
// failed.
func (p Progress) Finished() int64 { return p.Files + p.Skipped + p.Failed }

// sample is the bytes received at a point in time.
type sample struct {
	at    time.Time
	bytes int64
}

// tracker turns the bytes received so far into a rate over a window of
// time. It is used under the download's lock.
type tracker struct {
	samples []sample
}

// add records that total bytes have been received by now.
func (t *tracker) add(now time.Time, total int64) {
	if n := len(t.samples); n > 0 && now.Sub(t.samples[n-1].at) < 250*time.Millisecond {
		t.samples[n-1].bytes = total
		return
	}
	t.samples = append(t.samples, sample{now, total})
	// Keep one sample at or before the window's start, so the rate spans
	// the whole window and not only what follows its first sample.
	for len(t.samples) > 2 && now.Sub(t.samples[1].at) >= rateWindow {
		t.samples = t.samples[1:]
	}
}

// rate is the bytes per second between the oldest sample kept and the
// newest, or zero when there is no span to measure.
func (t *tracker) rate() float64 {
	if len(t.samples) < 2 {
		return 0
	}
	first, last := t.samples[0], t.samples[len(t.samples)-1]
	span := last.at.Sub(first.at).Seconds()
	if span <= 0 {
		return 0
	}
	return float64(last.bytes-first.bytes) / span
}

// remaining is the time to receive left bytes at rate, or -1.
func remaining(left int64, rate float64) time.Duration {
	if left <= 0 {
		return 0
	}
	if rate <= 0 {
		return -1
	}
	return time.Duration(float64(left) / rate * float64(time.Second))
}
