package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/cli/download"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// downloadReporter shows a download's progress on standard error and the
// events around it (a file removed, the server asking to wait).
type downloadReporter interface {
	// show draws the overall progress.
	show(download.Progress)
	// failure tells of a file removed for not matching.
	failure(message string)
	// throttled tells that the server refused a stream and the download waits.
	throttled(wait time.Duration, streams int)
	// end finishes what was drawn, so the next line starts clean.
	end()
}

// silentReporter shows nothing; failures still go to standard error.
type silentReporter struct{ w io.Writer }

func (silentReporter) show(download.Progress)       {}
func (silentReporter) throttled(time.Duration, int) {}
func (silentReporter) end()                         {}
func (s silentReporter) failure(message string)     { failedLine(s.w, message) }

// failedLine says, on standard error, that a file was removed.
func failedLine(w io.Writer, message string) {
	_, _ = fmt.Fprintf(w, "nopacs: file failed its check and was removed: %s\n", output.Clean(message))
}

// progressLine draws the progress on one line of a terminal, at most four
// times a second.
type progressLine struct {
	w     io.Writer
	last  time.Time
	shown int
}

// show draws the progress.
func (p *progressLine) show(pr download.Progress) {
	if time.Since(p.last) < 250*time.Millisecond && pr.Phase != download.PhaseDone {
		return
	}
	p.last = time.Now()
	p.draw(progressText(pr))
}

// draw replaces the line with text, padded to cover what it was.
func (p *progressLine) draw(text string) {
	pad := ""
	if n := p.shown - len(text); n > 0 {
		pad = strings.Repeat(" ", n)
	}
	p.shown = len(text)
	_, _ = fmt.Fprintf(p.w, "\r%s%s", text, pad)
}

// failure ends the line and says the failure.
func (p *progressLine) failure(message string) {
	p.end()
	failedLine(p.w, message)
}

// throttled says the download is waiting for the server.
func (p *progressLine) throttled(wait time.Duration, streams int) {
	p.draw(fmt.Sprintf("the server asked for fewer streams: waiting %s, now at most %d", wait.Round(time.Second), streams))
}

// end finishes the line.
func (p *progressLine) end() {
	if p.shown > 0 {
		_, _ = fmt.Fprintln(p.w)
		p.shown = 0
	}
}

// progressText is the one line of progress: files, bytes, rate, time left.
func progressText(p download.Progress) string {
	if p.Phase == download.PhaseListing {
		return fmt.Sprintf("listing: %d files found, %d already complete", p.FilesTotal, p.Skipped)
	}
	text := fmt.Sprintf("%d/%d files, %s of %s", p.Finished(), p.FilesTotal, output.Human(p.Bytes), output.Human(p.BytesTotal))
	if p.Phase == download.PhaseDone {
		return text + fmt.Sprintf(", done in %s", p.Elapsed.Round(time.Second))
	}
	if p.Rate > 0 {
		text += ", " + output.Human(int64(p.Rate)) + "/s"
	}
	if p.Remaining >= 0 && p.Rate > 0 {
		text += ", " + p.Remaining.Round(time.Second).String() + " left"
	}
	return text
}

// jsonLines writes one JSON document per line to standard error, for a
// program that drives the command: the fields are those of progressEvent, and
// a line is never split or merged with another.
type jsonLines struct {
	w    io.Writer
	last time.Time
	was  string
}

// jsonLinesEvery is the least time between two progress lines in a phase.
const jsonLinesEvery = time.Second

// progressEvent is a line of "download --progress json". Its fields keep
// their names and meaning in later versions; a field may be added.
type progressEvent struct {
	SchemaVersion int    `json:"schemaVersion"`
	Event         string `json:"event"`
	// Phase is "listing", "downloading" or "done".
	Phase string `json:"phase,omitempty"`
	// Files are counted over the whole set; done is fetched, skipped and
	// failed together, so it reaches total when the download ends.
	Files *progressFiles `json:"files,omitempty"`
	// Bytes counts what this run receives.
	Bytes *progressBytes `json:"bytes,omitempty"`
	// RateBytesPerSecond is the rate over the last ten seconds.
	RateBytesPerSecond *int64 `json:"rateBytesPerSecond,omitempty"`
	// RemainingSeconds is the time left at that rate; absent when unknown.
	RemainingSeconds *int64   `json:"remainingSeconds,omitempty"`
	ElapsedSeconds   *float64 `json:"elapsedSeconds,omitempty"`
	// Message names a file that failed.
	Message string `json:"message,omitempty"`
	// WaitSeconds and Streams describe a "throttled" event: how long the
	// download waits, and how many streams it holds at most afterwards.
	WaitSeconds *float64 `json:"waitSeconds,omitempty"`
	Streams     *int     `json:"streams,omitempty"`
}

// progressFiles is the file counts of a progress line.
type progressFiles struct {
	Done    int64 `json:"done"`
	Total   int64 `json:"total"`
	Fetched int64 `json:"fetched"`
	Skipped int64 `json:"skipped"`
	Failed  int64 `json:"failed"`
}

// progressBytes is the byte counts of a progress line.
type progressBytes struct {
	Received int64 `json:"received"`
	Total    int64 `json:"total"`
}

// emit writes one event as a line.
func (j *jsonLines) emit(e progressEvent) {
	e.SchemaVersion = 1
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	_, _ = j.w.Write(append(b, '\n'))
}

// show writes a progress line, at most one a second in a phase and always at
// a phase's start.
func (j *jsonLines) show(p download.Progress) {
	if p.Phase == j.was && time.Since(j.last) < jsonLinesEvery {
		return
	}
	j.was, j.last = p.Phase, time.Now()
	rate := int64(math.Round(p.Rate))
	elapsed := math.Round(p.Elapsed.Seconds()*1000) / 1000
	e := progressEvent{
		Event: "progress", Phase: p.Phase,
		Files:              &progressFiles{Done: p.Finished(), Total: p.FilesTotal, Fetched: p.Files, Skipped: p.Skipped, Failed: p.Failed},
		Bytes:              &progressBytes{Received: p.Bytes, Total: p.BytesTotal},
		RateBytesPerSecond: &rate, ElapsedSeconds: &elapsed,
	}
	if p.Remaining == 0 || (p.Remaining > 0 && p.Rate > 0) {
		s := int64(math.Round(p.Remaining.Seconds()))
		e.RemainingSeconds = &s
	}
	j.emit(e)
}

// failure writes a "failed" line.
func (j *jsonLines) failure(message string) {
	j.emit(progressEvent{Event: "failed", Message: output.Clean(message)})
}

// throttled writes a "throttled" line.
func (j *jsonLines) throttled(wait time.Duration, streams int) {
	w := math.Round(wait.Seconds()*1000) / 1000
	j.emit(progressEvent{Event: "throttled", WaitSeconds: &w, Streams: &streams})
}

// end has nothing to finish: every line is whole.
func (j *jsonLines) end() {}
