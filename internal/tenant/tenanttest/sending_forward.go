package tenanttest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testForward guards a receiver's forward: a reception that reaches done
// makes one job per sender its receiver names, in the step's transaction,
// once whatever runs the step again; each sends exactly what that
// reception archived; a reception that fails makes none.
func testForward(e *senv) {
	e.addReceiver("recv", "store", 11112)
	e.addSender("orthanc")
	e.addSender("pacs")
	_, err := e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "recv", ForwardTo: []string{"orthanc", "pacs"}},
		[]tenant.ReceiverField{tenant.FieldForwardTo}, T0, e.audit(), nil)
	e.ok("SetReceiver forward_to", err)
	first := e.archive("recv", 1, 1, 3, 0, at(0), "ses-1")
	start := e.now()
	r := e.finish(first)
	jobs := allPages(e.penv, 10, func(p master.Page) ([]tenant.Job, string, error) {
		return e.p.ListJobs(e.ctx, tenant.JobFilter{}, p)
	})
	if len(jobs) != 2 {
		e.Fatalf("a reception done made %d jobs, want one per sender", len(jobs))
	}
	for _, j := range jobs {
		if j.ID != tenant.ForwardJobID(r.ID, j.Sender) || j.ReceptionID != r.ID || j.Receiver != "recv" ||
			j.Target.SessionLabel != "ses-1" || j.CreatedByKeyID != "" || !j.Created.Equal(r.StepTime) {
			e.Errorf("a forward reads %+v", j)
		}
	}
	events := e.jobEvents(start)
	if len(events) != 2 || events[0].Payload.(*tenant.JobStepReached).Reception != r.ID {
		e.Errorf("the forwards' events: %v", types(events))
	}
	_, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Verified, Event: reception.Finish,
		At: r.StepTime.Add(time.Minute)}, nil)
	if !errors.Is(err, tenant.ErrStateChanged) && fault.KindOf(err) != fault.Conflict {
		e.Errorf("a step taken again: %v", err)
	}
	second := e.archive("recv", 1, 1, 2, 500, at(7200), "ses-1")
	e.finish(second)
	failed := e.archive("recv", 1, 1, 1, 800, at(9000), "ses-1")
	_, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: failed.r.ID, From: reception.Archived, Event: reception.Fail,
		At: failed.r.StepTime.Add(time.Minute), Failure: "storage unreachable"}, nil)
	e.ok("StepReception fail", err)
	all, _, err := e.p.ListJobs(e.ctx, tenant.JobFilter{Sender: "orthanc"}, master.Page{Limit: 10})
	e.ok("ListJobs", err)
	if len(all) != 2 {
		e.Errorf("the orthanc forwards: %d, want one per reception done", len(all))
	}
	j := e.list("orthanc", at(9500))
	if j.ID != tenant.ForwardJobID(r.ID, "orthanc") {
		e.Fatalf("the first listing was of %s", j.ID)
	}
	e.wantCounts("a forward listed after a later arrival", j, 3, 0, 0, 0, 1, 0, true)
	if later := e.list("orthanc", at(9600)); later.Counts.Instances != 2 {
		e.Errorf("the later reception's forward lists %d instances, want its 2", later.Counts.Instances)
	}
	w := e.claim("orthanc", "w", at(9700))
	if got := uidsOf(e.instances(w, "")); fmt.Sprint(got) != fmt.Sprint(fileUIDs(first.files, first.files[0].SeriesUID)) {
		e.Errorf("a forward's stream = %v, want exactly its reception's", got)
	}
}

