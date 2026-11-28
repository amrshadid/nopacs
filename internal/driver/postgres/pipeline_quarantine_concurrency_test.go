package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tests in this file need several writers at once, which only a
// database such as Postgres has: many connections, through several stores
// on one schema as several processes would be, calling at the same moment.
// Each runs its race many rounds and then checks the rows themselves, read
// with a connection of its own.

// TestHoldRacesReceivedBatch guards a hold and a received batch of the same
// file at once, a hundred rounds: the file is one received file and one
// record, quarantined once, and received once or twice: once when the batch
// came first and the hold saw its file, twice when the hold came first, as
// the two would in either order on one writer.
func TestHoldRacesReceivedBatch(t *testing.T) {
	e := newCenv(t, 2, 6)
	e.storage("store")
	e.receiver("a", "store", 11112)
	for round := range 100 {
		r, _, err := e.p().OpenReception(e.ctx, "a", cUID(1, round), e.id("rc"), cT0)
		e.ok("OpenReception", err)
		f := cFile(r, round, round)
		q := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineBadLabel,
			Details: map[string]string{"label": "subject", "fault": tenant.FaultEmpty}, Receiver: "a", ReceptionID: r.ID,
			SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SOPClassUID: f.SOPClassUID,
			TransferSyntaxUID: f.TransferSyntaxUID, SHA256: f.SHA256, Size: f.Size, Incoming: f.Incoming, ContentKept: true, Quarantined: cT0}
		var errs [2]error
		atOnce(2, func(i int) {
			if i == 0 {
				_, errs[0] = e.stores[0].AddReceivedFiles(e.ctx, []tenant.ReceivedFile{f}, cT0, nil)
			} else {
				_, errs[1] = e.stores[1].HoldFiles(e.ctx, []tenant.QuarantinedFile{q}, nil)
			}
		})
		e.ok("AddReceivedFiles", errs[0])
		e.ok("HoldFiles", errs[1])
		got, _, err := e.p().Reception(e.ctx, r.ID)
		e.ok("Reception", err)
		c := got.Counts
		if c.FilesQuarantined != 1 || (c.FilesReceived != 1 && c.FilesReceived != 2) || c.BytesReceived != c.FilesReceived*f.Size {
			t.Fatalf("round %d: %+v, want quarantined once and received once or twice", round, c)
		}
	}
	e.checkTotals("after the holds")
}

// TestReleaseRacesQuietStep guards a release into a reception that is
// receiving against the reception's quiet step, a hundred rounds: a release
// of a held file of the study and the quiet step at once never leave the
// file in the reception the step closed. Either the release comes first,
// and the step decided from the last file before it is refused, or the
// step comes first, and the file joins the study's next arrival.
func TestReleaseRacesQuietStep(t *testing.T) {
	e := newCenv(t, 2, 6)
	e.storage("store")
	e.receiver("a", "store", 11112)
	for round := range 100 {
		r, _, err := e.p().OpenReception(e.ctx, "a", cUID(1, round), e.id("rc"), cT0)
		e.ok("OpenReception", err)
		f := cFile(r, round, round)
		q := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineBadLabel,
			Details: map[string]string{"label": "subject", "fault": tenant.FaultEmpty}, Receiver: "a",
			SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SOPClassUID: f.SOPClassUID,
			TransferSyntaxUID: f.TransferSyntaxUID, SHA256: f.SHA256, Size: f.Size, Incoming: "quarantine/" + f.SOPInstanceUID,
			ContentKept: true, Quarantined: cT0}
		_, err = e.p().HoldFiles(e.ctx, []tenant.QuarantinedFile{q}, nil)
		e.ok("HoldFiles", err)
		var released tenant.QuarantinedFile
		var release, step error
		atOnce(2, func(i int) {
			if i == 0 {
				released, release = e.stores[0].ReleaseQuarantined(e.ctx, q.ID, tenant.ReleaseAsIs, "", e.id("rc"), cT0.Add(time.Second), e.audit(), nil)
			} else {
				_, step = e.stores[1].StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Quiet,
					At: cT0.Add(time.Minute), LastFile: r.LastFile}, nil)
			}
		})
		e.ok("ReleaseQuarantined", release)
		if step != nil && !errors.Is(step, tenant.ErrStateChanged) {
			t.Fatalf("round %d: the quiet step: %v, want success or the state changed", round, step)
		}
		if step == nil && released.ReleaseReception == r.ID {
			t.Fatalf("round %d: the released file joined the reception the quiet step closed", round)
		}
		if step != nil && released.ReleaseReception != r.ID {
			t.Fatalf("round %d: the quiet step was refused, yet the file joined %s, not %s", round, released.ReleaseReception, r.ID)
		}
	}
}

