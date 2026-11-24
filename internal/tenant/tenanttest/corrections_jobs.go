package tenanttest

import (
	"slices"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// deletedStream reads a scan unit's whole list of deleted instances after
// the cursor given, a page of limit at a time.
func (e *cenv) deletedStream(w tenant.Work, after string, limit int) []tenant.ScanInstance {
	e.Helper()
	var out []tenant.ScanInstance
	for range 1000 {
		page, err := e.p.DeletedJobInstances(e.ctx, w.Fence(), after, limit)
		e.ok("DeletedJobInstances", err)
		out = append(out, page...)
		if len(page) < limit {
			return out
		}
		after = page[len(page)-1].SOPInstanceUID
	}
	e.Fatal("a list of deleted instances did not end")
	return nil
}

// testJobMeetsDeletes guards a send job whose scans a delete reached
// after the job listed them, before their units started: one instance of
// one scan deleted, and another scan deleted whole. Both units start
// (the scan hidden whole is still its unit's, with its labels as they
// were, rather than refused as no job's on every attempt); each unit's
// stream shows what is held, and DeletedJobInstances lists, in their
// order and over more than one page, the instances the delete hid that
// the job counted, each with its size, reception and held time; a unit
// that refuses those as deleted in the archive ends with the job's counts
// whole (every instance answered, none remaining) and the job partial,
// the deleted ones listed as its failures. An instance archived after the
// job's create time and then deleted is not the job's and is not listed;
// a page outside 1 to 1000 is refused.
func testJobMeetsDeletes(e *cenv) {
	e.addReceiver("recv", "store", 11112)
	e.addSender("peer")
	a := e.archive("recv", 1, 2, 3, 0, at(0), "ses-1")
	e.finish(a)
	partly, whole := "scan-"+uidOf(2, 100), "scan-"+uidOf(2, 101)
	gone := a.files[1]
	held, _, err := e.p.ScanInstances(e.ctx, uidOf(2, 101), master.Page{Limit: 1})
	e.ok("ScanInstances", err)
	if len(held) != 1 {
		e.Fatal("the scan to be deleted whole holds no instance")
	}
	heldAt := held[0].Held
	e.createJob("j1", "peer", tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: "ses-1"}, at(400))
	e.list("peer", at(401))
	// An instance of the whole scan's series archived after the job was
	// made: in neither its stream nor its deleted list.
	late := e.archive("recv", 1, 2, 1, 50, at(450), "ses-1")
	e.finish(late)
	one := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: partly, Instance: gone.SOPInstanceUID}, 500)
	e.commit(one.ID, 502, nil)
	all := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: whole}, 510)
	e.commit(all.ID, 512, nil)

	var j tenant.Job
	started := map[string]bool{}
	for range 2 {
		w := e.claim("peer", "sender", at(600))
		u, err := e.p.StartJobUnit(e.ctx, w.Fence(), at(600))
		e.ok("StartJobUnit of a scan a delete reached", err)
		if u.Listing || u.Scan.Path.Subject != "sub-01" || u.Scan.Path.Session != "ses-1" || u.Scan.Storage != "store" {
			e.Fatalf("a scan's unit started as %+v, want the scan's labels and storage", u)
		}
		started[u.Scan.Path.Scan] = true
		live := e.instances(w, u.Scan.Cursor)
		deleted := e.deletedStream(w, u.Scan.Cursor, 2)
		var wantLive, wantGone []string
		var files []tenant.ReceivedFile
		switch u.Scan.Path.Scan {
		case partly:
			files = a.files[0:3]
			wantLive, wantGone = []string{a.files[0].SOPInstanceUID, a.files[2].SOPInstanceUID}, []string{gone.SOPInstanceUID}
		case whole:
			files = a.files[3:6]
			wantGone = []string{a.files[3].SOPInstanceUID, a.files[4].SOPInstanceUID, a.files[5].SOPInstanceUID}
		default:
			e.Fatalf("a unit of scan %q, want one of the job's two", u.Scan.Path.Scan)
		}
		if got := uidsOf(live); !slices.Equal(got, wantLive) {
			e.Errorf("%s: the stream shows %v, want %v", u.Scan.Path.Scan, got, wantLive)
		}
		if got := uidsOf(deleted); !slices.Equal(got, wantGone) {
			e.Errorf("%s: the deleted instances listed are %v, want %v", u.Scan.Path.Scan, got, wantGone)
		}
		for _, d := range deleted {
			i := slices.IndexFunc(files, func(f tenant.ReceivedFile) bool { return f.SOPInstanceUID == d.SOPInstanceUID })
			if i < 0 || d.Size != files[i].Size || d.ReceptionID != a.r.ID || !d.Held.Equal(heldAt) {
				e.Errorf("%s: deleted instance %+v, want its size, its reception %s and its held time %v", u.Scan.Path.Scan, d,
					a.r.ID, heldAt)
			}
		}
		// The unit answers in the stream's order: what is held sent, what
		// was deleted refused as deleted in the archive.
		merged := append(slices.Clone(live), deleted...)
		slices.SortFunc(merged, func(x, y tenant.ScanInstance) int { return strings.Compare(x.SOPInstanceUID, y.SOPInstanceUID) })
		p := progress(w, u.Scan.Cursor, merged, 0, func(_ int, in tenant.ScanInstance) tenant.JobFailureCode {
			if slices.Contains(wantGone, in.SOPInstanceUID) {
				return tenant.FailureDeletedInArchive
			}
			return ""
		}, at(601))
		_, err = e.p.RecordJobProgress(e.ctx, p)
		e.ok("RecordJobProgress", err)
		j, err = e.p.EndJobScan(e.ctx, tenant.JobProgress{Fence: w.Fence(), From: p.To, To: p.To,
			FailuresKept: tenant.DefaultFailuresKept, Now: at(602)})
		e.ok("EndJobScan", err)
		_, err = e.p.DeletedJobInstances(e.ctx, w.Fence(), "", 0)
		e.wantKind("DeletedJobInstances of a page of none", err, fault.Invalid)
		_, err = e.p.DeletedJobInstances(e.ctx, w.Fence(), "", tenant.MaxJobInstancePage+1)
		e.wantKind("DeletedJobInstances of a page past the most", err, fault.Invalid)
	}
	if !started[partly] || !started[whole] {
		e.Fatalf("units of %v started, want both scans'", started)
	}
	if _, err := e.p.ClaimWork(e.ctx, tenant.SendKind("peer"), "sender", at(700), time.Minute); fault.KindOf(err) != fault.NotFound {
		e.Fatalf("a unit is still due after both ended (%v): a unit of a scan deleted whole is tried again", err)
	}
	e.wantCounts("a job over deleted instances", j, 6, 2, 0, 4, 2, 2, true)
	if j.State != tenant.JobPartial || j.Counts.InstancesRemaining() != 0 {
		e.Errorf("the job ended %s with %d remaining, want partial with none", j.State, j.Counts.InstancesRemaining())
	}
	failures := allPages(e.penv, 10, func(pg master.Page) ([]tenant.JobFailure, string, error) {
		return e.p.ListJobFailures(e.ctx, j.ID, false, pg)
	})
	if len(failures) != 4 {
		e.Fatalf("the job lists %d failures, want the four deleted instances", len(failures))
	}
	for _, f := range failures {
		if f.Code != tenant.FailureDeletedInArchive {
			e.Errorf("failure %+v, want deleted in the archive", f)
		}
	}
}
