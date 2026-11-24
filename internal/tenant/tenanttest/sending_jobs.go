package tenanttest

import (
	"fmt"
	"slices"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// uidsOf returns the SOP Instance UIDs of the instances, in order.
func uidsOf(ins []tenant.ScanInstance) []string {
	out := make([]string, len(ins))
	for i, in := range ins {
		out[i] = in.SOPInstanceUID
	}
	return out
}

// fileUIDs returns the sorted SOP Instance UIDs of the files of one series.
func fileUIDs(files []tenant.ReceivedFile, series string) []string {
	var out []string
	for _, f := range files {
		if f.SeriesUID == series {
			out = append(out, f.SOPInstanceUID)
		}
	}
	slices.Sort(out)
	return out
}

// testCreateJob guards making a job: a sender the tenant does not have is
// a precondition failure, a target the catalog does not hold NotFound, a
// scan without its session Invalid, an id taken a Conflict; a job made is
// queued, with its key, its event and a listing unit of its sender's kind.
func testCreateJob(e *senv) {
	_, target := e.setup("orthanc", 1, 2)
	mk := func(id, sender string, t tenant.JobTarget) error {
		_, err := e.p.CreateJob(e.ctx, tenant.NewJob{ID: id, Sender: sender, Target: t, Created: at(100)}, e.audit(), nil)
		return err
	}
	e.wantPrecondition("CreateJob to a sender not there", mk("j1", "missing", target))
	e.wantKind("CreateJob of a session not there", mk("j1", "orthanc", tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: "ses-9"}), fault.NotFound)
	e.wantKind("CreateJob of a subject not there", mk("j1", "orthanc", tenant.JobTarget{SubjectLabel: "sub-99"}), fault.NotFound)
	e.wantKind("CreateJob of a scan without its session", mk("j1", "orthanc", tenant.JobTarget{SubjectLabel: "sub-01", ScanLabel: "x"}), fault.Invalid)
	start := e.now()
	j := e.createJob("j1", "orthanc", target, at(100))
	if j.State != tenant.JobQueued || j.Kind != tenant.JobSend || j.CreatedByKeyID != "K1234567890a" || !j.Created.Equal(at(100)) ||
		!j.StateTime.Equal(at(100)) || !j.Started.IsZero() || !j.Ended.IsZero() || j.Target != target || j.Counts.Listed {
		e.Errorf("a job made reads %+v", j)
	}
	e.wantKind("CreateJob of an id taken", mk("j1", "orthanc", target), fault.Conflict)
	events := e.jobEvents(start)
	if len(events) != 1 {
		e.Fatalf("CreateJob wrote %d job events, want 1", len(events))
	}
	p := events[0].Payload.(*tenant.JobStepReached)
	if p.Job != "j1" || p.State != tenant.JobQueued || p.PreviousState != "" || p.Sender != "orthanc" || !events[0].Time.Equal(at(100)) {
		e.Errorf("the event of a job made is %+v at %v", p, events[0].Time)
	}
	w := e.claim("orthanc", "w", at(100))
	if w.MaxAttempts != tenant.JobUnitAttempts || w.Subject != "j1" {
		e.Errorf("a job's listing unit reads %+v", w)
	}
	if _, err := e.p.ClaimWork(e.ctx, "send-other", "w", at(100), time.Minute); fault.KindOf(err) != fault.NotFound {
		e.Errorf("a job's unit is claimed under another sender's kind (%v)", err)
	}
	got, err := e.p.Job(e.ctx, "j1")
	if err != nil || got.ID != "j1" {
		e.Errorf("Job = %+v, %v", got, err)
	}
	_, err = e.p.Job(e.ctx, "missing")
	e.wantKind("Job not there", err, fault.NotFound)
}

