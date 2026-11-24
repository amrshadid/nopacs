package tenant_test

import (
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestJobEventsNameASenderByKind guards the events of a job by its kind:
// a send job's events name its sender, so one with none is refused; a
// deid job waits for no sender, so one naming a sender is refused.
func TestJobEventsNameASenderByKind(t *testing.T) {
	at := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name   string
		j      tenant.Job
		wantOK bool
	}{
		{"a send job with its sender", tenant.Job{ID: "j1", Kind: tenant.JobSend, Sender: "pacs", State: tenant.JobQueued}, true},
		{"a send job with no sender", tenant.Job{ID: "j1", Kind: tenant.JobSend, State: tenant.JobQueued}, false},
		{"a deid job with no sender", tenant.Job{ID: "j1", Kind: tenant.JobDeid, Mode: tenant.DeidCheck, State: tenant.JobQueued}, true},
		{"a deid job naming a sender", tenant.Job{ID: "j1", Kind: tenant.JobDeid, Mode: tenant.DeidCheck, Sender: "pacs", State: tenant.JobQueued}, false},
	} {
		_, err := tenant.JobStepEvent("", c.j, at)
		_, perr := tenant.JobProgressEvent(c.j, at)
		if (err == nil) != c.wantOK || (perr == nil) != c.wantOK {
			t.Errorf("%s: step %v, progress %v; want accepted %v", c.name, err, perr, c.wantOK)
		}
	}
}

// TestTooLargeIsANote guards the code of a file a deid job does not read
// for its size: a note (passed with one), never a refusal, since nothing
// was judged against it.
func TestTooLargeIsANote(t *testing.T) {
	if tenant.FailureTooLarge.Refused() {
		t.Error("too_large counts as refused")
	}
	for _, c := range []tenant.JobFailureCode{tenant.FailureWouldHold, tenant.FailureCheckFailed, tenant.FailureDamagedInStorage} {
		if !c.Refused() {
			t.Errorf("%s counts as a note", c)
		}
	}
}
