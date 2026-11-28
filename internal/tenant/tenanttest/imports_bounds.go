package tenanttest

import (
	"crypto/sha256"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/importpath"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// noise returns n bytes of base64url text made from seed: no byte a path's
// segment refuses, and too varied for a store to compress, so a path made
// of it takes its full length in an index row (Postgres compresses a long
// index value when it can, which would hide a row too long).
func noise(seed string, n int) string {
	var out []byte
	for i := 0; len(out) < n; i++ {
		h := sha256.Sum256([]byte(seed + "/" + strconv.Itoa(i)))
		out = append(out, base64.RawURLEncoding.EncodeToString(h[:])...)
	}
	return string(out[:n])
}

// boundPath returns a valid path of exactly n bytes under dir, whose first
// segment there starts with tag, the rest noise from seed in segments of
// about 200 bytes.
func boundPath(dir, tag string, n int, seed string) string {
	var b strings.Builder
	b.WriteString(dir + "/" + tag)
	seg, src := len(tag), noise(seed, n)
	for i := 0; b.Len() < n; {
		if seg >= 200 && n-b.Len() >= 2 {
			b.WriteByte('/')
			seg = 0
			continue
		}
		b.WriteByte(src[i])
		i++
		seg++
	}
	return b.String()
}

// testImportPathBound guards the bound of a path a store keeps in an
// index: with a job id of the longest length, a folder, a skipped file, a
// file handed in and a file held as it arrived, each of exactly
// importpath.MaxPathLength bytes of text no store can compress, are all
// kept (Postgres refuses an index row past 2704 bytes); a file or a skip
// past the bound is refused as invalid and records nothing, never failing
// the unit; entries too long are skipped as path_too_long, each counted
// once, listed in one row under the folder that holds them, the unit's
// cursor on the last one's own path; and the job ends partial for them.
func testImportPathBound(e *ienv) {
	e.setupImport()
	id := strings.Repeat("j", master.MaxIDLength)
	e.newImport(id, "b")
	lw := e.claimImport("w", at(1))
	e.start(lw, at(1))
	bound := importpath.MaxPathLength
	folder := boundPath("b", "a", bound, "folder")
	skipped := boundPath("b", "c", bound, "skip")
	arrived := boundPath("b", "d", bound, "arrive")
	heldAt := boundPath("b", "e", bound, "held")
	for _, p := range []string{folder, skipped, arrived, heldAt} {
		if len(p) != bound || importpath.Check(p) != nil {
			e.Fatalf("a path at the bound is %d bytes (%v), want %d and valid", len(p), importpath.Check(p), bound)
		}
	}
	_, err := e.p.AddImportUnits(e.ctx, lw.Fence(), []string{folder}, at(2))
	e.ok("AddImportUnits at the bound", err)
	e.skip(lw, skipped, tenant.SkipNotPart10, "")
	r := e.openReception(importReceiver, 21, at(10))
	f := importFile(r, 21, 1, lw, arrived)
	e.handIn(f)
	e.hold(tenant.QuarantinedFile{ID: "h1", Reason: tenant.QuarantineNotDICOM, Details: map[string]string{"offset": "0"},
		Receiver: importReceiver, Size: 300, Quarantined: at(12),
		Import: &tenant.ImportArrival{Fence: lw.Fence(), Path: heldAt, Size: 300}})
	e.reconciled("at the bound", id)

	long := boundPath("b", "f", bound+1, "long")
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{importFile(r, 21, 2, lw, long)}, at(12), nil)
	e.wantKind("a file past the bound", err, fault.Invalid)
	_, err = e.p.RecordImportSkip(e.ctx, tenant.ImportSkip{Fence: lw.Fence(), Path: long, Code: tenant.SkipNotPart10, Now: at(12)})
	e.wantKind("a skip past the bound not as too long", err, fault.Invalid)
	_, err = e.p.RecordImportSkip(e.ctx, tenant.ImportSkip{Fence: lw.Fence(), Path: "b/g", Code: tenant.SkipPathTooLong, Now: at(12)})
	e.wantKind("a path that fits skipped as too long", err, fault.Invalid)
	past := boundPath("b", "h", importpath.MaxEntryPathLength+1, "past")
	_, err = e.p.RecordImportSkip(e.ctx, tenant.ImportSkip{Fence: lw.Fence(), Path: past, Code: tenant.SkipPathTooLong, Now: at(12)})
	e.wantKind("an entry longer than a walk meets", err, fault.Invalid)
	e.wantImport("refusals record nothing", e.reconciled("refusals record nothing", id), 3, 2, 0, 0, 1)
	e.endWalk(lw, at(13))

	w := e.claimImport("w", at(14))
	if u := e.start(w, at(14)); u.Folder != folder || u.State != tenant.ImportUnitWalking {
		e.Fatalf("the folder's unit is %d bytes of folder, %s; want the folder at the bound, walking", len(u.Folder), u.State)
	}
	first, second := folder+"/a"+noise("first", 20), folder+"/b"+noise("second", 30)
	e.skip(w, first, tenant.SkipPathTooLong, "")
	e.skip(w, first, tenant.SkipPathTooLong, "") // sent again after a lost answer
	e.skip(w, second, tenant.SkipPathTooLong, "")
	units, _, err := e.p.ListImportUnits(e.ctx, id, tenant.ImportUnitFilter{States: []tenant.ImportUnitState{tenant.ImportUnitWalking}},
		master.Page{Limit: 10})
	e.ok("ListImportUnits", err)
	if len(units) != 1 || units[0].Cursor != second || units[0].Counts.Skipped[tenant.SkipPathTooLong] != 2 {
		e.Errorf("the walking unit's cursor is %d bytes with %+v, want the second entry's own path and 2 skipped too long",
			len(units[0].Cursor), units[0].Counts)
	}
	e.endWalk(w, at(15))

	r = e.stepTo(r, reception.Identified, "store")
	e.commit(r, f)
	e.stepTo(r, reception.Done, "store")
	j := e.reconciled("the job ended", id)
	c := j.Import.Counts
	if j.State != tenant.JobPartial || c.Found != 5 || c.Skipped[tenant.SkipPathTooLong] != 2 || c.Skipped[tenant.SkipNotPart10] != 1 ||
		c.Archived != 1 || c.Quarantined[tenant.QuarantineNotDICOM] != 1 {
		e.Errorf("the job is %s with %+v, want partial: 5 found, 2 skipped too long, 1 not Part 10, 1 archived, 1 quarantined", j.State, c)
	}
	files := e.files(id, tenant.ImportFileFilter{})
	want := []string{folder + " skipped", skipped + " skipped", heldAt + " quarantined"}
	if got := paths(files); !slices.Equal(got, want) {
		e.Errorf("%d files listed, want the folder's row for its entries too long, the skip and the held file", len(got))
	} else if files[0].SkipCode != tenant.SkipPathTooLong || files[1].SkipCode != tenant.SkipNotPart10 {
		e.Errorf("the rows' codes are %s and %s, want path_too_long and not_part10", files[0].SkipCode, files[1].SkipCode)
	}
}