// testForwardNothingArchived guards that a reception that reaches done
// having archived nothing (every file a duplicate of one held) makes no
// forward job, which would send nothing.
func testForwardNothingArchived(e *senv) {
	e.addReceiver("recv", "store", 11112)
	e.addSender("orthanc")
	_, err := e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "recv", ForwardTo: []string{"orthanc"}},
		[]tenant.ReceiverField{tenant.FieldForwardTo}, T0, e.audit(), nil)
	e.ok("SetReceiver forward_to", err)
	first := e.finish(e.archive("recv", 1, 1, 2, 0, at(0), "ses-1"))
	r, _, err := e.p.OpenReception(e.ctx, "recv", uidOf(1, 1), e.id("rc"), at(600))
	e.ok("OpenReception", err)
	again := file(r, 1, 0)
	again.SeriesUID = uidOf(2, 100)
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{again}, at(600))
	e.ok("AddReceivedFiles", err)
	for _, s := range []struct {
		from reception.State
		ev   reception.Event
	}{{reception.Receiving, reception.Quiet}, {reception.Complete, reception.Identify}, {reception.Identified, reception.Archive},
		{reception.Archived, reception.Verify}, {reception.Verified, reception.Finish}} {
		st := tenant.ReceptionStep{ID: r.ID, From: s.from, Event: s.ev, At: r.StepTime.Add(time.Minute)}
		if s.ev == reception.Identify {
			st.SubjectLabel, st.SessionLabel, st.Storage = "sub-01", "ses-1", "store"
		}
		r, err = e.p.StepReception(e.ctx, st, nil)
		e.ok("StepReception "+string(s.ev), err)
	}
	if r.State != reception.Done {
		e.Fatalf("the reception of a duplicate ended %s", r.State)
	}
	if _, err := e.p.Job(e.ctx, tenant.ForwardJobID(first.ID, "orthanc")); err != nil {
		e.Errorf("the reception that archived made no forward: %v", err)
	}
	if _, err := e.p.Job(e.ctx, tenant.ForwardJobID(r.ID, "orthanc")); fault.KindOf(err) != fault.NotFound {
		e.Errorf("a reception that archived nothing made a forward (%v)", err)
	}
}

// testSenderRemovedWhileForwarding guards a sender removed while a forward
// to it is made: the step that reaches done holds the sender's row from its
// forward to its commit, so a removal that comes meanwhile waits, then sees
// the job and cancels it, and no job is left queued for a sender that is
// gone. The step is held open in its hook, after its forward; the removal
// starts while it is open (on a database with several writers it waits on
// the sender's row; with one writer, on the writer).
func testSenderRemovedWhileForwarding(e *senv) {
	e.addReceiver("recv", "store", 11112)
	e.addSender("orthanc")
	_, err := e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "recv", ForwardTo: []string{"orthanc"}},
		[]tenant.ReceiverField{tenant.FieldForwardTo}, T0, e.audit(), nil)
	e.ok("SetReceiver forward_to", err)
	a := e.archive("recv", 1, 1, 2, 0, at(0), "ses-1")
	r, err := e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: a.r.ID, From: reception.Archived, Event: reception.Verify,
		At: a.r.StepTime.Add(time.Minute)}, nil)
	e.ok("StepReception verify", err)
	inHook, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hook := func(ctx context.Context, _, _ tenant.Reception) error {
		once.Do(func() { close(inHook) })
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}
	type removal struct {
		canceled int64
		err      error
	}
	removed := make(chan removal, 1)
	go func() {
		<-inHook
		if _, err := e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "recv"}, []tenant.ReceiverField{tenant.FieldForwardTo},
			at(9000), e.audit(), nil); err != nil {
			removed <- removal{err: err}
			return
		}
		_, n, err := e.p.RemoveSender(e.ctx, "orthanc", true, at(9000), e.audit(), nil)
		removed <- removal{canceled: n, err: err}
	}()
	go func() {
		<-inHook
		time.Sleep(300 * time.Millisecond)
		close(release)
	}()
	done, err := e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Verified, Event: reception.Finish,
		At: r.StepTime.Add(time.Minute)}, hook)
	e.ok("StepReception finish", err)
	got := <-removed
	e.ok("SetReceiver and RemoveSender", got.err)
	j, err := e.p.Job(e.ctx, tenant.ForwardJobID(done.ID, "orthanc"))
	e.ok("Job of the forward", err)
	if got.canceled != 1 || j.State != tenant.JobCanceled || j.Reason != tenant.JobReasonSenderRemoved {
		e.Errorf("a removal during a forward canceled %d jobs and left the forward %s (%q); want it seen and canceled",
			got.canceled, j.State, j.Reason)
	}
	if _, err := e.p.Sender(e.ctx, "orthanc"); fault.KindOf(err) != fault.NotFound {
		e.Errorf("the sender is still there (%v)", err)
	}
	jobs, _, err := e.p.ListJobs(e.ctx, tenant.JobFilter{InProgress: true}, master.Page{Limit: 10})
	e.ok("ListJobs", err)
	if len(jobs) != 0 {
		e.Errorf("jobs left in progress for a sender removed: %d", len(jobs))
	}
}