// TestDecisionsOnOneRecordAtOnce guards the lock a release and a rejection
// take on a quarantine record's row before reading it: a second decision
// made while the first is inside its transaction waits for it and is then
// answered as the record stands, the record when it asks for the decision
// made, a precondition failure when it asks for the other one, and never
// Unavailable. The first decision is held open in its guard until the
// second is waiting, so the two meet in this order every run, for each
// pair of decisions.
func TestDecisionsOnOneRecordAtOnce(t *testing.T) {
	e := newCenv(t, 2, 8)
	e.storage("store")
	e.receiver("a", "store", 11112)
	const release, reject = "release", "reject"
	for round, pair := range [][2]string{{release, release}, {release, reject}, {reject, release}, {reject, reject}} {
		r, _, err := e.p().OpenReception(e.ctx, "a", cUID(1, round), e.id("rc"), cT0)
		e.ok("open", err)
		f := cFile(r, round, round)
		q := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineBadLabel,
			Details: map[string]string{"label": "subject", "fault": tenant.FaultEmpty}, Receiver: "a",
			SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SOPClassUID: f.SOPClassUID,
			TransferSyntaxUID: f.TransferSyntaxUID, SHA256: f.SHA256, Size: f.Size, Incoming: f.Incoming, ContentKept: true, Quarantined: cT0}
		_, err = e.p().HoldFiles(e.ctx, []tenant.QuarantinedFile{q}, nil)
		e.ok("hold", err)

		decide := func(store *Tenant, what string, guard tenant.Guard) (tenant.QuarantinedFile, error) {
			if what == reject {
				return store.RejectQuarantined(e.ctx, q.ID, "not wanted", cT0, e.audit(), guard)
			}
			return store.ReleaseQuarantined(e.ctx, q.ID, tenant.ReleaseAsIs, "", e.id("rc"), cT0, e.audit(), guard)
		}
		inside, letGo := make(chan struct{}), make(chan struct{})
		first := make(chan error, 1)
		go func() {
			var once sync.Once
			_, err := decide(e.stores[0], pair[0], func(context.Context) error {
				once.Do(func() {
					close(inside)
					<-letGo
				})
				return nil
			})
			first <- err
		}()
		<-inside
		second := make(chan error, 1)
		var got tenant.QuarantinedFile
		go func() {
			var err error
			got, err = decide(e.stores[1], pair[1], nil)
			second <- err
		}()
		e.waitBlocked(1)
		close(letGo)
		e.ok(pair[0]+" first", <-first)
		err = <-second
		wantState := map[string]tenant.QuarantineState{release: tenant.QuarantineReleased, reject: tenant.QuarantineRejected}[pair[0]]
		switch {
		case pair[0] == pair[1]:
			if err != nil || got.State != wantState {
				t.Fatalf("%s then %s: the second = %s, %v (kind %v); want the record %s as it stands",
					pair[0], pair[1], got.State, err, fault.KindOf(err), wantState)
			}
		case fault.KindOf(err) != fault.Precondition:
			t.Fatalf("%s then %s: the second = %v (kind %v); want a precondition failure", pair[0], pair[1], err, fault.KindOf(err))
		}
		e.checkTotals(fmt.Sprintf("%s then %s", pair[0], pair[1]))
	}
}