// testJobList guards listing jobs newest first, filtered by state, by
// whether they ended, by sender and by time, paged by a cursor bound to its
// filter.
func testJobList(e *senv) {
	_, target := e.setup("orthanc", 1, 1)
	e.addSender("pacs")
	for i := range 7 {
		sender := "orthanc"
		if i%2 == 1 {
			sender = "pacs"
		}
		e.createJob(fmt.Sprintf("j%d", i), sender, target, at(100+i))
	}
	for _, id := range []string{"j1", "j4"} {
		_, err := e.p.CancelJob(e.ctx, id, at(200), e.audit(), nil)
		e.ok("CancelJob", err)
	}
	ids := func(f tenant.JobFilter) []string {
		e.Helper()
		jobs := allPages(e.penv, 2, func(p master.Page) ([]tenant.Job, string, error) { return e.p.ListJobs(e.ctx, f, p) })
		var out []string
		for _, j := range jobs {
			out = append(out, j.ID)
		}
		return out
	}
	for _, c := range []struct {
		f    tenant.JobFilter
		want string
	}{
		{tenant.JobFilter{}, "[j6 j5 j4 j3 j2 j1 j0]"},
		{tenant.JobFilter{Sender: "pacs"}, "[j5 j3 j1]"},
		{tenant.JobFilter{States: []tenant.JobState{tenant.JobCanceled}}, "[j4 j1]"},
		{tenant.JobFilter{States: []tenant.JobState{tenant.JobCanceled, tenant.JobQueued}}, "[j6 j5 j4 j3 j2 j1 j0]"},
		{tenant.JobFilter{InProgress: true}, "[j6 j5 j3 j2 j0]"},
		{tenant.JobFilter{InProgress: true, Sender: "orthanc"}, "[j6 j2 j0]"},
		{tenant.JobFilter{InProgress: true, States: []tenant.JobState{tenant.JobCanceled}}, "[]"},
		{tenant.JobFilter{Since: at(102), Until: at(105)}, "[j4 j3 j2]"},
	} {
		if got := fmt.Sprint(ids(c.f)); got != c.want {
			e.Errorf("ListJobs %+v = %s, want %s", c.f, got, c.want)
		}
	}
	page, next, err := e.p.ListJobs(e.ctx, tenant.JobFilter{}, master.Page{Limit: 2})
	e.ok("ListJobs", err)
	if len(page) != 2 || next == "" {
		e.Fatalf("a first page of 2 = %d jobs, next %q", len(page), next)
	}
	_, _, err = e.p.ListJobs(e.ctx, tenant.JobFilter{Sender: "pacs"}, master.Page{Limit: 2, Cursor: next})
	e.wantKind("ListJobs with another filter's cursor", err, fault.Invalid)
	_, _, err = e.p.ListJobs(e.ctx, tenant.JobFilter{States: []tenant.JobState{"lost"}}, master.Page{Limit: 2})
	e.wantKind("ListJobs of a state not known", err, fault.Invalid)
}

