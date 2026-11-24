package tenanttest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testOpenReception guards open-or-find: a second open of a study at a
// receiver finds the one receiving; once it is complete the next open is a
// new arrival; arrivals are numbered across the tenant's receivers, in
// order; a receiver the tenant does not have is a precondition.
func testOpenReception(e *penv) {
	e.addReceiver("a", "store", 11112)
	e.addReceiver("b", "store", 11113)
	study := uidOf(1, 1)
	first, opened, err := e.p.OpenReception(e.ctx, "a", study, "rc-1", at(1))
	e.ok("OpenReception", err)
	if !opened || first.ID != "rc-1" || first.Arrival != 1 || first.State != reception.Receiving || !first.Started.Equal(at(1)) ||
		!first.LastFile.Equal(at(1)) || first.Receiver != "a" || first.StudyUID != study || !first.Ended.IsZero() {
		e.Errorf("the first open = %+v, %v", first, opened)
	}
	again, opened, err := e.p.OpenReception(e.ctx, "a", study, "rc-2", at(2))
	e.ok("OpenReception again", err)
	if opened || again.ID != "rc-1" {
		e.Errorf("an open of a study receiving opened %v, %s; want the one receiving", opened, again.ID)
	}
	other, opened, err := e.p.OpenReception(e.ctx, "b", study, "rc-3", at(3))
	e.ok("OpenReception at another receiver", err)
	if !opened || other.Arrival != 2 {
		e.Errorf("the study at another receiver opened %v with arrival %d, want a new reception, arrival 2", opened, other.Arrival)
	}
	e.stepTo(first, reception.Complete, "store")
	next, opened, err := e.p.OpenReception(e.ctx, "a", study, "rc-4", at(4))
	e.ok("OpenReception after complete", err)
	if !opened || next.ID != "rc-4" || next.Arrival != 3 {
		e.Errorf("an open after complete = %s arrival %d (%v), want rc-4 arrival 3", next.ID, next.Arrival, opened)
	}
	got, arrivals, err := e.p.Reception(e.ctx, "rc-4")
	e.ok("Reception", err)
	if got.ID != "rc-4" || len(arrivals) != 3 {
		e.Fatalf("Reception = %s with %d arrivals, want 3", got.ID, len(arrivals))
	}
	for i, a := range arrivals {
		if a.Arrival != i+1 {
			e.Errorf("arrival %d is numbered %d", i, a.Arrival)
		}
	}
	if arrivals[0].ID != "rc-1" || arrivals[0].State != reception.Complete {
		e.Errorf("the first arrival is %+v", arrivals[0])
	}
	_, _, err = e.p.OpenReception(e.ctx, "nobody", study, "rc-5", at(5))
	e.wantPrecondition("OpenReception at a receiver the tenant does not have", err)
	_, _, err = e.p.OpenReception(e.ctx, "a", "1.02", "rc-6", at(5))
	e.wantKind("OpenReception of a bad UID", err, fault.Invalid)
	_, _, err = e.p.Reception(e.ctx, "missing")
	e.wantKind("Reception not there", err, fault.NotFound)
}

