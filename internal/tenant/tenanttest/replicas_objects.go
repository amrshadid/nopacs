package tenanttest

import (
	"slices"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// objectsOf pages through StorageObjects of q, limit at a time, failing the
// test on an error.
func (e *renv) objectsOf(q tenant.StorageObjectQuery, limit int) []tenant.StoredObject {
	e.Helper()
	var out []tenant.StoredObject
	page := master.Page{Limit: limit}
	for {
		got, next, err := e.p.StorageObjects(e.ctx, q, page)
		e.ok("StorageObjects", err)
		if len(got) > limit {
			e.Fatalf("a page of %d objects answered %d", limit, len(got))
		}
		out = append(out, got...)
		if next == "" {
			return out
		}
		page.Cursor = next
	}
}

// testStorageObjects guards StorageObjects: a primary's objects are its
// scans' instances, each with its key, size and archived checksum, in
// order of scan then SOP Instance UID and paged without a gap or a repeat;
// a path narrows them to a subject, a session or a scan; a replica answers
// only the objects of its scans in step; a storage the tenant does not
// have is NotFound.
func testStorageObjects(e *renv) {
	e.replicaSetup()
	all := e.objectsOf(tenant.StorageObjectQuery{Storage: "store"}, 3)
	if len(all) != 4 {
		e.Fatalf("the primary's objects are %d, want its 4 instances", len(all))
	}
	for i, o := range all {
		want, err := layout.InstanceKey(o.Path, o.SOPInstanceUID)
		e.ok("InstanceKey", err)
		if o.Storage != "store" || o.Key != want || o.Size <= 0 || o.ArchivedSHA256.IsZero() || o.Path.Subject != "sub-01" ||
			o.Path.Session != "ses-1" || (o.SeriesUID != seriesA && o.SeriesUID != seriesB) {
			e.Errorf("object %d is %+v", i, o)
		}
		if i > 0 && all[i-1].SeriesUID == o.SeriesUID && all[i-1].SOPInstanceUID >= o.SOPInstanceUID {
			e.Errorf("the objects of a scan are not in order of SOP Instance UID: %s then %s", all[i-1].SOPInstanceUID, o.SOPInstanceUID)
		}
	}
	keys := make([]string, len(all))
	for i, o := range all {
		keys[i] = o.Key
	}
	if len(slices.Compact(slices.Sorted(slices.Values(keys)))) != 4 {
		e.Errorf("the pages answered an object twice: %v", keys)
	}
	scanA := all[0].Path
	if got := e.objectsOf(tenant.StorageObjectQuery{Storage: "store", Path: scanA}, 10); len(got) != 2 || got[0].Path != scanA || got[1].Path != scanA {
		e.Errorf("the objects of one scan are %+v, want its 2", got)
	}
	if got := e.objectsOf(tenant.StorageObjectQuery{Storage: "store", Path: label.Path{Subject: "sub-01", Session: "ses-1"}}, 10); len(got) != 4 {
		e.Errorf("the objects of the session are %d, want 4", len(got))
	}
	if got := e.objectsOf(tenant.StorageObjectQuery{Storage: "store", Path: label.Path{Subject: "sub-99"}}, 10); len(got) != 0 {
		e.Errorf("a subject the catalog does not hold answered %d objects", len(got))
	}
	e.attach("copy", "store", 10)
	if got := e.objectsOf(tenant.StorageObjectQuery{Storage: "copy"}, 10); len(got) != 0 {
		e.Errorf("a replica with no scan in step answered %d objects", len(got))
	}
	e.copyAll(20)
	got := e.objectsOf(tenant.StorageObjectQuery{Storage: "copy"}, 10)
	if len(got) != 4 || got[0].Storage != "copy" || got[0].Key != all[0].Key || got[0].ArchivedSHA256 != all[0].ArchivedSHA256 {
		e.Errorf("the replica's objects in step are %+v, want the primary's 4 under its name", got)
	}
	_, _, err := e.p.StorageObjects(e.ctx, tenant.StorageObjectQuery{Storage: "gone"}, master.Page{Limit: 10})
	e.wantKind("StorageObjects of a storage the tenant does not have", err, fault.NotFound)
	_, _, err = e.p.StorageObjects(e.ctx, tenant.StorageObjectQuery{Storage: "store", Path: label.Path{Scan: "x"}}, master.Page{Limit: 10})
	e.wantKind("StorageObjects of a scan without its subject", err, fault.Invalid)
}

// archiveWithoutDigest archives, through the receiver recv into "store",
// one scan of study study of two instances committed with no manifest
// digest (as a scan archived before manifests were digested is held), and
// returns its series.
func (e *renv) archiveWithoutDigest(study int, at time.Time, session string) string {
	e.Helper()
	r, _, err := e.p.OpenReception(e.ctx, "recv", uidOf(1, study), e.id("rc"), at)
	e.ok("OpenReception", err)
	series := uidOf(2, study*100)
	var files []tenant.ReceivedFile
	for i := range 2 {
		f := file(r, study, 50000+study*10+i)
		f.SeriesUID = series
		files = append(files, f)
	}
	_, err = e.p.AddReceivedFiles(e.ctx, files, at)
	e.ok("AddReceivedFiles", err)
	step := func(from reception.State, ev reception.Event) {
		e.Helper()
		st := tenant.ReceptionStep{ID: r.ID, From: from, Event: ev, At: r.StepTime.Add(time.Minute)}
		if ev == reception.Identify {
			st.SubjectLabel, st.SessionLabel, st.Storage = "sub-01", session, "store"
		}
		r, err = e.p.StepReception(e.ctx, st, nil)
		e.ok("StepReception "+string(ev), err)
	}
	step(reception.Receiving, reception.Quiet)
	step(reception.Complete, reception.Identify)
	claims := make([]tenant.InstanceClaim, len(files))
	for i, f := range files {
		claims[i] = tenant.InstanceClaim{SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SHA256: f.SHA256}
	}
	_, err = e.p.ClaimInstances(e.ctx, r.ID, claims, r.StepTime, nil)
	e.ok("ClaimInstances", err)
	p := label.Path{Subject: "sub-01", Session: session, Scan: "scan-" + series}
	c := tenant.ScanCommit{ReceptionID: r.ID, StudyUID: r.StudyUID, SeriesUID: series, SubjectLabel: p.Subject, SessionLabel: p.Session,
		ScanLabel: p.Scan, Storage: "store", At: r.StepTime}
	for _, f := range files {
		key, err := layout.InstanceKey(p, f.SOPInstanceUID)
		e.ok("InstanceKey", err)
		c.Instances = append(c.Instances, tenant.ConfirmedInstance{SOPInstanceUID: f.SOPInstanceUID, Key: key, Length: f.Size, Size: f.Size,
			ArchivedSHA256: f.SHA256})
	}
	_, err = e.p.CommitScan(e.ctx, c)
	e.ok("CommitScan", err)
	step(reception.Identified, reception.Archive)
	e.finish(archived{r: r, files: files})
	return series
}

// testStorageObjectsAfterSwap guards StorageObjects over the rows a
// switch's swap records in step at the catalog's digest: a scan held with
// no manifest digest is in step on the replica at none, as a read that
// turns to the replica judges it, so its objects are answered as the
// replica's with the others'.
func testStorageObjectsAfterSwap(e *renv) {
	e.replicaSetup()
	e.archiveWithoutDigest(3, at(1), "ses-3")
	e.attach("copy", "store", 10)
	for _, c := range e.claimAll(20) {
		_, err := e.p.EndReplicaScan(e.ctx, c.Fence, c.ManifestSHA256, at(21))
		e.ok("EndReplicaScan", err)
	}
	_, _, err := e.p.BeginSwitch(e.ctx, tenant.SwitchStart{ID: "sw-1", Primary: "store", Replica: "copy", Token: "tok-1", Now: at(30)},
		e.audit(), nil)
	e.ok("BeginSwitch", err)
	_, err = e.p.SwitchStep(e.ctx, "sw-1", tenant.SwitchDraining, at(32))
	e.ok("SwitchStep", err)
	_, err = e.p.SwapStorages(e.ctx, tenant.SwitchSwap{ID: "sw-1", Now: at(40)}, e.serverAudit())
	e.ok("SwapStorages", err)
	got := e.objectsOf(tenant.StorageObjectQuery{Storage: "copy"}, 10)
	sessions := map[string]int{}
	for _, o := range got {
		sessions[o.Path.Session]++
	}
	if len(got) != 6 || sessions["ses-3"] != 2 {
		e.Errorf("the replica's objects after the swap are %d (by session %v), want 6 with the 2 of the scan held with no digest", len(got), sessions)
	}
}

// testBackfillAfterCanceledJob guards the count of a backfill made after
// the replica's replicate job was canceled: the cancel leaves the scans
// owed (the copy is the replica's state, not the job's), so the new job
// counts every scan the replica owes, and ends done once each is copied.
func testBackfillAfterCanceledJob(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	sched, _, err := e.p.ReplicateJob(e.ctx, "copy")
	e.ok("ReplicateJob", err)
	_, err = e.p.CancelJob(e.ctx, sched.ID, at(11), e.audit(), nil)
	e.ok("CancelJob", err)
	j, made, err := e.p.BackfillReplica(e.ctx, tenant.ReplicaBackfill{Replica: "copy", JobID: "J0000000000d1", Now: at(12)}, e.audit(), nil)
	e.ok("BackfillReplica", err)
	if !made || j.Counts.Scans != 2 || j.Counts.ScansDone != 0 {
		e.Errorf("a backfill after the canceled job counts %d scans (%d done, made %v), want the 2 the replica owes", j.Counts.Scans,
			j.Counts.ScansDone, made)
	}
	e.copyAll(20)
	if j, err := e.p.Job(e.ctx, "J0000000000d1"); err != nil || j.State != tenant.JobDone || j.Counts.ScansDone != 2 {
		e.Errorf("the backfill's job after the copy is %+v, %v; want done with 2 scans done", j, err)
	}
}

// testRecallsOf guards RecallsOf: the rows of the keys asked, by key, a key
// with no row absent, and more keys than a batch refused.
func testRecallsOf(e *renv) {
	e.recallSetup()
	e.ask(10, false, "data/a.dcm", "data/b.dcm")
	got, err := e.p.RecallsOf(e.ctx, "cold", []string{"data/a.dcm", "data/z.dcm"})
	e.ok("RecallsOf", err)
	if len(got) != 1 || got["data/a.dcm"].State != tenant.RecallRequested || got["data/a.dcm"].Key != "data/a.dcm" {
		e.Errorf("RecallsOf answered %+v, want the one row asked for", got)
	}
	if got, err := e.p.RecallsOf(e.ctx, "cold", nil); err != nil || len(got) != 0 {
		e.Errorf("RecallsOf of no key answered %+v, %v", got, err)
	}
	many := make([]string, tenant.MaxRecallBatch+1)
	for i := range many {
		many[i] = "data/k" + string(rune('a'+i%26)) + ".dcm"
	}
	_, err = e.p.RecallsOf(e.ctx, "cold", many)
	e.wantKind("RecallsOf of more keys than a batch", err, fault.Invalid)
}