// testJobRun guards a job carried out: its listing lists the scans with
// their counts and queues a unit for each; the first unit makes it running;
// each unit's stream is its scan's instances in the catalog's order; the
// peer's answers are counted, the refused and warned listed; the last unit
// ends it partial; each state is one event.
func testJobRun(e *senv) {
	a, target := e.setup("orthanc", 2, 5)
	start := e.now()
	e.createJob("j1", "orthanc", target, at(3600))
	j := e.list("orthanc", at(3700))
	var bytes int64
	for _, f := range a.files {
		bytes += f.Size
	}
	if j.State != tenant.JobRunning || !j.Started.Equal(at(3700)) || j.Counts.Bytes != bytes {
		e.Errorf("a job listed reads %+v, want running since its first unit with %d bytes", j, bytes)
	}
	e.wantCounts("a job listed", j, 10, 0, 0, 0, 2, 0, true)
	peer := func(i int, _ tenant.ScanInstance) tenant.JobFailureCode {
		switch i {
		case 1:
			return tenant.FailurePeerRefused
		case 2:
			return tenant.FailurePeerWarning
		}
		return ""
	}
	seen := map[string]bool{}
	for range 2 {
		w := e.claim("orthanc", "sender", at(3800))
		u, err := e.p.StartJobUnit(e.ctx, w.Fence(), at(3800))
		e.ok("StartJobUnit", err)
		if u.Listing || u.Scan.Storage != "store" || u.Scan.Path.Subject != "sub-01" || u.Scan.Counts.Instances != 5 {
			e.Errorf("a scan unit reads %+v", u)
		}
		ins := e.instances(w, "")
		series := u.Scan.Path.Scan[len("scan-"):]
		if want := fileUIDs(a.files, series); fmt.Sprint(uidsOf(ins)) != fmt.Sprint(want) {
			e.Errorf("the stream of %s = %v, want %v", u.Scan.Path.Scan, uidsOf(ins), want)
		}
		for _, in := range ins {
			if in.Storage != "store" || in.Key == "" || in.Size == 0 {
				e.Errorf("an instance of a stream reads %+v", in)
			}
		}
		seen[u.Scan.Path.Scan] = true
		j = e.sendScan(w, 2, peer, at(3900))
	}
	if len(seen) != 2 {
		e.Errorf("the units sent the scans %v, want each of two once", seen)
	}
	if _, err := e.p.ClaimWork(e.ctx, tenant.SendKind("orthanc"), "w", at(4000), time.Minute); fault.KindOf(err) != fault.NotFound {
		e.Errorf("a unit is left after the job ended (%v)", err)
	}
	e.wantCounts("a job ended", j, 10, 6, 2, 2, 2, 2, true)
	if j.State != tenant.JobPartial || !j.Ended.Equal(at(3900)) || j.Reason != "" {
		e.Errorf("a job with refusals ended %+v", j)
	}
	if s := steps(e.jobEvents(start), "j1"); fmt.Sprint(s) != "[queued running partial]" {
		e.Errorf("the job's steps: %v", s)
	}
	refused := allPages(e.penv, 1, func(p master.Page) ([]tenant.JobFailure, string, error) {
		return e.p.ListJobFailures(e.ctx, "j1", false, p)
	})
	all := allPages(e.penv, 3, func(p master.Page) ([]tenant.JobFailure, string, error) {
		return e.p.ListJobFailures(e.ctx, "j1", true, p)
	})
	if len(refused) != 2 || len(all) != 4 {
		e.Fatalf("the failures: %d refused, %d with warnings; want 2 and 4", len(refused), len(all))
	}
	for i := 1; i < len(all); i++ {
		a, b := all[i-1], all[i]
		if a.Path.Scan > b.Path.Scan || (a.Path.Scan == b.Path.Scan && a.SOPInstanceUID >= b.SOPInstanceUID) {
			e.Errorf("failures out of order: %v before %v", a, b)
		}
	}
	for _, f := range refused {
		if f.Code != tenant.FailurePeerRefused || f.PeerStatus != 0xC000 || f.Path.Session != "ses-1" || !f.Time.Equal(at(3900)) ||
			f.TransferSyntaxUID == "" {
			e.Errorf("a refused instance reads %+v", f)
		}
	}
	_, _, err := e.p.ListJobFailures(e.ctx, "missing", false, master.Page{Limit: 10})
	e.wantKind("ListJobFailures of a job not there", err, fault.NotFound)
}

// testJobSnapshot guards that a job sends what the catalog held when it was
// made: an instance archived into the same scan later is neither counted
// nor in the stream; a job made after it sends both.
func testJobSnapshot(e *senv) {
	_, target := e.setup("orthanc", 1, 3)
	e.createJob("j1", "orthanc", target, at(3600))
	later := e.archive("recv", 1, 1, 2, 500, at(7200), "ses-1")
	e.finish(later)
	j := e.list("orthanc", at(8000))
	e.wantCounts("a job made before an arrival", j, 3, 0, 0, 0, 1, 0, true)
	w := e.claim("orthanc", "w", at(8000))
	if ins := e.instances(w, ""); len(ins) != 3 {
		e.Errorf("the stream of a job made before an arrival holds %d instances, want 3", len(ins))
	}
	e.sendScan(w, 10, allSent, at(8100))
	e.createJob("j2", "orthanc", target, at(9000))
	j2 := e.list("orthanc", at(9000))
	e.wantCounts("a job made after the arrival", j2, 5, 0, 0, 0, 1, 0, true)
}