// testOpenReceptionSameID guards an open called again, or at once, with
// the id of the reception it opened (a receiver's call run again, an
// association that reconnects): the open reception is answered with opened
// false, never a Conflict, and nothing else is made. An id taken by a
// reception that is no longer receiving is still a Conflict, since a new
// reception would need it. And a receiver the tenant no longer has is a
// precondition failure even while a reception of it is open. Both drivers
// must answer alike.
func testOpenReceptionSameID(e *penv) {
	e.addReceiver("a", "store", 11112)
	e.addReceiver("b", "store", 11113)
	study := uidOf(1, 3)
	first, opened, err := e.p.OpenReception(e.ctx, "a", study, "same-1", at(1))
	e.ok("OpenReception", err)
	if !opened {
		e.Error("the first open did not open")
	}
	again, opened, err := e.p.OpenReception(e.ctx, "a", study, "same-1", at(2))
	e.ok("OpenReception again with the same id", err)
	if opened || again.ID != first.ID || !again.Started.Equal(at(1)) {
		e.Errorf("an open again with the same id = %s started %v (opened %v), want %s as it was", again.ID, again.Started, opened, first.ID)
	}
	const n = 8
	errs := make([]error, n)
	ids := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			r, _, err := e.p.OpenReception(e.ctx, "a", uidOf(1, 4), "same-2", at(3))
			ids[i], errs[i] = r.ID, err
		})
	}
	close(start)
	wg.Wait()
	for i := range n {
		e.ok("OpenReception at once with one id", errs[i])
		if ids[i] != "same-2" {
			e.Errorf("an open at once with one id answered %q", ids[i])
		}
	}
	e.stepTo(first, reception.Complete, "store")
	_, _, err = e.p.OpenReception(e.ctx, "a", study, "same-1", at(4))
	e.wantKind("OpenReception with the id of a reception that ended", err, fault.Conflict)
	gone, _, err := e.p.OpenReception(e.ctx, "b", study, "same-3", at(5))
	e.ok("OpenReception", err)
	_, err = e.p.RemoveReceiver(e.ctx, "b", e.audit(), nil)
	e.ok("RemoveReceiver", err)
	_, _, err = e.p.OpenReception(e.ctx, "b", study, gone.ID, at(6))
	e.wantPrecondition("OpenReception at a removed receiver whose reception is open", err)
	list, _, err := e.p.ListReceptions(e.ctx, tenant.ReceptionFilter{}, master.Page{Limit: 100})
	e.ok("ListReceptions", err)
	if len(list) != 3 {
		e.Errorf("%d receptions, want 3", len(list))
	}
}

// testOpenReceptionConcurrent guards that opens of one study at one
// receiver made at once open one reception, whatever the order they reach
// the database in.
func testOpenReceptionConcurrent(e *penv) {
	e.addReceiver("a", "store", 11112)
	const n = 16
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			r, _, err := e.p.OpenReception(e.ctx, "a", uidOf(1, 7), fmt.Sprintf("c%02d", i), at(1))
			ids[i], errs[i] = r.ID, err
		})
	}
	close(start)
	wg.Wait()
	for i := range n {
		e.ok("OpenReception", errs[i])
		if ids[i] != ids[0] {
			e.Errorf("opens at once made two receptions: %s and %s", ids[0], ids[i])
		}
	}
	list, _, err := e.p.ListReceptions(e.ctx, tenant.ReceptionFilter{}, master.Page{Limit: 100})
	e.ok("ListReceptions", err)
	if len(list) != 1 {
		e.Errorf("%d receptions after opens at once, want 1", len(list))
	}
}

// testReceptionSteps guards the steps along the main line: each is the
// state the core's table gives, with its time, the labels and storage with
// identify, the duplicate counts a step decides, the archived counts a
// scan's commit adds, and the end time at the end; the counts a step does
// not decide are kept.
func testReceptionSteps(e *penv) {
	e.addReceiver("a", "store", 11112)
	r := e.openReception("a", 1, at(1))
	_, err := e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{file(r, 1, 1), file(r, 1, 2)}, at(2))
	e.ok("AddReceivedFiles", err)
	r, _, err = e.p.Reception(e.ctx, r.ID)
	e.ok("Reception", err)
	r, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Quiet, At: at(70), LastFile: at(2)}, nil)
	e.ok("StepReception quiet", err)
	if r.State != reception.Complete || !r.StepTime.Equal(at(70)) || r.Counts.FilesReceived != 2 {
		e.Errorf("after quiet: %+v", r)
	}
	r, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Complete, Event: reception.Identify, At: at(71),
		SubjectLabel: "sub-x", SessionLabel: "ses-y", Storage: "store"}, nil)
	e.ok("StepReception identify", err)
	if r.SubjectLabel != "sub-x" || r.SessionLabel != "ses-y" || r.Storage != "store" {
		e.Errorf("identify kept no labels: %+v", r)
	}
	e.commit(r, file(r, 1, 1))
	r, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Identified, Event: reception.Archive, At: at(72),
		Counts: &tenant.StepCounts{FilesDuplicate: 1, BytesDuplicate: 1002}}, nil)
	e.ok("StepReception archive", err)
	want := tenant.ReceptionCounts{FilesReceived: 2, BytesReceived: 2003, FilesArchived: 1, BytesArchived: 1001, FilesDuplicate: 1, BytesDuplicate: 1002}
	if r.Counts != want {
		e.Errorf("counts after archive = %+v, want %+v", r.Counts, want)
	}
	r = e.stepTo(r, reception.Done, "store")
	if !r.State.Ended() || r.Ended.IsZero() || r.SessionLabel != "ses-y" || r.Counts != want {
		e.Errorf("at the end: %+v", r)
	}
	failed := e.failReception(e.openReception("a", 2, at(100)))
	if failed.State != reception.Failed || failed.Failure != "storage unreachable" || failed.Ended.IsZero() {
		e.Errorf("a failed reception reads %+v", failed)
	}
}

