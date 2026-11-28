package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tests in this file make Tenant.pipe meet the failures it runs a change
// again for, which the row order the package keeps prevents in every race
// the other tests play: a seam ends a chosen attempt, after the change, its
// hook and its guard ran and before its commit, with the error Postgres
// gives for a serialization failure (40001), a deadlock (40P01) or a unique
// violation (23505).

// failOn returns a failAttempts function that ends the attempts for which
// fail reports true with SQLSTATE code, and counts every attempt in n.
func failOn(code string, n *int, fail func(attempt int) bool) func(int) error {
	return func(attempt int) error {
		*n = attempt
		if fail(attempt) {
			return &pgconn.PgError{Severity: "ERROR", Code: code, Message: "injected by the test"}
		}
		return nil
	}
}

// TestPipeRunsAStepAgain guards the retry of a change Postgres ended: a step
// whose first attempt ends with a serialization failure, and one whose first
// attempt ends with a deadlock, run twice; the step's hook runs again; the
// answer is the reception as the second attempt committed it; and pipe
// counts the retry by its SQLSTATE.
func TestPipeRunsAStepAgain(t *testing.T) {
	for i, code := range []string{sqlstateSerialization, sqlstateDeadlock} {
		t.Run(code, func(t *testing.T) {
			e := newCenv(t, 1, 4)
			e.storage("store")
			e.receiver("a", "store", 11112)
			r, _, err := e.p().OpenReception(e.ctx, "a", cUID(1, i), "rc-1", cT0)
			e.ok("open", err)
			_, err = e.p().StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Quiet, At: cT0.Add(time.Minute)}, nil)
			e.ok("quiet", err)
			attempts, hooks := 0, 0
			e.p().failAttempts(failOn(code, &attempts, func(a int) bool { return a == 1 }))
			got, err := e.p().StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Complete, Event: reception.Identify,
				At: cT0.Add(2 * time.Minute), SubjectLabel: "sub-01", SessionLabel: "ses-01", Storage: "store"},
				func(_ context.Context, before, _ tenant.Reception) error {
					hooks++
					if before.State != reception.Complete {
						t.Errorf("attempt %d's hook saw the reception %s; want complete, nothing of the failed attempt kept", hooks, before.State)
					}
					return nil
				})
			e.p().failAttempts(nil)
			e.ok("identify", err)
			if attempts != 2 || hooks != 2 {
				t.Errorf("the step ran %d times and its hook %d; want 2 and 2", attempts, hooks)
			}
			stored, _, err := e.p().Reception(e.ctx, r.ID)
			e.ok("Reception", err)
			if got.State != reception.Identified || got.Storage != "store" || got.SubjectLabel != "sub-01" ||
				stored.State != got.State || stored.StepTime != got.StepTime || stored.Storage != got.Storage {
				t.Errorf("the answer is %+v and the stored reception %+v; want both identified to store", got, stored)
			}
			counts := map[string]int64{sqlstateSerialization: e.p().retried.serialization.Load(), sqlstateDeadlock: e.p().retried.deadlocks.Load()}
			if counts[code] != 1 {
				t.Errorf("retries counted %v; want one for %s", counts, code)
			}
		})
	}
}

// TestPipeRunsAnAuditedChangeAgain guards the retry of a change a person
// asked for: a rejection whose first attempt ends with a deadlock runs its
// guard again, writes its audit entry once, and answers the record as it
// was committed.
func TestPipeRunsAnAuditedChangeAgain(t *testing.T) {
	e := newCenv(t, 1, 4)
	e.storage("store")
	e.receiver("a", "store", 11112)
	r, _, err := e.p().OpenReception(e.ctx, "a", cUID(1, 0), "rc-1", cT0)
	e.ok("open", err)
	f := cFile(r, 0, 1)
	q := tenant.QuarantinedFile{ID: "q1", Reason: tenant.QuarantineBadLabel,
		Details: map[string]string{"label": "subject", "fault": tenant.FaultEmpty}, Receiver: "a",
		SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SOPClassUID: f.SOPClassUID,
		TransferSyntaxUID: f.TransferSyntaxUID, SHA256: f.SHA256, Size: f.Size, Incoming: f.Incoming, ContentKept: true, Quarantined: cT0}
	_, err = e.p().HoldFiles(e.ctx, []tenant.QuarantinedFile{q}, nil)
	e.ok("hold", err)
	attempts, guards := 0, 0
	e.p().failAttempts(failOn(sqlstateDeadlock, &attempts, func(a int) bool { return a == 1 }))
	audit := e.audit()
	got, err := e.p().RejectQuarantined(e.ctx, "q1", "not wanted", cT0, audit, func(context.Context) error {
		guards++
		return nil
	})
	e.p().failAttempts(nil)
	e.ok("reject", err)
	if attempts != 2 || guards != 2 {
		t.Errorf("the rejection ran %d times and its guard %d; want 2 and 2", attempts, guards)
	}
	stored, err := e.p().QuarantinedFile(e.ctx, "q1")
	e.ok("QuarantinedFile", err)
	if got.State != tenant.QuarantineRejected || stored.State != got.State || stored.RejectReason != got.RejectReason {
		t.Errorf("the answer is %s (%q) and the stored record %s (%q); want both rejected", got.State, got.RejectReason, stored.State, stored.RejectReason)
	}
	if n := e.scalar(`SELECT count(*) FROM audit WHERE id = $1`, audit.ID); n != 1 {
		t.Errorf("%d audit entries for the rejection; want 1", n)
	}
	e.checkTotals("after the rejection")
	if n := e.p().retried.deadlocks.Load(); n != 1 {
		t.Errorf("%d deadlocks run again; want 1", n)
	}
}

// TestPipeGivesUpAsUnavailable guards the bound on retries: a change that
// meets a serialization failure, a deadlock or a unique violation on every
// attempt runs maxTxAttempts times, commits nothing, and reaches its caller
// as Unavailable, to be tried again, never as an unknown error.
func TestPipeGivesUpAsUnavailable(t *testing.T) {
	for i, code := range []string{sqlstateSerialization, sqlstateDeadlock, sqlstateUniqueViolation} {
		t.Run(code, func(t *testing.T) {
			e := newCenv(t, 1, 4)
			e.storage("store")
			e.receiver("a", "store", 11112)
			r, _, err := e.p().OpenReception(e.ctx, "a", cUID(1, i), "rc-1", cT0)
			e.ok("open", err)
			attempts, hooks := 0, 0
			e.p().failAttempts(failOn(code, &attempts, func(int) bool { return true }))
			_, err = e.p().StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Quiet, At: cT0.Add(time.Minute)},
				func(context.Context, tenant.Reception, tenant.Reception) error {
					hooks++
					return nil
				})
			e.p().failAttempts(nil)
			if fault.KindOf(err) != fault.Unavailable {
				t.Fatalf("a step that fails with %s on every attempt = %v (kind %v); want Unavailable", code, err, fault.KindOf(err))
			}
			if attempts != maxTxAttempts || hooks != maxTxAttempts {
				t.Errorf("the step ran %d times and its hook %d; want %d", attempts, hooks, maxTxAttempts)
			}
			stored, _, err := e.p().Reception(e.ctx, r.ID)
			e.ok("Reception", err)
			if stored.State != reception.Receiving {
				t.Errorf("the reception is %s after every attempt failed; want receiving, nothing committed", stored.State)
			}
		})
	}
}