// testJobFencing guards a unit's writes: after a takeover the old lease
// writes nothing; the new attempt goes on after the cursor; a write sent
// again after its answer was lost changes nothing; a write that does not
// follow the cursor is refused.
func testJobFencing(e *senv) {
	_, target := e.setup("orthanc", 1, 6)
	e.createJob("j1", "orthanc", target, at(3600))
	e.list("orthanc", at(3600))
	a := e.claim("orthanc", "worker-a", at(3700))
	_, err := e.p.StartJobUnit(e.ctx, a.Fence(), at(3700))
	e.ok("StartJobUnit", err)
	ins := e.instances(a, "")
	first := progress(a, "", ins[:2], 0, allSent, at(3710))
	_, err = e.p.RecordJobProgress(e.ctx, first)
	e.ok("RecordJobProgress", err)
	again, err := e.p.RecordJobProgress(e.ctx, first)
	e.ok("RecordJobProgress sent again", err)
	if again.Counts.InstancesSent != 2 {
		e.Errorf("a write sent again counted twice: %+v", again.Counts)
	}
	skip := progress(a, ins[2].SOPInstanceUID, ins[3:4], 3, allSent, at(3720))
	_, err = e.p.RecordJobProgress(e.ctx, skip)
	e.wantPrecondition("RecordJobProgress that does not follow the cursor", err)
	b := e.claim("orthanc", "worker-b", at(3800)) // a's lease ended at 3760
	if b.ID != a.ID || b.Attempt != 2 {
		e.Fatalf("the takeover claimed %s attempt %d", b.ID, b.Attempt)
	}
	late := progress(a, first.To, ins[2:4], 2, allSent, at(3810))
	_, err = e.p.RecordJobProgress(e.ctx, late)
	e.wantIs("RecordJobProgress of a lease taken over", err, tenant.ErrLeaseLost, fault.Conflict)
	_, err = e.p.EndJobScan(e.ctx, late)
	e.wantIs("EndJobScan of a lease taken over", err, tenant.ErrLeaseLost, fault.Conflict)
	u, err := e.p.StartJobUnit(e.ctx, b.Fence(), at(3800))
	e.ok("StartJobUnit after the takeover", err)
	if u.Scan.Cursor != first.To || u.Scan.Counts.InstancesSent != 2 {
		e.Errorf("the takeover starts at %q with %+v, want after %q with 2 sent", u.Scan.Cursor, u.Scan.Counts, first.To)
	}
	if rest := e.instances(b, u.Scan.Cursor); fmt.Sprint(uidsOf(rest)) != fmt.Sprint(uidsOf(ins[2:])) {
		e.Errorf("the takeover's stream = %v, want %v", uidsOf(rest), uidsOf(ins[2:]))
	}
	j := e.sendScan(b, 4, allSent, at(3900))
	e.wantCounts("a job taken over midway", j, 6, 6, 0, 0, 1, 1, true)
	if j.State != tenant.JobDone {
		e.Errorf("the job ended %s, want done", j.State)
	}
}

// testJobFailuresBounded guards job.failures-kept: past it a failure is
// counted, in the job's counts and failures_not_listed, and not listed;
// and a retry sends whole a scan whose refused were not all listed.
func testJobFailuresBounded(e *senv) {
	_, target := e.setup("orthanc", 1, 150)
	e.createJob("j1", "orthanc", target, at(3600))
	e.list("orthanc", at(3600))
	w := e.claim("orthanc", "w", at(3700))
	_, err := e.p.StartJobUnit(e.ctx, w.Fence(), at(3700))
	e.ok("StartJobUnit", err)
	ins := e.instances(w, "")
	refuse := func(int, tenant.ScanInstance) tenant.JobFailureCode { return tenant.FailureSyntaxNotAccepted }
	cursor := ""
	var j tenant.Job
	for i := 0; i < len(ins); i += 64 {
		p := progress(w, cursor, ins[i:min(i+64, len(ins))], i, refuse, at(3710))
		p.FailuresKept = tenant.MinFailuresKept
		if i+64 < len(ins) {
			j, err = e.p.RecordJobProgress(e.ctx, p)
		} else {
			j, err = e.p.EndJobScan(e.ctx, p)
		}
		e.ok("progress", err)
		cursor = p.To
	}
	listed := allPages(e.penv, 1000, func(p master.Page) ([]tenant.JobFailure, string, error) {
		return e.p.ListJobFailures(e.ctx, "j1", true, p)
	})
	if len(listed) != 100 || j.FailuresNotListed != 50 || j.Counts.InstancesRefused != 150 || j.State != tenant.JobPartial {
		e.Errorf("past the bound: %d listed, %d not listed, %+v (%s); want 100, 50, 150 refused, partial", len(listed),
			j.FailuresNotListed, j.Counts, j.State)
	}
	_, retry, err := e.p.RetryJob(e.ctx, "j1", "j2", at(4000), e.audit(), nil)
	e.ok("RetryJob", err)
	r := e.list("orthanc", at(4000))
	if r.ID != retry.ID || r.Counts.Instances != 150 {
		e.Errorf("a retry of a scan whose refused were not all listed sends %d instances, want the whole 150", r.Counts.Instances)
	}
}

