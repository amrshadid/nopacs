package tenanttest

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// deidSetup adds a receiver and a profile version, and archives two
// sessions of two subjects' worth: ses-1 (two scans of three instances)
// as received, and ses-2 (one scan of two) de-identified by the version,
// which it returns.
func (e *senv) deidSetup() tenant.ProfileRef {
	e.Helper()
	e.addReceiver("recv", "store", 11112)
	v, _, err := e.p.CreateProfileVersion(e.ctx, newVersion("research", "one"), at(0), e.audit(), nil)
	e.ok("CreateProfileVersion", err)
	e.archive("recv", 1, 2, 3, 0, at(-3600), "ses-1")
	e.profile = v.Ref()
	e.archive("recv", 2, 1, 2, 100, at(-3600), "ses-2")
	e.profile = tenant.ProfileRef{}
	return v.Ref()
}

// createDeid makes a deid job at the time given, failing the test on an
// error.
func (e *senv) createDeid(id string, mode tenant.DeidMode, p tenant.ProfileRef, t tenant.JobTarget, when time.Time) tenant.Job {
	e.Helper()
	j, err := e.p.CreateJob(e.ctx, tenant.NewJob{ID: id, Kind: tenant.JobDeid, Mode: mode, Profile: p, Target: t, Created: when}, e.audit(), nil)
	e.ok("CreateJob", err)
	return j
}

// claimDeid claims the next unit of the tenant's deid jobs.
func (e *senv) claimDeid(owner string, now time.Time) (tenant.Work, error) {
	return e.p.ClaimWork(e.ctx, tenant.DeidWorkKind, owner, now, time.Minute)
}

// listDeid claims a deid job's listing unit and lists every scan.
func (e *senv) listDeid(now time.Time) tenant.Job {
	e.Helper()
	w, err := e.claimDeid("lister", now)
	e.ok("ClaimWork", err)
	u, err := e.p.StartJobUnit(e.ctx, w.Fence(), now)
	e.ok("StartJobUnit", err)
	if !u.Listing {
		e.Fatalf("the first unit of a deid job is %+v, want its listing", u)
	}
	for range 1000 {
		j, more, err := e.p.ListJobScans(e.ctx, w.Fence(), now)
		e.ok("ListJobScans", err)
		if !more {
			return j
		}
	}
	e.Fatal("a listing did not end")
	return tenant.Job{}
}