// testImportArrivalAfterCancel guards a file handed in after its job was
// canceled: refused as a precondition with ErrImportEnded in the chain,
// whether a received file or one held as it arrived, its batch recording
// nothing; a resend of the file at the cursor, recorded before the cancel,
// still changes nothing; the counts do not move.
func testImportArrivalAfterCancel(e *ienv) {
	e.setupImport()
	w := e.listingOnly("j", "p")
	r := e.openReception(importReceiver, 22, at(10))
	e.handIn(importFile(r, 22, 1, w, "p/a.dcm"))
	_, err := e.p.CancelJob(e.ctx, "j", at(11), e.audit(), nil)
	e.ok("CancelJob", err)
	plain := file(r, 22, 9)
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{plain, importFile(r, 22, 2, w, "p/b.dcm")}, at(12), nil)
	e.wantIs("a file handed in after the cancel", err, tenant.ErrImportEnded, fault.Precondition)
	files, _, err := e.p.ReceivedFiles(e.ctx, r.ID, master.Page{Limit: 10})
	e.ok("ReceivedFiles", err)
	if len(files) != 1 {
		e.Errorf("the reception holds %d files, want 1: a batch refused records nothing", len(files))
	}
	_, err = e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{{ID: "h1", Reason: tenant.QuarantineNotDICOM,
		Details: map[string]string{"offset": "0"}, Receiver: importReceiver, Size: 300, Quarantined: at(12),
		Import: &tenant.ImportArrival{Fence: w.Fence(), Path: "p/c.bin", Size: 300}}}, nil)
	e.wantIs("a file held as it arrived after the cancel", err, tenant.ErrImportEnded, fault.Precondition)
	if _, err := e.p.QuarantinedFile(e.ctx, "h1"); fault.KindOf(err) != fault.NotFound {
		e.Errorf("a refused hold left its record: %v", err)
	}
	e.handIn(importFile(r, 22, 1, w, "p/a.dcm"))
	j := e.reconciled("after the cancel", "j")
	e.wantImport("after the cancel", j, 1, 1, 0, 0, 1)
	if j.State != tenant.JobCanceled {
		e.Errorf("the job is %s, want canceled", j.State)
	}
	e.endWalk(w, at(13))
}