// testJobCancel guards canceling: the job ends canceled with its event; its
// queued units end in the same change; a unit a worker holds is told at
// its next write, whose counts are kept; a cancel again changes nothing;
// a job ended otherwise is refused.
func testJobCancel(e *senv) {
	_, target := e.setup("orthanc", 3, 2)
	e.createJob("j1", "orthanc", target, at(3600))
	e.list("orthanc", at(3600))
	w := e.claim("orthanc", "w", at(3700))
	_, err := e.p.StartJobUnit(e.ctx, w.Fence(), at(3700))
	e.ok("StartJobUnit", err)
	start := e.now()
	j, err := e.p.CancelJob(e.ctx, "j1", at(3710), e.audit(), nil)
	e.ok("CancelJob", err)
	if j.State != tenant.JobCanceled || j.Reason != tenant.JobReasonCanceled || !j.Ended.Equal(at(3710)) || j.Counts.ScansDone != 2 {
		e.Errorf("a job canceled reads %+v, want its two queued scans ended with it", j)
	}
	if _, err := e.p.ClaimWork(e.ctx, tenant.SendKind("orthanc"), "w2", at(3720), time.Minute); fault.KindOf(err) != fault.NotFound {
		e.Errorf("a queued unit of a canceled job is still claimed (%v)", err)
	}
	ins := e.instances(w, "")
	after, err := e.p.RecordJobProgress(e.ctx, progress(w, "", ins[:1], 0, allSent, at(3720)))
	e.ok("RecordJobProgress after a cancel", err)
	if after.State != tenant.JobCanceled || after.Counts.InstancesSent != 1 {
		e.Errorf("the write after a cancel answers %+v, want canceled with its instance counted", after)
	}
	end, err := e.p.EndJobScan(e.ctx, tenant.JobProgress{Fence: w.Fence(), From: ins[0].SOPInstanceUID, To: ins[0].SOPInstanceUID,
		FailuresKept: tenant.DefaultFailuresKept, Now: at(3730)})
	e.ok("EndJobScan after a cancel", err)
	if end.State != tenant.JobCanceled || end.Counts.ScansDone != 3 {
		e.Errorf("the last unit of a canceled job leaves %+v", end)
	}
	again, err := e.p.CancelJob(e.ctx, "j1", at(3800), e.audit(), nil)
	if err != nil || !again.Ended.Equal(at(3710)) {
		e.Errorf("CancelJob again = %+v, %v; want it as it stands", again, err)
	}
	if s := steps(e.jobEvents(start), "j1"); fmt.Sprint(s) != "[canceled]" {
		e.Errorf("the cancels' events: %v, want one", s)
	}
	e.createJob("j2", "orthanc", target, at(4000))
	done := e.runJob("orthanc", allSent, at(4100))
	if done.State != tenant.JobDone {
		e.Fatalf("the second job ended %s", done.State)
	}
	_, err = e.p.CancelJob(e.ctx, "j2", at(4200), e.audit(), nil)
	e.wantPrecondition("CancelJob of a job done", err)
	_, err = e.p.CancelJob(e.ctx, "missing", at(4200), e.audit(), nil)
	e.wantKind("CancelJob of a job not there", err, fault.NotFound)
}