// testDeidJobs guards the jobs that judge archived instances against a
// profile version: a deid job names no sender, a mode, and a profile
// version only for a dry run, which must exist; it may name the whole
// tenant, whose listing finds every scan of every subject; its units are
// of one kind of their own, never a sender's; its stream says which
// version each instance records; its progress records passes, notes and
// refusals with the counts per key, a write sent again counting nothing
// twice; it ends partial with a refusal, its events naming no sender; and
// a retry makes a deid job of the same mode and profile that judges again
// exactly what was refused.
func testDeidJobs(e *senv) {
	ref := e.deidSetup()
	whole := tenant.JobTarget{}
	mk := func(j tenant.NewJob) error {
		j.ID, j.Created = "bad", at(100)
		_, err := e.p.CreateJob(e.ctx, j, e.audit(), nil)
		return err
	}
	e.wantKind("a deid job naming a sender", mk(tenant.NewJob{Kind: tenant.JobDeid, Mode: tenant.DeidCheck, Sender: "orthanc"}), fault.Invalid)
	e.wantKind("a deid job with no mode", mk(tenant.NewJob{Kind: tenant.JobDeid}), fault.Invalid)
	e.wantKind("a check naming a version", mk(tenant.NewJob{Kind: tenant.JobDeid, Mode: tenant.DeidCheck, Profile: ref}), fault.Invalid)
	e.wantKind("a send job of the whole tenant", mk(tenant.NewJob{Sender: "orthanc"}), fault.Invalid)
	e.wantKind("a dry run of a version not stored",
		mk(tenant.NewJob{Kind: tenant.JobDeid, Mode: tenant.DeidDryRun, Profile: tenant.ProfileRef{Name: "research", Version: 2}}), fault.NotFound)
	e.wantKind("a deid job of a subject not there",
		mk(tenant.NewJob{Kind: tenant.JobDeid, Mode: tenant.DeidCheck, Target: tenant.JobTarget{SubjectLabel: "sub-99"}}), fault.NotFound)
	dry := e.createDeid("dry", tenant.DeidDryRun, ref, tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: "ses-2"}, at(50))
	if dry.Kind != tenant.JobDeid || dry.Mode != tenant.DeidDryRun || dry.Profile != ref || dry.Sender != "" {
		e.Errorf("a dry run made reads %+v", dry)
	}
	_, err := e.p.CancelJob(e.ctx, "dry", at(60), e.audit(), nil)
	e.ok("CancelJob of the dry run", err)
	start := e.now()
	j := e.createDeid("chk", tenant.DeidCheck, tenant.ProfileRef{}, whole, at(100))
	if j.Kind != tenant.JobDeid || j.Mode != tenant.DeidCheck || !j.Profile.IsZero() || !j.Target.Whole() || j.State != tenant.JobQueued {
		e.Errorf("a check made reads %+v", j)
	}
	if _, err := e.p.ClaimWork(e.ctx, tenant.SendKind("orthanc"), "w", at(100), time.Minute); fault.KindOf(err) != fault.NotFound {
		e.Error("a deid job's unit was claimed under a sender's kind")
	}
	listed := e.listDeid(at(100))
	e.wantCounts("the whole tenant listed", listed, 8, 0, 0, 0, 3, 0, true)
	if got, err := e.p.Job(e.ctx, "chk"); err != nil || got.Mode != tenant.DeidCheck || got.Kind != tenant.JobDeid {
		e.Errorf("Job of a check = %+v, %v", got, err)
	}
	var last tenant.Job
	var refusedUID string
	for range 3 {
		w, err := e.claimDeid("worker", at(200))
		e.ok("ClaimWork", err)
		u, err := e.p.StartJobUnit(e.ctx, w.Fence(), at(200))
		e.ok("StartJobUnit", err)
		ins := e.instances(w, "")
		deidentified := u.Scan.Path.Session == "ses-2"
		for _, in := range ins {
			if want := map[bool]tenant.ProfileRef{true: ref}[deidentified]; in.Profile != want {
				e.Errorf("instance %s of %s records %v in a job's stream, want %v", in.SOPInstanceUID, u.Scan.Path, in.Profile, want)
			}
		}
		// As received: each passed with a note. De-identified: the first
		// refused, the other passed.
		outcome := func(i int, _ tenant.ScanInstance) tenant.JobFailureCode {
			switch {
			case !deidentified:
				return tenant.FailureNotDeidentified
			case i == 0:
				return tenant.FailureCheckFailed
			}
			return ""
		}
		p := progress(w, "", ins, 0, outcome, at(210))
		p.Tallies = map[string]int64{"class|1.2.840.10008.5.1.4.1.1.4|passed": int64(len(ins))}
		if deidentified {
			refusedUID = ins[0].SOPInstanceUID
			p.Tallies = map[string]int64{"check|removed_element": 1, "check|removed_element|00100030": 1,
				"class|1.2.840.10008.5.1.4.1.1.4|passed": 1, "class|1.2.840.10008.5.1.4.1.1.4|refused": 1}
		}
		_, err = e.p.RecordJobProgress(e.ctx, p)
		e.ok("RecordJobProgress", err)
		_, err = e.p.RecordJobProgress(e.ctx, p)
		e.ok("RecordJobProgress sent again", err)
		last, err = e.p.EndJobScan(e.ctx, tenant.JobProgress{Fence: w.Fence(), From: p.To, To: p.To,
			FailuresKept: tenant.DefaultFailuresKept, Now: at(220)})
		e.ok("EndJobScan", err)
	}
	e.wantCounts("the check ended", last, 8, 1, 6, 1, 3, 3, true)
	if last.State != tenant.JobPartial {
		e.Errorf("a check with a refusal ended %s, want partial", last.State)
	}
	tallies, err := e.p.JobTallies(e.ctx, "chk")
	e.ok("JobTallies", err)
	want := "[{class|1.2.840.10008.5.1.4.1.1.4|passed 7} {check|removed_element 1} {check|removed_element|00100030 1} {class|1.2.840.10008.5.1.4.1.1.4|refused 1}]"
	if got := fmt.Sprint(tallies); got != want {
		e.Errorf("JobTallies = %s, want %s (a write sent again counted once)", got, want)
	}
	for _, ev := range e.jobEvents(start) {
		if p, ok := ev.Payload.(*tenant.JobStepReached); ok && p.Job == "chk" && p.Sender != "" {
			e.Errorf("a deid job's event names sender %q", p.Sender)
		}
	}
	if got := steps(e.jobEvents(start), "chk"); fmt.Sprint(got) != "[queued running partial]" {
		e.Errorf("a check's states were %v", got)
	}
	failures, _, err := e.p.ListJobFailures(e.ctx, "chk", true, firstPage(100))
	e.ok("ListJobFailures", err)
	if len(failures) != 7 {
		e.Errorf("a check listed %d failures with the notes, want 7", len(failures))
	}
	ended, retry, err := e.p.RetryJob(e.ctx, "chk", "chk2", at(300), e.audit(), nil)
	e.ok("RetryJob of a check", err)
	if ended.RetryJob != "chk2" || retry.Kind != tenant.JobDeid || retry.Mode != tenant.DeidCheck || retry.Sender != "" || !retry.Target.Whole() {
		e.Errorf("the retry of a check reads %+v", retry)
	}
	r := e.listDeid(at(310))
	e.wantCounts("the retry of a check listed", r, 1, 0, 0, 0, 1, 0, true)
	w, err := e.claimDeid("worker", at(320))
	e.ok("ClaimWork of the retry", err)
	if ins := e.instances(w, ""); len(ins) != 1 || ins[0].SOPInstanceUID != refusedUID {
		e.Errorf("the retry of a check judges %v, want only %s", uidsOf(ins), refusedUID)
	}
	tallies, err = e.p.JobTallies(e.ctx, "chk2")
	if e.ok("JobTallies of a job that found nothing yet", err); len(tallies) != 0 {
		e.Errorf("a new job has tallies %v", tallies)
	}
	_, err = e.p.JobTallies(e.ctx, "missing")
	e.wantKind("JobTallies of no job", err, fault.NotFound)
}