// testReceptionStepRefusals guards the compare-and-set: a step from a state
// the reception is no longer in changes nothing (ErrStateChanged), as does a
// quiet step decided before a later file; a step the core refuses holds its
// Refusal, with Already for a step taken; a step under a lease lost changes
// nothing; a file is never a step.
func testReceptionStepRefusals(e *penv) {
	e.addReceiver("a", "store", 11112)
	r := e.openReception("a", 1, at(1))
	_, err := e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{file(r, 1, 1)}, at(5))
	e.ok("AddReceivedFiles", err)
	_, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Quiet, At: at(70), LastFile: at(1)}, nil)
	e.wantIs("a quiet step decided before the last file", err, tenant.ErrStateChanged, fault.Conflict)
	r, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Quiet, At: at(70), LastFile: at(5)}, nil)
	e.ok("a quiet step decided after the last file", err)
	_, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Quiet, At: at(71)}, nil)
	e.wantIs("a step from a state the reception left", err, tenant.ErrStateChanged, fault.Conflict)
	_, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Complete, Event: reception.Quiet, At: at(71)}, nil)
	var refusal *reception.Refusal
	if !errors.As(err, &refusal) || refusal.Kind != reception.Already || fault.KindOf(err) != fault.Conflict {
		e.Errorf("a step already taken: %v, want a Refusal of kind Already, Conflict", err)
	}
	_, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Complete, Event: reception.Verify, At: at(71)}, nil)
	if !errors.As(err, &refusal) || refusal.Kind != reception.OutOfOrder {
		e.Errorf("a step out of order: %v, want a Refusal of kind OutOfOrder", err)
	}
	_, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Complete, Event: reception.File, At: at(71)}, nil)
	e.wantKind("a file as a step", err, fault.Invalid)
	_, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Complete, Event: reception.Identify, At: at(71), SessionLabel: "s", Storage: "store"}, nil)
	e.wantKind("identify with no subject label", err, fault.Invalid)
	_, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: "missing", From: reception.Complete, Event: reception.Fail, At: at(71), Failure: "x"}, nil)
	e.wantKind("a step of a reception not there", err, fault.NotFound)

	_, _, err = e.p.EnqueueWork(e.ctx, tenant.Work{ID: "w1", Kind: "reception", Subject: r.ID, Due: at(1)}, at(1))
	e.ok("EnqueueWork", err)
	first, err := e.p.ClaimWork(e.ctx, "reception", "worker-1", at(72), time.Minute)
	e.ok("ClaimWork", err)
	_, err = e.p.ClaimWork(e.ctx, "reception", "worker-2", at(72+61), time.Minute)
	e.ok("ClaimWork after the lease ended", err)
	step := tenant.ReceptionStep{ID: r.ID, From: reception.Complete, Event: reception.Identify, At: at(140),
		SubjectLabel: "sub-1", SessionLabel: "ses-1", Storage: "store", Fence: &tenant.WorkFence{ID: "w1", Attempt: first.Attempt}}
	_, err = e.p.StepReception(e.ctx, step, nil)
	e.wantIs("a step under a lease taken over", err, tenant.ErrLeaseLost, fault.Conflict)
	if got, _, _ := e.p.Reception(e.ctx, r.ID); got.State != reception.Complete {
		e.Errorf("a step under a lease lost moved the reception to %s", got.State)
	}
	step.Fence.Attempt = first.Attempt + 1
	_, err = e.p.StepReception(e.ctx, step, nil)
	e.ok("a step under the lease that holds", err)
}