// testJobRetry guards a retry: a new job of exactly the ended job's
// refused instances and those it never sent; the ended job names it and is
// not rewritten; a retry again answers the same; a job running, and one
// that sent everything, are refused.
func testJobRetry(e *senv) {
	a, target := e.setup("orthanc", 2, 4)
	e.createJob("j1", "orthanc", target, at(3600))
	e.list("orthanc", at(3600))
	_, _, err := e.p.RetryJob(e.ctx, "j1", "j2", at(3650), e.audit(), nil)
	e.wantPrecondition("RetryJob of a job running", err)
	// The first scan: its second instance refused. The second: two sent,
	// then the job is canceled.
	w := e.claim("orthanc", "w", at(3700))
	e.sendScan(w, 2, func(i int, _ tenant.ScanInstance) tenant.JobFailureCode {
		if i == 1 {
			return tenant.FailureClassNotAccepted
		}
		return ""
	}, at(3700))
	w2 := e.claim("orthanc", "w", at(3800))
	_, err = e.p.StartJobUnit(e.ctx, w2.Fence(), at(3800))
	e.ok("StartJobUnit", err)
	second := e.instances(w2, "")
	_, err = e.p.RecordJobProgress(e.ctx, progress(w2, "", second[:2], 0, allSent, at(3810)))
	e.ok("RecordJobProgress", err)
	_, err = e.p.CancelJob(e.ctx, "j1", at(3820), e.audit(), nil)
	e.ok("CancelJob", err)
	_, err = e.p.EndJobScan(e.ctx, tenant.JobProgress{Fence: w2.Fence(), From: second[1].SOPInstanceUID,
		To: second[1].SOPInstanceUID, FailuresKept: tenant.DefaultFailuresKept, Now: at(3830)})
	e.ok("EndJobScan of the unit told of the cancel", err)
	ins1 := e.instances(w, "")
	want := []string{ins1[1].SOPInstanceUID, second[2].SOPInstanceUID, second[3].SOPInstanceUID}
	slices.Sort(want)
	ended, retry, err := e.p.RetryJob(e.ctx, "j1", "j2", at(4000), e.audit(), nil)
	e.ok("RetryJob", err)
	if ended.RetryJob != "j2" || retry.RetryOf != "j1" || retry.State != tenant.JobQueued || retry.Target != target ||
		ended.Counts.InstancesSent != 5 || ended.State != tenant.JobCanceled {
		e.Errorf("RetryJob = %+v and %+v", ended, retry)
	}
	again, same, err := e.p.RetryJob(e.ctx, "j1", "j3", at(4100), e.audit(), nil)
	if err != nil || same.ID != "j2" || again.RetryJob != "j2" {
		e.Errorf("RetryJob again = %s, %v; want j2 as it stands", same.ID, err)
	}
	if _, err := e.p.Job(e.ctx, "j3"); fault.KindOf(err) != fault.NotFound {
		e.Error("a retry again made a job")
	}
	r := e.list("orthanc", at(4200))
	e.wantCounts("a retry listed", r, 3, 0, 0, 0, 2, 0, true)
	var got []string
	for range 2 {
		u := e.claim("orthanc", "w", at(4300))
		got = append(got, uidsOf(e.instances(u, ""))...)
		r = e.sendScan(u, 5, allSent, at(4300))
	}
	slices.Sort(got)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		e.Errorf("the retry sent %v, want exactly %v", got, want)
	}
	if r.State != tenant.JobDone {
		e.Errorf("the retry ended %s", r.State)
	}
	_, _, err = e.p.RetryJob(e.ctx, "j2", "j4", at(4400), e.audit(), nil)
	e.wantPrecondition("RetryJob of a job that sent everything", err)
	if old, _ := e.p.Job(e.ctx, "j1"); old.Counts != ended.Counts {
		e.Errorf("the retry rewrote the ended job's counts: %+v", old.Counts)
	}
	_ = a
}

// testJobRetryUnlisted guards a retry of a job canceled before its listing
// ended: it sends what the ended job never listed too.
func testJobRetryUnlisted(e *senv) {
	_, target := e.setup("orthanc", 2, 3)
	e.createJob("j1", "orthanc", target, at(3600))
	_, err := e.p.CancelJob(e.ctx, "j1", at(3610), e.audit(), nil)
	e.ok("CancelJob", err)
	_, _, err = e.p.RetryJob(e.ctx, "j1", "j2", at(3700), e.audit(), nil)
	e.ok("RetryJob", err)
	r := e.runJob("orthanc", allSent, at(3800))
	e.wantCounts("a retry of a job never listed", r, 6, 6, 0, 0, 2, 2, true)
}

