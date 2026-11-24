package tenanttest

import (
	"errors"
	"fmt"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The cases of a job's units that go past the plain run: a commit that
// lands late, a listing that gives up, a unit given back, and an instance
// the peer leaves unaccounted.

// testJobLateCommit guards an instance whose held time is before the job's
// creation and whose commit lands after the job listed its scan: it is in
// the scan's stream, so it is sent, and the scan's and the job's totals
// grow by it when its unit records it; the job ends done with every
// instance counted, never stuck on a write that counts past the listing.
func testJobLateCommit(e *senv) {
	_, target := e.setup("orthanc", 1, 3)
	e.createJob("j1", "orthanc", target, at(3600))
	j := e.list("orthanc", at(3700))
	e.wantCounts("a job listed", j, 3, 0, 0, 0, 1, 0, true)
	e.archive("recv", 1, 1, 2, 500, at(1000), "ses-1") // held at 1120 s, before the job
	w := e.claim("orthanc", "w", at(3800))
	if ins := e.instances(w, ""); len(ins) != 5 {
		e.Errorf("the stream after the late commit holds %d instances, want 5", len(ins))
	}
	j = e.sendScan(w, 2, allSent, at(3900))
	e.wantCounts("a job whose scan grew after its listing", j, 5, 5, 0, 0, 1, 1, true)
	if j.State != tenant.JobDone || j.Counts.BytesRemaining() != 0 {
		e.Errorf("the job ended %s with %d bytes remaining, want done with none", j.State, j.Counts.BytesRemaining())
	}
}

// testJobRetryLateCommit guards testJobLateCommit's case for a retry made
// right after its job: an instance held before the ended job's creation
// and committed after the retry listed its scan is the retry's to send, and
// the retry ends done counting it.
func testJobRetryLateCommit(e *senv) {
	_, target := e.setup("orthanc", 1, 3)
	e.createJob("j1", "orthanc", target, at(3600))
	e.list("orthanc", at(3600))
	w := e.claim("orthanc", "w", at(3700))
	ended := e.sendScan(w, 3, func(i int, _ tenant.ScanInstance) tenant.JobFailureCode {
		if i == 1 {
			return tenant.FailurePeerRefused
		}
		return ""
	}, at(3700))
	if ended.State != tenant.JobPartial {
		e.Fatalf("the first job ended %s, want partial", ended.State)
	}
	_, _, err := e.p.RetryJob(e.ctx, "j1", "j2", at(4000), e.audit(), nil)
	e.ok("RetryJob", err)
	r := e.list("orthanc", at(4000))
	e.wantCounts("a retry listed", r, 1, 0, 0, 0, 1, 0, true)
	e.archive("recv", 1, 1, 2, 500, at(1000), "ses-1") // held at 1120 s, before the first job
	u := e.claim("orthanc", "w", at(4100))
	r = e.sendScan(u, 2, allSent, at(4100))
	e.wantCounts("a retry whose scan grew after its listing", r, 3, 3, 0, 0, 1, 1, true)
	if r.State != tenant.JobDone {
		e.Errorf("the retry ended %s, want done", r.State)
	}
}

// listingRetryTimes returns when the claims of a unit failed at each
// attempt from start fall: the time of attempt k's claim is the one before
// it plus RetryDelay of that attempt, as FailWork queues it.
func listingRetryTimes(start time.Time) []time.Time {
	out := []time.Time{start}
	for k := 1; k < tenant.JobUnitAttempts; k++ {
		out = append(out, out[k-1].Add(tenant.RetryDelay(k)))
	}
	return out
}

// testJobListingGivesUp guards a listing whose attempts run out after its
// first page queued scans: the job ends failed and, in the same change, its
// queued units end and every scan of it is ended, so its counts are final;
// a unit held then is answered with the failed job at its next write,
// which records nothing; a retry of the failed job sends the whole target.
func testJobListingGivesUp(e *senv) {
	_, target := e.setup("orthanc", tenant.JobListPage+1, 1)
	e.createJob("j1", "orthanc", target, at(3600))
	times := listingRetryTimes(at(3600))
	last := times[len(times)-1]
	// The first page queues its scans due after every attempt of the
	// listing, so each claim until then takes the listing.
	later := last.Add(30 * time.Minute)
	kind := tenant.SendKind("orthanc")
	lister, err := e.p.ClaimWork(e.ctx, kind, "lister", times[0], time.Hour)
	e.ok("ClaimWork", err)
	if _, err := e.p.StartJobUnit(e.ctx, lister.Fence(), times[0]); err != nil {
		e.Fatalf("StartJobUnit: %v", err)
	}
	if _, more, err := e.p.ListJobScans(e.ctx, lister.Fence(), later); err != nil || !more {
		e.Fatalf("the first page = more %v, %v; want more to list", more, err)
	}
	for k := 1; k < len(times); k++ {
		_, err := e.p.FailWork(e.ctx, lister.Fence(), "the database did not answer", times[k-1])
		e.ok("FailWork", err)
		if lister, err = e.p.ClaimWork(e.ctx, kind, "lister", times[k], time.Hour); err != nil || lister.Subject != "j1" {
			e.Fatalf("claim %d took %+v, %v; want the listing", k+1, lister, err)
		}
	}
	held, err := e.p.ClaimWork(e.ctx, kind, "held", later, time.Hour)
	e.ok("ClaimWork of a scan", err)
	if _, err := e.p.StartJobUnit(e.ctx, held.Fence(), later); err != nil {
		e.Fatalf("StartJobUnit of a scan: %v", err)
	}
	start := e.now()
	w, err := e.p.FailWork(e.ctx, lister.Fence(), "the database did not answer", later)
	e.ok("FailWork of the last attempt", err)
	if w.State != tenant.WorkFailed {
		e.Fatalf("the listing's last attempt left it %s", w.State)
	}
	j, err := e.p.Job(e.ctx, "j1")
	e.ok("Job", err)
	if j.State != tenant.JobFailed || j.Reason != tenant.JobReasonAttemptsRanOut || j.Counts.Listed ||
		j.Counts.Scans != tenant.JobListPage || j.Counts.ScansDone != j.Counts.Scans {
		e.Errorf("a job whose listing gave up reads %+v (%s), want failed with every scan ended", j.Counts, j.State)
	}
	if s := steps(e.jobEvents(start), "j1"); fmt.Sprint(s) != "[failed]" {
		e.Errorf("the give-up's events: %v", s)
	}
	if w, err := e.p.ClaimWork(e.ctx, kind, "w", later.Add(time.Minute), time.Minute); fault.KindOf(err) != fault.NotFound {
		e.Errorf("a queued unit of a job failed is still claimed: %+v (%v)", w, err)
	}
	ins := e.instances(held, "")
	after, err := e.p.RecordJobProgress(e.ctx, progress(held, "", ins, 0, allSent, later.Add(time.Minute)))
	e.ok("RecordJobProgress into a job failed", err)
	if after.State != tenant.JobFailed || after.Counts != j.Counts {
		e.Errorf("a write into a job failed answers %+v (%s), want it as it ended", after.Counts, after.State)
	}
	end, err := e.p.EndJobScan(e.ctx, tenant.JobProgress{Fence: held.Fence(), FailuresKept: tenant.DefaultFailuresKept,
		Now: later.Add(time.Minute)})
	e.ok("EndJobScan of the held unit", err)
	if end.State != tenant.JobFailed || end.Counts != j.Counts {
		e.Errorf("the held unit's end changed the failed job: %+v (%s)", end.Counts, end.State)
	}
	_, _, err = e.p.RetryJob(e.ctx, "j1", "j2", later.Add(time.Hour), e.audit(), nil)
	e.ok("RetryJob", err)
	r := e.runJob("orthanc", allSent, later.Add(2*time.Hour))
	e.wantCounts("a retry of a job whose listing gave up", r, tenant.JobListPage+1, tenant.JobListPage+1, 0, 0,
		tenant.JobListPage+1, tenant.JobListPage+1, true)
}

// testJobReleased guards giving a unit back without spending an attempt:
// thirty releases, each due again later, leave the unit its attempts and
// the job running; a released lease writes nothing; FailWork still spends
// one; the unit then sends its scan.
func testJobReleased(e *senv) {
	_, target := e.setup("orthanc", 1, 2)
	e.createJob("j1", "orthanc", target, at(3600))
	e.list("orthanc", at(3600))
	now := at(3700)
	left := func(w tenant.Work) int {
		e.Helper()
		got, err := e.p.Work(e.ctx, w.ID)
		e.ok("Work", err)
		return got.MaxAttempts - got.Attempt
	}
	var w tenant.Work
	for k := range tenant.JobUnitAttempts {
		w = e.claim("orthanc", "w", now)
		if k == 0 {
			_, err := e.p.StartJobUnit(e.ctx, w.Fence(), now)
			e.ok("StartJobUnit", err)
		}
		if n := left(w); n != tenant.JobUnitAttempts-1 {
			e.Fatalf("claim %d leaves %d attempts, want %d: a release spent one", k+1, n, tenant.JobUnitAttempts-1)
		}
		if err := e.p.ReleaseJobUnit(e.ctx, w.Fence(), now.Add(time.Minute), "the sender is down", now); err != nil {
			e.Fatalf("ReleaseJobUnit: %v", err)
		}
		if _, err := e.p.ClaimWork(e.ctx, tenant.SendKind("orthanc"), "w", now.Add(30*time.Second), time.Minute); fault.KindOf(err) != fault.NotFound {
			e.Fatalf("a unit given back is claimed before it is due (%v)", err)
		}
		now = now.Add(time.Minute)
	}
	ins := e.instances(w, "")
	_, err := e.p.RecordJobProgress(e.ctx, progress(w, "", ins[:1], 0, allSent, now))
	if !errors.Is(err, tenant.ErrLeaseLost) {
		e.Errorf("a lease given back wrote: %v", err)
	}
	if err := e.p.ReleaseJobUnit(e.ctx, w.Fence(), now, "the sender is down", now); !errors.Is(err, tenant.ErrLeaseLost) {
		e.Errorf("a lease given back gave it back again: %v", err)
	}
	if j, _ := e.p.Job(e.ctx, "j1"); j.State != tenant.JobRunning {
		e.Errorf("after %d releases the job is %s, want running", tenant.JobUnitAttempts, j.State)
	}
	w = e.claim("orthanc", "w", now)
	_, err = e.p.FailWork(e.ctx, w.Fence(), "the peer aborted", now)
	e.ok("FailWork", err)
	w = e.claim("orthanc", "w", now.Add(time.Hour))
	if n := left(w); n != tenant.JobUnitAttempts-2 {
		e.Errorf("after FailWork the unit has %d attempts left, want %d", n, tenant.JobUnitAttempts-2)
	}
	if j := e.sendScan(w, 2, allSent, now.Add(time.Hour)); j.State != tenant.JobDone {
		e.Errorf("the job ended %s, want done", j.State)
	}
}

// testJobReleasedAfterItEnded guards a unit given back after its job ended
// (canceled while the unit sent, as removing its sender with its jobs
// canceled does): the release completes the unit and ends its scan, so the
// job counts every scan done and no queued unit is left that nobody would
// ever claim; a listing unit given back after a cancel is completed too.
func testJobReleasedAfterItEnded(e *senv) {
	_, target := e.setup("orthanc", 2, 2)
	e.createJob("j1", "orthanc", target, at(3600))
	e.list("orthanc", at(3600))
	now := at(3700)
	w := e.claim("orthanc", "w", now)
	_, err := e.p.StartJobUnit(e.ctx, w.Fence(), now)
	e.ok("StartJobUnit", err)
	ins := e.instances(w, "")
	_, err = e.p.RecordJobProgress(e.ctx, progress(w, "", ins[:1], 0, allSent, now))
	e.ok("RecordJobProgress", err)
	canceled, err := e.p.CancelJob(e.ctx, "j1", now.Add(time.Second), e.audit(), nil)
	e.ok("CancelJob", err)
	if canceled.Counts.ScansDone != 1 {
		e.Fatalf("the cancel ended %d scans, want the one no worker held", canceled.Counts.ScansDone)
	}
	if err := e.p.ReleaseJobUnit(e.ctx, w.Fence(), now.Add(2*time.Second), "sender removed", now.Add(2*time.Second)); err != nil {
		e.Fatalf("ReleaseJobUnit of a canceled job's unit: %v", err)
	}
	j, err := e.p.Job(e.ctx, "j1")
	e.ok("Job", err)
	if j.State != tenant.JobCanceled || j.Counts.ScansDone != j.Counts.Scans || j.Counts.InstancesSent != 1 {
		e.Errorf("after the release the job reads %+v (%s), want canceled with scans 2 of 2 and its one sent kept", j.Counts, j.State)
	}
	got, err := e.p.Work(e.ctx, w.ID)
	e.ok("Work", err)
	if got.State != tenant.WorkDone {
		e.Errorf("the unit given back after its job ended is %s, want done", got.State)
	}
	if left, err := e.p.ClaimWork(e.ctx, tenant.SendKind("orthanc"), "w", now.Add(time.Hour), time.Minute); fault.KindOf(err) != fault.NotFound {
		e.Errorf("a unit of the canceled job is still claimed: %+v (%v)", left, err)
	}

	// A listing unit given back after its job was canceled.
	e.createJob("j2", "orthanc", target, at(7200))
	l := e.claim("orthanc", "lister", at(7300))
	_, err = e.p.StartJobUnit(e.ctx, l.Fence(), at(7300))
	e.ok("StartJobUnit of the listing", err)
	_, err = e.p.CancelJob(e.ctx, "j2", at(7301), e.audit(), nil)
	e.ok("CancelJob of j2", err)
	if err := e.p.ReleaseJobUnit(e.ctx, l.Fence(), at(7302), "sender disabled", at(7302)); err != nil {
		e.Fatalf("ReleaseJobUnit of a canceled job's listing: %v", err)
	}
	if got, err := e.p.Work(e.ctx, l.ID); err != nil || got.State != tenant.WorkDone {
		e.Errorf("the listing given back after its job ended is %+v (%v), want done", got.State, err)
	}
}

// testJobOwed guards an instance the peer leaves unaccounted: the cursor
// moves past it and it is kept owed, so a crash after that write resends at
// most the batch after the cursor, and the next attempt is told what it
// owes; settling it records its answer once (a repeat changes nothing, an
// instance not owed is refused); one still owed when its unit ends stays
// remaining, and a retry sends it.
func testJobOwed(e *senv) {
	_, target := e.setup("orthanc", 1, 6)
	e.createJob("j1", "orthanc", target, at(3600))
	e.list("orthanc", at(3600))
	const batch = 3
	var peer []string // every instance the peer received, repeats included
	a := e.claim("orthanc", "a", at(3700))
	_, err := e.p.StartJobUnit(e.ctx, a.Fence(), at(3700))
	e.ok("StartJobUnit", err)
	ins := e.instances(a, "")
	uid := func(i int) string { return ins[i].SOPInstanceUID }
	// The peer kept 0 and 2 and left 1 unaccounted.
	first := tenant.JobProgress{Fence: a.Fence(), From: "", To: uid(2), Sent: 2, SentBytes: ins[0].Size + ins[2].Size,
		Owed: []string{uid(1)}, FailuresKept: tenant.DefaultFailuresKept, Now: at(3701)}
	peer = append(peer, uidsOf(ins[:3])...)
	j, err := e.p.RecordJobProgress(e.ctx, first)
	e.ok("RecordJobProgress with an instance owed", err)
	e.wantCounts("a write with an instance owed", j, 6, 2, 0, 0, 1, 0, true)
	peer = append(peer, uidsOf(ins[3:5])...) // sent, then the process dies before recording them
	b := e.claim("orthanc", "b", at(3800))
	u, err := e.p.StartJobUnit(e.ctx, b.Fence(), at(3800))
	e.ok("StartJobUnit after the crash", err)
	if u.Scan.Cursor != uid(2) || len(u.Scan.Owed) != 1 || u.Scan.Owed[0].SOPInstanceUID != uid(1) || u.Scan.Owed[0].Key == "" ||
		u.Scan.Owed[0].Storage != "store" {
		e.Fatalf("the takeover starts at %q owing %+v, want after %q owing %q, placed", u.Scan.Cursor, u.Scan.Owed, uid(2), uid(1))
	}
	peer = append(peer, uid(1)) // the owed instance's second try
	settle := tenant.JobProgress{Fence: b.Fence(), From: uid(2), To: uid(2), Refused: 1, RefusedBytes: ins[1].Size,
		Settled: []string{uid(1)}, FailuresKept: tenant.DefaultFailuresKept, Now: at(3801),
		Failures: []tenant.JobFailure{{SOPInstanceUID: uid(1), Code: tenant.FailurePeerUnaccounted, Detail: "left unaccounted twice",
			Time: at(3801)}}}
	_, err = e.p.RecordJobProgress(e.ctx, settle)
	e.ok("RecordJobProgress settling the owed instance", err)
	again, err := e.p.RecordJobProgress(e.ctx, settle)
	e.ok("RecordJobProgress settling again", err)
	if again.Counts.InstancesRefused != 1 {
		e.Errorf("a settle sent again counted twice: %+v", again.Counts)
	}
	notOwed := progress(b, uid(2), ins[3:4], 3, allSent, at(3802))
	notOwed.Settled, notOwed.Sent, notOwed.SentBytes = []string{uid(0)}, 2, notOwed.SentBytes+ins[0].Size
	_, err = e.p.RecordJobProgress(e.ctx, notOwed)
	e.wantPrecondition("RecordJobProgress settling an instance not owed", err)
	rest := e.instances(b, u.Scan.Cursor)
	peer = append(peer, uidsOf(rest)...)
	j = e.sendScan(b, batch, allSent, at(3810))
	counts := map[string]int{}
	twice := 0
	for _, x := range peer {
		counts[x]++
		if counts[x] == 2 && x != uid(1) {
			twice++
		}
	}
	if len(counts) != 6 || twice > batch {
		e.Errorf("the peer holds %d instances, %d after the cursor received twice; want all 6 and at most %d", len(counts), twice, batch)
	}
	e.wantCounts("a job with an instance left unaccounted twice", j, 6, 5, 0, 1, 1, 1, true)
	failures, _, err := e.p.ListJobFailures(e.ctx, "j1", false, master.Page{Limit: 10})
	e.ok("ListJobFailures", err)
	if len(failures) != 1 || failures[0].SOPInstanceUID != uid(1) || failures[0].Code != tenant.FailurePeerUnaccounted {
		e.Errorf("the failures: %+v, want the owed instance refused as unaccounted", failures)
	}
	// A unit that ends owing an instance leaves it remaining; the retry
	// sends it.
	e.createJob("j2", "orthanc", target, at(4000))
	e.list("orthanc", at(4000))
	c := e.claim("orthanc", "c", at(4100))
	_, err = e.p.StartJobUnit(e.ctx, c.Fence(), at(4100))
	e.ok("StartJobUnit", err)
	all := e.instances(c, "")
	p := progress(c, "", all, 0, allSent, at(4101))
	p.Sent, p.SentBytes, p.Owed = p.Sent-1, p.SentBytes-all[4].Size, []string{all[4].SOPInstanceUID}
	j, err = e.p.EndJobScan(e.ctx, p)
	e.ok("EndJobScan owing an instance", err)
	e.wantCounts("a job whose unit ended owing one", j, 6, 5, 0, 0, 1, 1, true)
	_, _, err = e.p.RetryJob(e.ctx, "j2", "j3", at(4200), e.audit(), nil)
	e.ok("RetryJob", err)
	r := e.list("orthanc", at(4200))
	e.wantCounts("a retry of an owed instance", r, 1, 0, 0, 0, 1, 0, true)
	last := e.claim("orthanc", "d", at(4300))
	if got := uidsOf(e.instances(last, "")); fmt.Sprint(got) != fmt.Sprint([]string{all[4].SOPInstanceUID}) {
		e.Errorf("the retry sends %v, want the owed instance", got)
	}
}