// quarantine steps the reception, complete, to quarantined.
func (e *ienv) quarantine(r tenant.Reception) tenant.Reception {
	e.Helper()
	r, err := e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: r.State, Event: reception.Quarantine,
		At: r.LastFile.Add(time.Minute)}, nil)
	e.ok("StepReception quarantine", err)
	return r
}

// testImportQuarantinedEnd guards the outcome of an import's files at a
// reception's quarantined end. Every file held before the step, as the
// pipeline holds them, is quarantined by its record, and the step leaves
// none to decide. A file left processing anyway is counted failed, and
// listed failed, never already held, since nothing held it.
func testImportQuarantinedEnd(e *ienv) {
	e.setupImport()
	w := e.listingOnly("j", "p")
	ra, rb := e.openReception(importReceiver, 23, at(10)), e.openReception(importReceiver, 24, at(10))
	a1, a2 := importFile(ra, 23, 1, w, "p/a1.dcm"), importFile(ra, 23, 2, w, "p/a2.dcm")
	b1, b2 := importFile(rb, 24, 3, w, "p/b1.dcm"), importFile(rb, 24, 4, w, "p/b2.dcm")
	e.handIn(a1, a2, b1, b2)
	e.endWalk(w, at(13))

	ra = e.stepTo(ra, reception.Complete, "store")
	e.hold(held("qa1", a1, tenant.QuarantineBadLabel, badLabel), held("qa2", a2, tenant.QuarantineBadLabel, badLabel))
	e.quarantine(ra)
	j := e.reconciled("every file held, then quarantined", "j")
	if c := j.Import.Counts; c.Quarantined[tenant.QuarantineBadLabel] != 2 || c.Failed != 0 || c.AlreadyHeld != 0 || c.Processing != 2 {
		e.Errorf("after a reception whose files were all held: %+v, want 2 quarantined, none failed or already held", c)
	}

	rb = e.stepTo(rb, reception.Complete, "store")
	e.hold(held("qb1", b1, tenant.QuarantineBadLabel, badLabel))
	e.quarantine(rb)
	j = e.reconciled("a file left at the quarantine", "j")
	if c := j.Import.Counts; c.Quarantined[tenant.QuarantineBadLabel] != 3 || c.Failed != 1 || c.AlreadyHeld != 0 ||
		c.Processing != 0 || j.State != tenant.JobPartial {
		e.Errorf("the job is %s with %+v, want partial: 3 quarantined, the file left failed, none already held", j.State, c)
	}
	want := []string{"p/a1.dcm quarantined", "p/a2.dcm quarantined", "p/b1.dcm quarantined", "p/b2.dcm failed"}
	if got := paths(e.files("j", tenant.ImportFileFilter{})); !slices.Equal(got, want) {
		e.Errorf("the files listed are %q, want %q", got, want)
	}
}

// testImportEndEventLast guards where an import job's end stands in the
// feed: a job ended by a reception's step to done is ended after that
// step, its event after the step's, on every driver.
func testImportEndEventLast(e *ienv) {
	e.setupImport()
	r0, before := e.identified(importReceiver, 25, 7)
	e.commit(r0, before...)
	e.stepTo(r0, reception.Done, "store")
	w := e.listingOnly("j", "dup")
	r := e.openReception(importReceiver, 25, at(10))
	e.handIn(importFile(r, 25, 7, w, "dup/c.dcm"))
	e.endWalk(w, at(13))
	r = e.stepTo(r, reception.Identified, "store")
	r = e.stepTo(r, reception.Done, "store")
	if j := e.reconciled("ended by the step", "j"); j.State != tenant.JobDone || j.Import.Counts.AlreadyHeld != 1 {
		e.Fatalf("the job is %s with %+v, want done with the file already held", j.State, j.Import.Counts)
	}
	events, _ := e.readAll("", 100)
	step, end := -1, -1
	for i, ev := range events {
		switch p := ev.Payload.(type) {
		case *tenant.ReceptionStepReached:
			if p.Reception == r.ID && p.Step == reception.Done {
				step = i
			}
		case *tenant.JobStepReached:
			if p.Job == "j" && p.State == tenant.JobDone {
				end = i
			}
		}
	}
	if step < 0 || end < 0 || end < step {
		e.Errorf("the reception's step to done is event %d and the job's end event %d, want the end after the step", step, end)
	}
}