// firstPage is the first page of a list of n.
func firstPage(n int) master.Page { return master.Page{Limit: n} }

// sorted returns uids in byte order.
func sorted(uids ...string) []string {
	slices.Sort(uids)
	return uids
}

// testDeidJobTallies guards the bound on a job's tallies: past 1000 keys,
// what a write counts under a new key is added to "other", while the keys
// kept go on counting; and a write that counts no instance adds to none.
func testDeidJobTallies(e *senv) {
	e.deidSetup()
	e.createDeid("chk", tenant.DeidCheck, tenant.ProfileRef{}, tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: "ses-1"}, at(100))
	e.listDeid(at(100))
	w, err := e.claimDeid("worker", at(200))
	e.ok("ClaimWork", err)
	_, err = e.p.StartJobUnit(e.ctx, w.Fence(), at(200))
	e.ok("StartJobUnit", err)
	ins := e.instances(w, "")
	cursor := ""
	for i, in := range ins {
		p := progress(w, cursor, []tenant.ScanInstance{in}, i, allSent, at(210))
		p.Tallies = map[string]int64{"check|kept": 1}
		for k := range 400 {
			p.Tallies["rule|r"+strconv.Itoa(i*400+k)+"|remove"] = 2
		}
		if len(p.Tallies) > tenant.MaxJobTallyBatch {
			e.Fatalf("the test writes %d keys at once", len(p.Tallies))
		}
		_, err := e.p.RecordJobProgress(e.ctx, p)
		e.ok("RecordJobProgress", err)
		cursor = p.To
	}
	bad := tenant.JobProgress{Fence: w.Fence(), From: cursor, To: cursor, FailuresKept: tenant.DefaultFailuresKept, Now: at(220),
		Tallies: map[string]int64{"check|kept": 1}}
	_, err = e.p.RecordJobProgress(e.ctx, bad)
	e.wantKind("a write that counts no instance with tallies", err, fault.Invalid)
	tallies, err := e.p.JobTallies(e.ctx, "chk")
	e.ok("JobTallies", err)
	byKey := map[string]int64{}
	for _, t := range tallies {
		byKey[t.Key] = t.Count
	}
	// Three writes of 400 keys of their own and one they share: 1201 keys,
	// of which 1000 are kept.
	if len(tallies) != tenant.MaxJobTallies+1 || byKey["check|kept"] != 3 || byKey[tenant.TallyOther] != 2*(3*400+1-tenant.MaxJobTallies) {
		e.Errorf("a job past its bound keeps %d tallies, check|kept %d, other %d", len(tallies), byKey["check|kept"], byKey[tenant.TallyOther])
	}
	// Most first, then by key: "other" (402) leads, "check|kept" (3) next.
	ordered := slices.IsSortedFunc(tallies, func(a, b tenant.JobTally) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Key, b.Key))
	})
	if !ordered || tallies[0].Key != tenant.TallyOther || tallies[1].Key != "check|kept" {
		e.Errorf("tallies are not most first, then by key: %v, %v ... %v", tallies[0], tallies[1], tallies[len(tallies)-1])
	}
}