// testStepHook guards the hook a step runs in its transaction: it sees the
// reception before and after, the step is not visible to another reader
// while it runs, and an error from it undoes the step and comes back as it
// is.
func testStepHook(e *penv) {
	e.addReceiver("a", "store", 11112)
	r := e.openReception("a", 1, at(1))
	boom := fault.New(fault.Unavailable, "the event cannot be written")
	var calls int
	_, err := e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Quiet, At: at(70)},
		func(_ context.Context, before, after tenant.Reception) error {
			calls++
			if before.State != reception.Receiving || after.State != reception.Complete {
				e.Errorf("the hook saw %s -> %s", before.State, after.State)
			}
			return boom
		})
	if !errors.Is(err, boom) || fault.KindOf(err) != fault.Unavailable {
		e.Errorf("a step whose hook fails: %v, want the hook's error", err)
	}
	if calls != 1 {
		e.Errorf("the hook ran %d times, want once", calls)
	}
	got, _, err := e.p.Reception(e.ctx, r.ID)
	e.ok("Reception", err)
	if got.State != reception.Receiving || !got.StepTime.Equal(at(1)) {
		e.Errorf("a step whose hook failed was kept: %s at %v", got.State, got.StepTime)
	}
	var seenInside reception.State
	_, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Quiet, At: at(70)},
		func(context.Context, tenant.Reception, tenant.Reception) error {
			x, _, err := e.p.Reception(e.ctx, r.ID)
			seenInside = x.State
			return err
		})
	e.ok("a step whose hook succeeds", err)
	if seenInside != reception.Receiving {
		e.Errorf("another reader saw %s while the hook ran: the hook ran after the commit", seenInside)
	}
}