// testJobGivesUp guards a unit whose attempts run out: a scan's ends with
// its instances remaining and its job failed once it was the last; a
// listing's ends its job failed.
func testJobGivesUp(e *senv) {
	_, target := e.setup("orthanc", 1, 2)
	e.createJob("j1", "orthanc", target, at(3600))
	e.list("orthanc", at(3600))
	start := e.now()
	exhaust := func(now time.Time) time.Time {
		e.Helper()
		for {
			w, err := e.p.ClaimWork(e.ctx, tenant.SendKind("orthanc"), "w", now, time.Minute)
			e.ok("ClaimWork", err)
			got, err := e.p.FailWork(e.ctx, w.Fence(), "the peer does not answer", now)
			e.ok("FailWork", err)
			now = now.Add(tenant.RetryDelay(w.Attempt) + time.Second)
			if got.State == tenant.WorkFailed {
				return now
			}
		}
	}
	now := exhaust(at(3700))
	j, err := e.p.Job(e.ctx, "j1")
	e.ok("Job", err)
	if j.State != tenant.JobFailed || j.Reason != tenant.JobReasonAttemptsRanOut || j.Counts.InstancesRemaining() != 2 {
		e.Errorf("a job whose unit gave up reads %+v", j)
	}
	if s := steps(e.jobEvents(start), "j1"); fmt.Sprint(s) != "[failed]" {
		e.Errorf("the give-up's events: %v", s)
	}
	e.createJob("j2", "orthanc", target, now)
	exhaust(now)
	if j2, _ := e.p.Job(e.ctx, "j2"); j2.State != tenant.JobFailed || j2.Counts.Listed {
		e.Errorf("a job whose listing gave up reads %+v", j2)
	}
}

// testJobProgressEvents guards the progress events: at most one per job
// in ProgressEventInterval, carrying its counts.
func testJobProgressEvents(e *senv) {
	_, target := e.setup("orthanc", 1, 6)
	e.createJob("j1", "orthanc", target, at(3600))
	e.list("orthanc", at(3600))
	w := e.claim("orthanc", "w", at(3600))
	ins := e.instances(w, "")
	start := e.now()
	cursor := ""
	for i, sec := range []int{3601, 3605, 3610, 3612} {
		p := progress(w, cursor, ins[i:i+1], i, allSent, at(sec))
		_, err := e.p.RecordJobProgress(e.ctx, p)
		e.ok("RecordJobProgress", err)
		cursor = p.To
	}
	var got []string
	for _, ev := range e.jobEvents(start) {
		if p, ok := ev.Payload.(*tenant.JobProgressed); ok {
			got = append(got, fmt.Sprintf("%d@%d", p.Counts.InstancesSent, ev.Time.Sub(T0)/time.Second))
		}
	}
	if fmt.Sprint(got) != "[1@3601 4@3612]" {
		e.Errorf("the progress events: %v, want one at 3601 s and the next 10 s or more later", got)
	}
}

// testJobsInStatus guards the status's jobs in progress: one entry per
// state not ended, with the jobs in it.
func testJobsInStatus(e *senv) {
	_, target := e.setup("orthanc", 1, 1)
	e.createJob("j1", "orthanc", target, at(3600))
	e.list("orthanc", at(3650))
	e.createJob("j2", "orthanc", target, at(3700))
	e.createJob("j3", "orthanc", target, at(3800))
	_, err := e.p.CancelJob(e.ctx, "j3", at(3900), e.audit(), nil)
	e.ok("CancelJob", err)
	s, err := e.ev.StatusSnapshot(e.ctx)
	e.ok("StatusSnapshot", err)
	if fmt.Sprint(s.JobsInProgress) != "[{queued 1} {running 1}]" {
		e.Errorf("the status's jobs in progress: %v", s.JobsInProgress)
	}
}