// testSeenUIDs guards the lookup a check makes of the catalog's UIDs: a
// series', a study's or an instance's UID is found among every scan and
// instance; among what was archived as received, only the UIDs of
// instances that record no profile version, and of their scans and
// studies; among what was archived de-identified, only those of instances
// that record one; a UID the catalog does not hold is never found; and a
// call past its bound is refused.
func testSeenUIDs(e *senv) {
	e.deidSetup()
	received := uidOf(3, 0)     // an instance of ses-1, as received
	deid := uidOf(3, 100)       // an instance of ses-2, de-identified
	series := uidOf(2, 100)     // the first scan of ses-1
	deidSeries := uidOf(2, 200) // the scan of ses-2
	study := uidOf(1, 1)
	deidStudy := uidOf(1, 2)
	all := []string{received, deid, series, deidSeries, study, deidStudy, "1.2.3.4"}
	seen, err := e.p.SeenUIDs(e.ctx, all, all)
	e.ok("SeenUIDs", err)
	wantKeyed := fmt.Sprint(sorted(received, deid, series, deidSeries, study, deidStudy))
	if got := fmt.Sprint(seen.Keyed); got != wantKeyed {
		e.Errorf("SeenUIDs keyed = %s, want %s", got, wantKeyed)
	}
	if got, want := fmt.Sprint(seen.AsReceived), fmt.Sprint(sorted(received, series, study)); got != want {
		e.Errorf("SeenUIDs as received = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(seen.Deidentified), fmt.Sprint(sorted(deid, deidSeries, deidStudy)); got != want {
		e.Errorf("SeenUIDs de-identified = %s, want %s", got, want)
	}
	big := make([]string, tenant.MaxSeenUIDs+1)
	for i := range big {
		big[i] = "1.2." + strconv.Itoa(i+1)
	}
	_, err = e.p.SeenUIDs(e.ctx, big, nil)
	e.wantKind("SeenUIDs past its bound", err, fault.Invalid)
	_, err = e.p.SeenUIDs(e.ctx, nil, []string{"not a uid"})
	e.wantKind("SeenUIDs of a malformed UID", err, fault.Invalid)
}