// testReceptionList guards the receptions list: newest first by start time
// then id, each filter and their combination, "in progress" counting every
// state that has not ended, and a cursor bound to its filter.
func testReceptionList(e *penv) {
	e.addReceiver("a", "store", 11112)
	e.addReceiver("b", "store", 11113)
	var all []tenant.Reception
	for i := range 12 {
		recv := "a"
		if i%3 == 0 {
			recv = "b"
		}
		r := e.openReception(recv, i, at(10*i))
		switch i % 4 {
		case 1:
			r = e.stepTo(r, reception.Archived, "store")
		case 2:
			r = e.stepTo(r, reception.Done, "store")
		case 3:
			r = e.failReception(r)
		}
		all = append(all, r)
	}
	list := func(f tenant.ReceptionFilter) []string {
		var ids []string
		for _, r := range allPages(e, 2, func(p master.Page) ([]tenant.Reception, string, error) { return e.p.ListReceptions(e.ctx, f, p) }) {
			ids = append(ids, r.ID)
		}
		return ids
	}
	want := func(keep func(tenant.Reception) bool) []string {
		var ids []string
		for i := len(all) - 1; i >= 0; i-- {
			if keep(all[i]) {
				ids = append(ids, all[i].ID)
			}
		}
		return ids
	}
	for _, c := range []struct {
		what string
		f    tenant.ReceptionFilter
		keep func(tenant.Reception) bool
	}{
		{"no filter", tenant.ReceptionFilter{}, func(tenant.Reception) bool { return true }},
		{"a receiver", tenant.ReceptionFilter{Receiver: "b"}, func(r tenant.Reception) bool { return r.Receiver == "b" }},
		{"a study", tenant.ReceptionFilter{StudyUID: uidOf(1, 2)}, func(r tenant.Reception) bool { return r.StudyUID == uidOf(1, 2) }},
		{"steps", tenant.ReceptionFilter{States: []reception.State{reception.Archived, reception.Failed}},
			func(r tenant.Reception) bool { return r.State == reception.Archived || r.State == reception.Failed }},
		{"in progress", tenant.ReceptionFilter{InProgress: true}, func(r tenant.Reception) bool { return !r.State.Ended() }},
		{"in progress, ended steps only", tenant.ReceptionFilter{InProgress: true, States: []reception.State{reception.Done, reception.Failed}},
			func(tenant.Reception) bool { return false }},
		{"in progress with steps", tenant.ReceptionFilter{InProgress: true, States: []reception.State{reception.Archived, reception.Failed}},
			func(r tenant.Reception) bool { return r.State == reception.Archived }},
		{"a receiver with steps", tenant.ReceptionFilter{Receiver: "a", States: []reception.State{reception.Archived, reception.Done}},
			func(r tenant.Reception) bool {
				return r.Receiver == "a" && (r.State == reception.Archived || r.State == reception.Done)
			}},
		{"a time range", tenant.ReceptionFilter{Since: at(20), Until: at(80)},
			func(r tenant.Reception) bool { return !r.Started.Before(at(20)) && r.Started.Before(at(80)) }},
		{"a receiver in progress since", tenant.ReceptionFilter{Receiver: "a", InProgress: true, Since: at(30)},
			func(r tenant.Reception) bool {
				return r.Receiver == "a" && !r.State.Ended() && !r.Started.Before(at(30))
			}},
	} {
		if got, w := list(c.f), want(c.keep); !slices.Equal(got, w) {
			e.Errorf("ListReceptions with %s = %q, want %q", c.what, got, w)
		}
	}
	_, cursor, err := e.p.ListReceptions(e.ctx, tenant.ReceptionFilter{Receiver: "a"}, master.Page{Limit: 1})
	e.ok("ListReceptions", err)
	_, _, err = e.p.ListReceptions(e.ctx, tenant.ReceptionFilter{Receiver: "b"}, master.Page{Limit: 1, Cursor: cursor})
	e.wantKind("ListReceptions with another filter's cursor", err, fault.Invalid)
	for _, bad := range []tenant.ReceptionFilter{
		{States: []reception.State{"lost"}}, {StudyUID: "x"}, {Receiver: "Bad"}, {Since: at(5), Until: at(5)},
		{States: make([]reception.State, tenant.MaxStepFilter+1)},
	} {
		_, _, err := e.p.ListReceptions(e.ctx, bad, master.Page{Limit: 1})
		e.wantKind(fmt.Sprintf("ListReceptions with %+v", bad), err, fault.Invalid)
	}
}

// testReceptionPagingWithInserts guards the receptions list against
// receptions opened while it is paged, newer and older than the position
// reached: each present throughout comes back exactly once.
func testReceptionPagingWithInserts(e *penv) {
	e.addReceiver("a", "store", 11112)
	for i := range 20 {
		e.openReception("a", i, at(100+i))
	}
	seen := map[string]int{}
	page := master.Page{Limit: 3}
	for round := 0; ; round++ {
		items, next, err := e.p.ListReceptions(e.ctx, tenant.ReceptionFilter{}, page)
		e.ok("ListReceptions", err)
		for _, r := range items {
			seen[r.ID]++
		}
		if next == "" {
			break
		}
		page.Cursor = next
		e.openReception("a", 1000+round, at(1000+round))
		e.openReception("a", 2000+round, at(round))
	}
	for id, n := range seen {
		if n != 1 {
			e.Errorf("%s came back %d times", id, n)
		}
	}
	all, _, _ := e.p.ListReceptions(e.ctx, tenant.ReceptionFilter{Since: at(100), Until: at(120)}, master.Page{Limit: 100})
	for _, r := range all {
		if seen[r.ID] != 1 {
			e.Errorf("%s, present throughout, came back %d times", r.ID, seen[r.ID])
		}
	}
}

