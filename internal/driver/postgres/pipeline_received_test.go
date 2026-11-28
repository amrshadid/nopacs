package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestBatchAnswersFirstRefusalInBatchOrder guards the answer of a received
// batch that more than one of its receptions refuses: it is the refusal of
// the reception the batch names first, whatever order the receptions are
// locked in (by id), so every driver gives the same batch the same answer.
// Each case names its receptions in an order that differs from their ids'.
func TestBatchAnswersFirstRefusalInBatchOrder(t *testing.T) {
	e := newCenv(t, 1, 4)
	e.storage("store")
	e.receiver("a", "store", 11112)
	closed, _, err := e.p().OpenReception(e.ctx, "a", cUID(1, 0), "rc-b", cT0)
	e.ok("open rc-b", err)
	_, err = e.p().StepReception(e.ctx, tenant.ReceptionStep{ID: closed.ID, From: reception.Receiving, Event: reception.Quiet, At: cT0.Add(time.Minute)}, nil)
	e.ok("quiet rc-b", err)
	open, _, err := e.p().OpenReception(e.ctx, "a", cUID(1, 1), "rc-d", cT0)
	e.ok("open rc-d", err)
	missing := func(id string) tenant.ReceivedFile {
		return cFile(tenant.Reception{ID: id, StudyUID: closed.StudyUID}, 0, 2)
	}
	otherStudy := cFile(open, 0, 3)
	otherStudy.StudyUID = cUID(1, 9)

	for _, c := range []struct {
		name  string
		batch []tenant.ReceivedFile
		want  func(error) bool
		what  string
	}{
		{"closed first, missing second (sorts first)", []tenant.ReceivedFile{cFile(closed, 0, 1), missing("rc-a")},
			func(err error) bool { return errors.Is(err, tenant.ErrReceptionClosed) }, "ErrReceptionClosed"},
		{"missing first, closed second (sorts first)", []tenant.ReceivedFile{missing("rc-c"), cFile(closed, 0, 1)},
			func(err error) bool { return fault.KindOf(err) == fault.NotFound }, "NotFound"},
		{"another study first, closed second (sorts first)", []tenant.ReceivedFile{otherStudy, cFile(closed, 0, 1)},
			func(err error) bool { return fault.KindOf(err) == fault.Invalid }, "Invalid"},
	} {
		_, err := e.p().AddReceivedFiles(e.ctx, c.batch, cT0, nil)
		if !c.want(err) {
			t.Errorf("%s: AddReceivedFiles = %v (kind %v); want %s", c.name, err, fault.KindOf(err), c.what)
		}
	}
	if n := e.scalar(`SELECT count(*) FROM received_files`); n != 0 {
		t.Errorf("%d received files written by refused batches; want none", n)
	}
}
