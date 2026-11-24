package servecmd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// streamDeadline is the default deadline the server of the test below is
// started with, in place of the contract's 30 seconds, so a stream it ends
// at its deadline is seen in a second.
const streamDeadline = time.Second

// queuedJob archives a corpus file into a new site whose server ends every
// call without its own deadline after streamDeadline, adds a stopped
// sender (whose jobs wait, queued, until it is enabled) and returns the
// site and the archived subject's label.
func queuedJobSite(t *testing.T) (*stowSite, string) {
	t.Helper()
	s := newSTOWSite(t, 1<<20, "--default-deadline", streamDeadline.String())
	if status, body := s.store(s.key, "studies/"+ctStudyUID, stowPart{file: ctStudy[0]}); status != http.StatusOK {
		t.Fatalf("store: %d %s", status, body)
	}
	if n := s.waitArchived(); n != 1 {
		t.Fatalf("archived %d, want 1", n)
	}
	post(t, s.hc, "/nopacs.v1.SettingsService/SetSetting",
		`{"setting":{"name":"sender.destinations","scope":{"installation":{}},"value":"127.0.0.1"}}`)
	post(t, s.hc, "/nopacs.v1.SenderService/AddSender", `{"tenant":"`+s.tenant+`","sender":{"name":"peer","enabled":false,`+
		`"dimse":{"host":"127.0.0.1","port":`+strconv.Itoa(freePort(t))+`,"calledAeTitle":"PEER","callingAeTitle":"NOPACS"}}}`)
	var subjects struct {
		Subjects []struct {
			Label string `json:"label"`
		} `json:"subjects"`
	}
	if err := json.Unmarshal(post(t, s.hc, "/nopacs.v1.CatalogService/ListSubjects", `{"tenant":"`+s.tenant+`"}`), &subjects); err != nil || len(subjects.Subjects) != 1 {
		t.Fatalf("subjects: %v %+v", err, subjects)
	}
	return s, subjects.Subjects[0].Label
}

// cancelAfter cancels the site's one job in progress once d has passed,
// as a person would while a command follows it.
func cancelAfter(t *testing.T, s *stowSite, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() { <-done })
	go func() {
		defer close(done)
		time.Sleep(d)
		var list struct {
			Jobs []struct {
				ID string `json:"id"`
			} `json:"jobs"`
		}
		for range 100 {
			_ = json.Unmarshal(post(t, s.hc, "/nopacs.v1.JobService/ListJobs", `{"tenant":"`+s.tenant+`","inProgress":true}`), &list)
			if len(list.Jobs) == 1 {
				post(t, s.hc, "/nopacs.v1.JobService/CancelJob", `{"tenant":"`+s.tenant+`","id":"`+list.Jobs[0].ID+`"}`)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Error("no job in progress to cancel")
	}()
}

// TestJobWatchOutlastsTheStreamDeadline guards "job watch" and "job create
// --wait" on the real server, which ends every stream of the feed as
// unavailable when its deadline passes: each follows the job for several
// times that deadline, across the streams the server ends, until the job
// ends, and says how it ended (exit 1, canceled) rather than the stream's
// end.
func TestJobWatchOutlastsTheStreamDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a server process; left out under -short")
	}
	s, subject := queuedJobSite(t)
	// No deadline on the commands' context, as a person's run has none: a
	// deadline would go with each stream and the server would keep to it
	// instead of its own.
	ctx := t.Context()

	code, out, errOut := nopacs(ctx, s.dataDir, "job", "create", s.tenant, "--sender", "peer", subject, "--json")
	var made struct {
		Job struct {
			ID string `json:"id"`
		} `json:"job"`
	}
	if err := json.Unmarshal([]byte(out), &made); code != 0 || err != nil || made.Job.ID == "" {
		t.Fatalf("job create: %d %s %s", code, out, errOut)
	}
	follow := 4 * streamDeadline
	cancelAfter(t, s, follow)
	start := time.Now()
	code, out, errOut = nopacs(ctx, s.dataDir, "job", "watch", s.tenant, made.Job.ID)
	if took := time.Since(start); code != 1 || errOut != "nopacs: job "+made.Job.ID+" ended canceled\n" || took < follow ||
		!strings.Contains(out, "canceled") {
		t.Fatalf("job watch over %s: exit %d after %s\nstdout:\n%s\nstderr:\n%s", follow, code, took, out, errOut)
	}

	cancelAfter(t, s, follow)
	start = time.Now()
	code, out, errOut = nopacs(ctx, s.dataDir, "job", "create", s.tenant, "--sender", "peer", subject, "--wait")
	if took := time.Since(start); code != 1 || !strings.HasSuffix(errOut, " ended canceled\n") || took < follow ||
		!strings.Contains(out, "queued") {
		t.Fatalf("job create --wait over %s: exit %d after %s\nstdout:\n%s\nstderr:\n%s", follow, code, took, out, errOut)
	}
}