// testRetryReception guards the retry of a failed reception: a new
// reception of the same study, complete, the next arrival, with the failed
// one's received files and both ids linked; a repeat answers the same pair
// and makes nothing; the contract's preconditions; the id of the reception
// that runs the files in the audit entry.
func testRetryReception(e *penv) {
	e.addReceiver("a", "store", 11112)
	e.addReceiver("b", "store", 11113)
	r := e.openReception("a", 1, at(1))
	_, err := e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{file(r, 1, 1), file(r, 1, 2)}, at(2))
	e.ok("AddReceivedFiles", err)
	r = e.failReception(e.stepTo(r, reception.Identified, "store"))
	audit := e.audit()
	failed, retry, err := e.p.RetryReception(e.ctx, r.ID, "retry-1", "", at(500), audit, nil)
	e.ok("RetryReception", err)
	if failed.RetryReception != "retry-1" || retry.RetryOf != r.ID || retry.State != reception.Complete || retry.Arrival != 2 ||
		retry.Receiver != "a" || retry.Counts.FilesReceived != 2 || retry.Counts.BytesReceived != 2003 || !retry.Started.Equal(at(500)) {
		e.Errorf("RetryReception = %+v, %+v", failed, retry)
	}
	files, _, err := e.p.ReceivedFiles(e.ctx, "retry-1", master.Page{Limit: 10})
	e.ok("ReceivedFiles", err)
	if len(files) != 2 {
		e.Errorf("the retry holds %d received files, want 2", len(files))
	}
	entries := e.allAudit(master.AuditFilter{})
	if entries[0].ID != audit.ID || entries[0].Details[tenant.DetailRetryReception] != "retry-1" {
		e.Errorf("the retry's audit entry is %+v", entries[0])
	}
	_, again, err := e.p.RetryReception(e.ctx, r.ID, "retry-2", "", at(600), e.audit(), nil)
	e.ok("RetryReception again", err)
	if again.ID != "retry-1" {
		e.Errorf("a repeated retry answered %s, want retry-1", again.ID)
	}
	if _, _, err := e.p.Reception(e.ctx, "retry-2"); fault.KindOf(err) != fault.NotFound {
		e.Error("a repeated retry made a reception")
	}

	_, _, err = e.p.RetryReception(e.ctx, "retry-1", "retry-3", "", at(600), e.audit(), nil)
	e.wantPrecondition("RetryReception of a reception that has not failed", err)
	gone := e.failReception(e.openReception("b", 2, at(10)))
	_, err = e.p.RemoveReceiver(e.ctx, "b", e.audit(), nil)
	e.ok("RemoveReceiver", err)
	refused := e.audit()
	_, _, err = e.p.RetryReception(e.ctx, gone.ID, "retry-4", "", at(600), refused, nil)
	e.wantPrecondition("RetryReception of a removed receiver's reception, naming none", err)
	// The failed reception is marked as retried in the statement that
	// checks it may be; a refusal after that leaves no mark, no reception
	// and no audit entry.
	if still, _, _ := e.p.Reception(e.ctx, gone.ID); still.RetryReception != "" {
		e.Errorf("a refused retry left the failed reception marked retried by %q", still.RetryReception)
	}
	if _, _, err := e.p.Reception(e.ctx, "retry-4"); fault.KindOf(err) != fault.NotFound {
		e.Error("a refused retry made a reception")
	}
	if e.auditIDs()[refused.ID] {
		e.Error("a refused retry wrote its audit entry")
	}
	// A reception already retried is answered as it stands, with the
	// reception made then, even once its receiver is removed and none is
	// named: the removed receiver is a precondition only for a reception
	// not yet retried.
	_, err = e.p.RemoveReceiver(e.ctx, "a", e.audit(), nil)
	e.ok("RemoveReceiver of the retried reception's receiver", err)
	_, stands, err := e.p.RetryReception(e.ctx, r.ID, "retry-x", "", at(650), e.audit(), nil)
	e.ok("RetryReception of a reception retried before its receiver was removed", err)
	if stands.ID != "retry-1" {
		e.Errorf("a retried reception whose receiver is gone answered %s, want retry-1", stands.ID)
	}
	_, err = e.p.AddReceiver(e.ctx, receiverOf("a", "store", 11112), at(651), e.audit(), nil)
	e.wantKind("AddReceiver with the name its receptions still hold", err, fault.Conflict)
	e.addReceiver("c", "store", 11114)
	_, _, err = e.p.RetryReception(e.ctx, gone.ID, "retry-4", "nobody", at(600), e.audit(), nil)
	e.wantPrecondition("RetryReception through a receiver not there", err)
	_, retry, err = e.p.RetryReception(e.ctx, gone.ID, "retry-4", "c", at(600), e.audit(), nil)
	e.ok("RetryReception through another receiver", err)
	if retry.Receiver != "c" {
		e.Errorf("the retry runs by %s, want c", retry.Receiver)
	}
	discarded := e.failReception(e.openReception("c", 3, at(20)))
	_, err = e.p.DiscardReception(e.ctx, discarded.ID, "bad data", at(30), e.audit(), nil)
	e.ok("DiscardReception", err)
	_, _, err = e.p.RetryReception(e.ctx, discarded.ID, "retry-5", "", at(600), e.audit(), nil)
	e.wantPrecondition("RetryReception of a discarded reception", err)
}

// testDiscardReception guards discarding a failed reception's files: the
// reason and time kept, a repeat answering the first reason, the
// contract's preconditions, and the files and bytes in the audit entry.
func testDiscardReception(e *penv) {
	e.addReceiver("a", "store", 11112)
	r := e.openReception("a", 1, at(1))
	_, err := e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{file(r, 1, 1), file(r, 1, 2)}, at(2))
	e.ok("AddReceivedFiles", err)
	r = e.failReception(r)
	audit := e.audit()
	got, err := e.p.DiscardReception(e.ctx, r.ID, "the study was sent to the wrong archive", at(100), audit, nil)
	e.ok("DiscardReception", err)
	if got.DiscardReason != "the study was sent to the wrong archive" || !got.DiscardTime.Equal(at(100)) || got.State != reception.Failed {
		e.Errorf("DiscardReception = %+v", got)
	}
	entries := e.allAudit(master.AuditFilter{})
	if d := entries[0].Details; entries[0].ID != audit.ID || d[tenant.DetailFiles] != "2" || d[tenant.DetailBytes] != "2003" {
		e.Errorf("the discard's audit entry is %+v", entries[0])
	}
	got, err = e.p.DiscardReception(e.ctx, r.ID, "another reason", at(200), e.audit(), nil)
	e.ok("DiscardReception again", err)
	if got.DiscardReason != "the study was sent to the wrong archive" || !got.DiscardTime.Equal(at(100)) {
		e.Errorf("a repeated discard changed the record: %+v", got)
	}
	live := e.openReception("a", 2, at(5))
	_, err = e.p.DiscardReception(e.ctx, live.ID, "x", at(10), e.audit(), nil)
	e.wantPrecondition("DiscardReception of a reception that has not failed", err)
	retried := e.failReception(e.openReception("a", 3, at(5)))
	_, _, err = e.p.RetryReception(e.ctx, retried.ID, e.id("rr"), "", at(10), e.audit(), nil)
	e.ok("RetryReception", err)
	_, err = e.p.DiscardReception(e.ctx, retried.ID, "x", at(10), e.audit(), nil)
	e.wantPrecondition("DiscardReception of a reception retried", err)
	for _, reason := range []string{"", "   ", string(make([]byte, tenant.MaxDecisionReasonLength+1))} {
		_, err = e.p.DiscardReception(e.ctx, r.ID, reason, at(10), e.audit(), nil)
		e.wantKind(fmt.Sprintf("DiscardReception with a reason of %d bytes", len(reason)), err, fault.Invalid)
	}
}
