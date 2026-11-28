package tenanttest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// runCorrections runs the tests of corrections: every transition of the
// state machine and every refusal, what a delete hides and an undo shows
// again, relabel and replace, purge and its record of objects, what
// deleted data matches, and the answers of ResolveInstances.
func runCorrections(t *testing.T, open OpenEvents) {
	tests := []struct {
		name string
		fn   func(*cenv)
	}{
		{"DeleteSubjectAndUndo", testDeleteSubjectAndUndo},
		{"DeleteUndoKeepsAETitles", testDeleteUndoKeepsAETitles},
		{"DeleteRefusals", testDeleteRefusals},
		{"DeleteInstance", testDeleteInstance},
		{"DeleteLastInstanceIsTheScan", testDeleteLastInstance},
		{"UndoAfterItsIDsWereTaken", testUndoAfterIDsTaken},
		{"PurgeRecordsObjects", testPurgeRecordsObjects},
		{"AbandonAndPending", testAbandonAndPending},
		{"Relabel", testRelabel},
		{"RelabelRefusals", testRelabelRefusals},
		{"RelabeledTo", testRelabeledTo},
		{"RelabeledToChains", testRelabeledToChains},
		{"Replace", testReplace},
		{"ReplaceProvenance", testReplaceProvenance},
		{"Edit", testEdit},
		{"KeepRolledBack", testKeepRolledBack},
		{"AbandonEditUndo", testAbandonEditUndo},
		{"ListCorrections", testListCorrections},
		{"DeleteHoldsItsLabels", testDeleteHoldsItsLabels},
		{"ClockAndPurgeTargets", testClockAndPurgeTargets},
		{"JobMeetsDeletes", testJobMeetsDeletes},
		{"QuotaCounts", testQuotaCounts},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := open(t, SuiteTenant)
			tt.fn(&cenv{senv: &senv{eenv: &eenv{penv: &penv{env: &env{T: t, s: s, ctx: context.Background()}, p: s}, ev: s}}})
		})
	}
}

// cenv is a correction test's store and its helpers; it has senv's too.
type cenv struct {
	*senv
	ids int
}

// grace is the grace period the suite publishes with.
const grace = 7 * 24 * time.Hour

// newID returns a new operation id made at second s, distinct from the
// test's others.
func (e *cenv) newID(s int) string {
	e.Helper()
	e.ids++
	id, err := tenant.NewCorrectionID(at(s).Add(time.Duration(e.ids) * time.Millisecond))
	e.ok("NewCorrectionID", err)
	return id
}

// setupTwo archives two sessions of the subject sub-01 at the receiver
// recv: ses-1 of study 1 with two scans of three instances, ses-2 of study
// 2 with one scan of two, both receptions done.
func (e *cenv) setupTwo() (archived, archived) {
	e.Helper()
	e.addReceiver("recv", "store", 11112)
	a := e.archive("recv", 1, 2, 3, 0, at(0), "ses-1")
	e.finish(a)
	b := e.archive("recv", 2, 1, 2, 10000, at(100), "ses-2")
	e.finish(b)
	return a, b
}

// totals reads the catalog's totals.
func (e *cenv) totals() tenant.CatalogCounts {
	e.Helper()
	c, err := e.p.CatalogTotals(e.ctx)
	e.ok("CatalogTotals", err)
	return c
}

// prepareDelete previews and prepares a delete of target at second s.
func (e *cenv) prepareDelete(target tenant.CorrectionTarget, s int) tenant.Correction {
	e.Helper()
	p, err := e.p.PreviewDelete(e.ctx, target, at(s))
	e.ok("PreviewDelete", err)
	c, made, err := e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(s), Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: p.Confirm, Now: at(s + 1)}, e.audit(), nil)
	e.ok("PrepareCorrection of a delete", err)
	if !made || c.State != record.StatePrepared {
		e.Fatalf("a new delete is %+v (made %v), want prepared", c, made)
	}
	return c
}

// commit publishes a correction at second s.
func (e *cenv) commit(id string, s int, manifests map[string]tenant.SHA256) tenant.Correction {
	e.Helper()
	c, err := e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: id, Now: at(s), Grace: grace, Manifests: manifests})
	e.ok("CommitCorrection", err)
	if c.State != record.StateCommitted {
		e.Fatalf("a published correction is %s", c.State)
	}
	return c
}

// wantState fails unless the correction of that id is in state.
func (e *cenv) wantState(id string, state record.State) tenant.Correction {
	e.Helper()
	c, err := e.p.Correction(e.ctx, id)
	e.ok("Correction", err)
	if c.State != state {
		e.Fatalf("correction %s is %s, want %s", id, c.State, state)
	}
	return c
}

// matches looks up what a probe matches.
func (e *cenv) matches(p tenant.DeletedProbe) tenant.DeletedMatch {
	e.Helper()
	m, err := e.p.DeletedMatches(e.ctx, p)
	e.ok("DeletedMatches", err)
	return m
}

// changedEvents returns the catalog_changed events of the feed.
func (e *cenv) changedEvents() []*tenant.CatalogChanged {
	e.Helper()
	all, _ := e.readAll("", 1000, tenant.EventCatalogChanged)
	var out []*tenant.CatalogChanged
	for _, ev := range all {
		if p, ok := ev.Payload.(*tenant.CatalogChanged); ok {
			out = append(out, p)
		}
	}
	return out
}

// testDeleteSubjectAndUndo guards a subject's delete: the preview's counts;
// nothing hidden while prepared, though its arrivals already match; every
// row hidden and every total moved by the publish, with its event; a
// publish run again changes nothing; the undo shows every row again as it
// was, totals recounted, and its targets stop matching.
func testDeleteSubjectAndUndo(e *cenv) {
	a, _ := e.setupTwo()
	before := e.totals()
	target := tenant.CorrectionTarget{Subject: "sub-01"}
	p, err := e.p.PreviewDelete(e.ctx, target, at(500))
	e.ok("PreviewDelete", err)
	if want := (tenant.CorrectionCounts{Subjects: 1, Sessions: 2, Scans: 3, Instances: 8, Bytes: p.Counts.Bytes}); p.Counts != want ||
		p.Counts.Bytes <= 0 || len(p.Storages) != 1 || p.Storages[0] != "store" {
		e.Fatalf("the preview is %+v, want %+v on store", p, want)
	}
	c := e.prepareDelete(target, 500)
	if e.totals() != before {
		e.Fatal("a prepared delete changed the catalog")
	}
	m := e.matches(tenant.DeletedProbe{Subject: "sub-01", StudyUID: uidOf(1, 1), Series: []string{uidOf(2, 100)},
		Instances: []string{a.files[0].SOPInstanceUID}})
	if m.Subject != c.ID || m.Study != c.ID || m.Series[uidOf(2, 100)] != c.ID || m.Of(uidOf(2, 100), a.files[0].SOPInstanceUID) != c.ID {
		e.Fatalf("a prepared subject's delete matches %+v, want its id %s for the subject, study and series", m, c.ID)
	}
	scans, err := e.p.CorrectionScans(e.ctx, c.ID)
	e.ok("CorrectionScans", err)
	if len(scans) != 3 || scans[0].Storage != "store" || scans[0].NewPath != scans[0].Path {
		e.Fatalf("the delete's scans are %+v, want three on store", scans)
	}
	c = e.commit(c.ID, 600, nil)
	if !c.CommitTime.Equal(at(600)) || !c.PurgeAfter.Equal(at(600).Add(grace)) {
		e.Fatalf("the publish fixed %v and %v, want %v and the grace after", c.CommitTime, c.PurgeAfter, at(600))
	}
	if got := e.totals(); got != (tenant.CatalogCounts{}) {
		e.Fatalf("after the publish the catalog totals %+v, want nothing", got)
	}
	e.wantTotalsRecounted("after the publish")
	if _, err := e.p.CatalogScan(e.ctx, label.Path{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + uidOf(2, 100)}); fault.KindOf(err) != fault.NotFound {
		e.Fatalf("a hidden scan is read: %v", err)
	}
	if again := e.commit(c.ID, 700, nil); !again.CommitTime.Equal(c.CommitTime) {
		e.Fatal("a publish run again moved the commit time")
	}
	ev := e.changedEvents()
	if len(ev) != 1 || ev[0].Correction != c.ID || ev[0].State != record.StateCommitted || ev[0].Instances != 8 || ev[0].SubjectLabel != "sub-01" {
		e.Fatalf("the publish's events are %+v, want one committed of 8 instances", ev)
	}
	c, err = e.p.UndoCorrection(e.ctx, c.ID, "the wrong subject", at(800), e.audit(), nil)
	e.ok("UndoCorrection", err)
	if c.State != record.StateUndone || c.UndoReason != "the wrong subject" || !c.Ended.Equal(at(800)) {
		e.Fatalf("an undone delete is %+v", c)
	}
	if got := e.totals(); got != before {
		e.Fatalf("after the undo the catalog totals %+v, want %+v", got, before)
	}
	e.wantTotalsRecounted("after the undo")
	held := allPages(e.penv, 1000, func(pg master.Page) ([]tenant.ScanInstance, string, error) {
		return e.p.ListHeldInstances(e.ctx, label.Path{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + uidOf(2, 100)}, pg)
	})
	if len(held) != 3 || held[0].Key == "" {
		e.Fatalf("after the undo the scan lists %+v, want its three instances", held)
	}
	if m := e.matches(tenant.DeletedProbe{Subject: "sub-01"}); m.Subject != "" {
		e.Fatal("an undone delete still matches")
	}
	again, err := e.p.UndoCorrection(e.ctx, c.ID, "again", at(900), e.audit(), nil)
	e.ok("UndoCorrection again", err)
	if again.State != record.StateUndone || again.UndoReason != "the wrong subject" {
		e.Fatalf("an undo asked again answers %+v", again)
	}
	_, err = e.p.StartPurge(e.ctx, c.ID, at(900), nil, nil)
	e.wantKind("StartPurge of an undone delete", err, fault.Precondition)
}

// testDeleteUndoKeepsAETitles guards the AE titles a session keeps from the
// first file that made it: a delete hides the session with them and its
// undo shows them again, so a dry run of routing reads the same titles
// before and after.
func testDeleteUndoKeepsAETitles(e *cenv) {
	e.addReceiver("recv", "store", 11112)
	route := tenant.ReceptionRoute{CallingAE: "SCANNER1", CalledAE: "ARCHIVE"}
	e.finish(e.archiveRouted("recv", 1, 1, 1, 0, at(0), "ses-1", route))
	titles := func(when string) {
		e.Helper()
		got, err := e.p.RecentSessions(e.ctx, "recv", 10)
		e.ok("RecentSessions", err)
		if len(got) != 1 || got[0].Route.CallingAE != "SCANNER1" || got[0].Route.CalledAE != "ARCHIVE" {
			e.Fatalf("%s the session reads %+v, want the AE titles SCANNER1 and ARCHIVE", when, got)
		}
	}
	titles("before the delete")
	c := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01"}, 500)
	c = e.commit(c.ID, 600, nil)
	if got, err := e.p.RecentSessions(e.ctx, "recv", 10); err != nil || len(got) != 0 {
		e.Fatalf("after the delete the receiver's sessions are %+v (%v), want none", got, err)
	}
	_, err := e.p.UndoCorrection(e.ctx, c.ID, "the wrong subject", at(800), e.audit(), nil)
	e.ok("UndoCorrection", err)
	titles("after the undo")
}

// testDeleteRefusals guards what a delete refuses: a token of another
// preview, one too old, one no longer matching what would go; a target
// the catalog does not hold; a second delete overlapping one not ended; a
// reception of the target not ended; a purge before the grace ends. A
// token used again answers the delete it made.
func testDeleteRefusals(e *cenv) {
	e.setupTwo()
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}
	prep := func(confirm string, s int) (tenant.Correction, bool, error) {
		return e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(s), Kind: record.KindDelete, Target: target,
			Reason: "consent withdrawn", Confirm: confirm, Now: at(s)}, e.audit(), nil)
	}
	_, _, err := prep("c1.1.0000000000000000000000000000000000000000000000000000000000000000", 500)
	e.wantKind("a token of no preview", err, fault.Precondition)
	p, err := e.p.PreviewDelete(e.ctx, target, at(500))
	e.ok("PreviewDelete", err)
	_, _, err = prep(p.Confirm, 500+3601)
	e.wantKind("a token more than an hour old", err, fault.Precondition)
	_, err = e.p.PreviewDelete(e.ctx, tenant.CorrectionTarget{Subject: "sub-09"}, at(500))
	e.wantKind("a preview of a subject not held", err, fault.NotFound)
	_, err = e.p.PreviewDelete(e.ctx, tenant.CorrectionTarget{Subject: "sub-01", Scan: "x"}, at(500))
	e.wantKind("a target that skips a level", err, fault.Invalid)
	// Data archived under the target since the preview: the token no
	// longer matches.
	late := e.archive("recv", 1, 1, 1, 50000, at(510), "ses-1")
	_, _, err = prep(p.Confirm, 520)
	e.wantKind("a token that no longer matches", err, fault.Precondition)
	p, err = e.p.PreviewDelete(e.ctx, target, at(530))
	e.ok("PreviewDelete", err)
	_, _, err = prep(p.Confirm, 531)
	e.wantKind("a delete while a reception of its study has not ended", err, fault.Precondition)
	e.finish(late)
	p, err = e.p.PreviewDelete(e.ctx, target, at(540))
	e.ok("PreviewDelete", err)
	c, made, err := prep(p.Confirm, 541)
	e.ok("PrepareCorrection", err)
	again, made2, err := prep(p.Confirm, 542)
	e.ok("PrepareCorrection with a token used", err)
	if !made || made2 || again.ID != c.ID {
		e.Fatalf("a token used again answers %s (made %v), want %s", again.ID, made2, c.ID)
	}
	other, err := e.p.PreviewDelete(e.ctx, tenant.CorrectionTarget{Subject: "sub-01"}, at(550))
	e.ok("PreviewDelete of the subject", err)
	_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(551), Kind: record.KindDelete,
		Target: tenant.CorrectionTarget{Subject: "sub-01"}, Reason: "r", Confirm: other.Confirm, Now: at(551)}, e.audit(), nil)
	e.wantKind("a delete overlapping one prepared", err, fault.Precondition)
	_, err = e.p.UndoCorrection(e.ctx, c.ID, "r", at(552), e.audit(), nil)
	e.wantKind("an undo of a prepared delete", err, fault.Precondition)
	_, err = e.p.StartPurge(e.ctx, c.ID, at(552), nil, nil)
	e.wantKind("a purge of a prepared delete", err, fault.Precondition)
	e.commit(c.ID, 600, nil)
	_, err = e.p.StartPurge(e.ctx, c.ID, at(601), nil, nil)
	e.wantKind("a server's purge before the grace ends", err, fault.Precondition)
	_, err = e.p.AbandonCorrection(e.ctx, c.ID, "stopped", at(602))
	e.wantKind("an abandon of a committed delete", err, fault.Precondition)
	_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: c.ID, Kind: record.KindDelete, Target: target,
		Reason: "\x07bell", Confirm: p.Confirm, Now: at(603)}, e.audit(), nil)
	e.wantKind("a reason with a control character", err, fault.Invalid)
}

// testDeleteInstance guards one instance's delete: only that instance
// hidden, its scan's other instances listed throughout and its counts
// moved; its purge gives the scan the manifest written without it, and
// ResolveInstances answers it deleted, its bytes gone.
func testDeleteInstance(e *cenv) {
	a, _ := e.setupTwo()
	in := a.files[1]
	scan := label.Path{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + uidOf(2, 100)}
	target := tenant.CorrectionTarget{Subject: scan.Subject, Session: scan.Session, Scan: scan.Scan, Instance: in.SOPInstanceUID}
	c := e.prepareDelete(target, 500)
	if c.Level != record.LevelInstance || c.Counts.Instances != 1 || c.InstanceKey == "" || c.InstanceSize != in.Size {
		e.Fatalf("an instance's delete is %+v", c)
	}
	before := e.totals()
	e.commit(c.ID, 600, nil)
	held := allPages(e.penv, 1000, func(pg master.Page) ([]tenant.ScanInstance, string, error) {
		return e.p.ListHeldInstances(e.ctx, scan, pg)
	})
	if len(held) != 2 || held[0].SOPInstanceUID == in.SOPInstanceUID || held[1].SOPInstanceUID == in.SOPInstanceUID {
		e.Fatalf("after the publish the scan lists %d instances, want the two others", len(held))
	}
	if got := e.totals(); got.Instances != before.Instances-1 || got.Scans != before.Scans {
		e.Fatalf("after the publish the totals are %+v, want one instance fewer of %+v", got, before)
	}
	e.wantTotalsRecounted("after an instance's delete")
	r, err := e.p.ResolveInstances(e.ctx, []tenant.InstanceRef{{UID: in.SOPInstanceUID, Revision: 1}, {UID: a.files[0].SOPInstanceUID,
		Revision: 1}, {UID: a.files[0].SOPInstanceUID, Revision: 2}, {UID: uidOf(3, 99999), Revision: 1}})
	e.ok("ResolveInstances", err)
	if r[0].Disposition != tenant.ResolvedDeleted || r[0].Correction != c.ID || !r[0].BytesKept ||
		r[1].Disposition != tenant.ResolvedCurrent || r[1].Path != scan || r[1].CurrentRevision != 1 ||
		r[2].Disposition != tenant.ResolvedUnknown || r[3].Disposition != tenant.ResolvedUnknown {
		e.Fatalf("resolve answers %+v", r)
	}
	_, err = e.p.StartPurge(e.ctx, c.ID, at(700), &master.AuditEntry{ID: e.id("a"), Time: at(700), Method: "PurgeCorrection",
		Outcome: master.OutcomeDone}, nil)
	e.ok("StartPurge by an admin", err)
	e.ok("RecordPurgedObject", e.p.RecordPurgedObject(e.ctx, tenant.PurgedObject{ID: c.ID, Storage: "store", Key: c.InstanceKey,
		Size: c.InstanceSize, SHA256: in.SHA256, Disposition: tenant.DispositionRemoved, Now: at(701)}))
	read, err := e.p.CatalogScan(e.ctx, scan)
	e.ok("CatalogScan", err)
	f, err := e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: c.ID, Now: at(702), Manifest: sum(77), ManifestBefore: read.ManifestSHA256})
	e.ok("FinishPurge", err)
	if f.State != record.StatePurged || f.Counts.ObjectsRemoved != 1 || f.Counts.BytesRemoved != in.Size ||
		len(f.Dispositions) != 1 || f.Dispositions[0].Removed != 1 {
		e.Fatalf("a purged instance's delete is %+v", f)
	}
	sc, err := e.p.CatalogScan(e.ctx, scan)
	e.ok("CatalogScan", err)
	if sc.ManifestSHA256 != sum(77) || sc.Instances != 2 {
		e.Fatalf("after the purge the scan is %+v, want the new manifest and two instances", sc)
	}
	r, err = e.p.ResolveInstances(e.ctx, []tenant.InstanceRef{{UID: in.SOPInstanceUID, Revision: 1}})
	e.ok("ResolveInstances after the purge", err)
	if r[0].Disposition != tenant.ResolvedDeleted || r[0].BytesKept || r[0].SHA256 != in.SHA256 {
		e.Fatalf("after the purge resolve answers %+v", r[0])
	}
	if m := e.matches(tenant.DeletedProbe{Instances: []string{in.SOPInstanceUID}}); m.Instances[in.SOPInstanceUID] != c.ID {
		e.Fatal("a purged instance's delete no longer matches its arrival")
	}
	e.wantTotalsRecounted("after an instance's purge")
	// A second instance's purge whose manifest the catalog no longer holds
	// (a reception wrote the scan since the purge read it): the scan keeps
	// the newer manifest's checksum.
	other := a.files[2]
	d := e.prepareDelete(tenant.CorrectionTarget{Subject: scan.Subject, Session: scan.Session, Scan: scan.Scan, Instance: other.SOPInstanceUID}, 800)
	e.commit(d.ID, 801, nil)
	_, err = e.p.StartPurge(e.ctx, d.ID, at(802), &master.AuditEntry{ID: e.id("a"), Time: at(802), Method: "PurgeCorrection",
		Outcome: master.OutcomeDone}, nil)
	e.ok("StartPurge by an admin", err)
	_, err = e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: d.ID, Now: at(803), Manifest: sum(78), ManifestBefore: sum(76)})
	e.ok("FinishPurge", err)
	if sc, _ := e.p.CatalogScan(e.ctx, scan); sc.ManifestSHA256 != sum(77) {
		e.Fatalf("a purge's end gave the scan %x though the catalog no longer held the manifest the purge read", sc.ManifestSHA256)
	}
}

// testDeleteLastInstance guards that a delete of a scan's last instance is
// the delete of the scan, and of a session's last scan takes the session.
func testDeleteLastInstance(e *cenv) {
	e.addReceiver("recv", "store", 11112)
	a := e.archive("recv", 1, 1, 1, 0, at(0), "ses-1")
	e.finish(a)
	b := e.archive("recv", 2, 1, 1, 10000, at(100), "ses-2")
	e.finish(b)
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + uidOf(2, 100), Instance: a.files[0].SOPInstanceUID}
	c := e.prepareDelete(target, 500)
	if c.Level != record.LevelScan || c.Counts.Scans != 1 || c.Counts.Sessions != 1 || c.Counts.Subjects != 0 {
		e.Fatalf("a delete of a scan's last instance is %+v, want its scan and session", c)
	}
	e.commit(c.ID, 600, nil)
	if got := e.totals(); got != (tenant.CatalogCounts{Subjects: 1, Sessions: 1, Scans: 1, Instances: 1}) {
		e.Fatalf("after the publish the totals are %+v", got)
	}
	e.wantTotalsRecounted("after a last instance's delete")
	if m := e.matches(tenant.DeletedProbe{Series: []string{uidOf(2, 100)}}); m.Series[uidOf(2, 100)] != c.ID {
		e.Fatal("a delete of a scan's last instance does not match its series")
	}
}

// testUndoAfterIDsTaken guards an undo after rows archived meanwhile took
// the integer ids the hidden rows had (a database that gives a removed
// row's id again): every row comes back under the ids free, its instances
// beneath it, and the rows archived meanwhile keep theirs.
func testUndoAfterIDsTaken(e *cenv) {
	e.setupTwo()
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-2"}
	c := e.prepareDelete(target, 500)
	e.commit(c.ID, 600, nil)
	late := e.archive("recv", 3, 1, 4, 20000, at(700), "ses-3")
	e.finish(late)
	before := e.totals()
	_, err := e.p.UndoCorrection(e.ctx, c.ID, "the wrong session", at(800), e.audit(), nil)
	e.ok("UndoCorrection", err)
	if got, want := e.totals(), (tenant.CatalogCounts{Subjects: 1, Sessions: before.Sessions + 1, Scans: before.Scans + 1,
		Instances: before.Instances + 2}); got != want {
		e.Fatalf("after the undo the totals are %+v, want %+v", got, want)
	}
	for _, x := range []struct {
		session string
		series  int
		n       int
	}{{"ses-2", 200, 2}, {"ses-3", 300, 4}} {
		p := label.Path{Subject: "sub-01", Session: x.session, Scan: "scan-" + uidOf(2, x.series)}
		held := allPages(e.penv, 1000, func(pg master.Page) ([]tenant.ScanInstance, string, error) {
			return e.p.ListHeldInstances(e.ctx, p, pg)
		})
		if len(held) != x.n {
			e.Fatalf("after the undo %s lists %d instances, want %d", p.Session, len(held), x.n)
		}
	}
	e.wantTotalsRecounted("after an undo whose ids were taken")
}

// testPurgeRecordsObjects guards a purge: due once its grace ends; each
// object recorded once with its disposition, the counts and dispositions
// summed per storage; the end drops the rows for good and an undo is
// refused after; the targets go on matching until arrivals are allowed.
func testPurgeRecordsObjects(e *cenv) {
	e.setupTwo()
	c := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-2"}, 500)
	c = e.commit(c.ID, 600, nil)
	due, err := e.p.DueCorrections(e.ctx, at(601), 10)
	e.ok("DueCorrections", err)
	if len(due) != 0 {
		e.Fatalf("a delete in its grace is due: %+v", due)
	}
	due, err = e.p.DueCorrections(e.ctx, c.PurgeAfter, 10)
	e.ok("DueCorrections", err)
	if len(due) != 1 || due[0].ID != c.ID {
		e.Fatalf("at its purge time the due corrections are %+v", due)
	}
	e.ok("RecordPurgedObject before the purge",
		func() error {
			err := e.p.RecordPurgedObject(e.ctx, tenant.PurgedObject{ID: c.ID, Storage: "store", Key: "data/x", Disposition: tenant.DispositionRemoved, Now: at(700)})
			if fault.KindOf(err) == fault.Precondition {
				return nil
			}
			return errors.Join(errors.New("want a precondition failure"), err)
		}())
	p, err := e.p.StartPurge(e.ctx, c.ID, c.PurgeAfter, nil, nil)
	e.ok("StartPurge by the server", err)
	if p.State != record.StatePurging {
		e.Fatalf("a purge started is %s", p.State)
	}
	objects := []tenant.PurgedObject{
		{Key: "data/sub-01/ses-2/a/1.dcm", Size: 10, Disposition: tenant.DispositionRemoved},
		{Key: "data/sub-01/ses-2/a/2.dcm", Size: 20, Disposition: tenant.DispositionHidden},
		{Key: "data/sub-01/ses-2/a/3.dcm", Version: "v1", Size: 30, Disposition: tenant.DispositionRemoved},
		{Key: "data/sub-01/ses-2/a/3.dcm", Version: "v2", Size: 30, Disposition: tenant.DispositionRetained, RetainUntil: at(99999)},
	}
	for _, o := range append(objects, objects[0]) {
		o.ID, o.Storage, o.SHA256, o.Now = c.ID, "store", sum(int(o.Size)), at(800)
		e.ok("RecordPurgedObject", e.p.RecordPurgedObject(e.ctx, o))
	}
	got := e.wantState(c.ID, record.StatePurging)
	d := got.Dispositions
	if len(d) != 1 || d[0].Removed != 1 || d[0].Hidden != 1 || d[0].Retained != 1 || d[0].VersionsRemoved != 1 ||
		!d[0].RetainUntil.Equal(at(99999)) || got.Counts.ObjectsRemoved != 1 || got.Counts.BytesRemoved != 10 {
		e.Fatalf("the purge's dispositions are %+v and counts %+v", d, got.Counts)
	}
	_, err = e.p.UndoCorrection(e.ctx, c.ID, "r", at(801), e.audit(), nil)
	e.wantKind("an undo once the purge started", err, fault.Precondition)
	f, err := e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: c.ID, Now: at(900), Audit: &master.AuditEntry{ID: e.id("a"), Time: at(900),
		LocalUser: "server", Method: "PurgeCorrection", Outcome: master.OutcomeDone}})
	e.ok("FinishPurge", err)
	if f.State != record.StatePurged || !f.Ended.Equal(at(900)) {
		e.Fatalf("a finished purge is %+v", f)
	}
	if again, err := e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: c.ID, Now: at(901)}); err != nil || !again.Ended.Equal(at(900)) {
		e.Fatalf("a finish run again answers %+v, %v", again, err)
	}
	e.wantTotalsRecounted("after a session's purge")
	if m := e.matches(tenant.DeletedProbe{StudyUID: uidOf(1, 2)}); m.Study != c.ID {
		e.Fatal("a purged session's delete no longer matches its study")
	}
	held := e.wantState(c.ID, record.StatePurged).Holds
	if len(held) != 2 || held[0] != (tenant.HeldValue{Kind: tenant.HeldSeries, Value: uidOf(2, 200)}) ||
		held[1] != (tenant.HeldValue{Kind: tenant.HeldStudy, Value: uidOf(1, 2)}) {
		e.Fatalf("a purged session's delete holds %+v, want its series and its study", held)
	}
	study := tenant.HeldValue{Kind: tenant.HeldStudy, Value: uidOf(1, 2)}
	if _, err := e.p.AllowArrivals(e.ctx, c.ID, study, "consent given again", at(950), e.audit(), nil); err != nil {
		e.Fatalf("AllowArrivals: %v", err)
	}
	if m := e.matches(tenant.DeletedProbe{StudyUID: uidOf(1, 2), Series: []string{uidOf(2, 200)}}); m.Study != "" || m.Series[uidOf(2, 200)] != c.ID {
		e.Fatalf("after its study is allowed the delete matches %+v, want its series alone", m)
	}
	if c := e.wantState(c.ID, record.StatePurged); c.ArrivalsAllowed {
		e.Fatal("the delete says its arrivals are allowed while it still holds its series")
	}
	_, err = e.p.AllowArrivals(e.ctx, c.ID, study, "again", at(951), e.audit(), nil)
	e.wantKind("an allow of a value no longer held", err, fault.Precondition)
	if _, err := e.p.AllowArrivals(e.ctx, c.ID, held[0], "consent given again", at(952), e.audit(), nil); err != nil {
		e.Fatalf("AllowArrivals of the series: %v", err)
	}
	if c := e.wantState(c.ID, record.StatePurged); !c.ArrivalsAllowed || len(c.Holds) != 0 {
		e.Fatal("the delete does not say its arrivals are allowed")
	}
	ev := e.changedEvents()
	if len(ev) != 2 || ev[1].State != record.StatePurged {
		e.Fatalf("the events are %+v, want committed then purged", ev)
	}
}

// testAbandonAndPending guards abandoning a prepared delete (nothing
// hidden, its targets no longer matching, asked again answered) and the
// corrections a start takes up: those not ended, and those whose records
// lag their state, until MarkRecords says they are written.
func testAbandonAndPending(e *cenv) {
	e.setupTwo()
	before := e.totals()
	c := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, 500)
	d := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-2"}, 510)
	pending, err := e.p.PendingCorrections(e.ctx, "", 10)
	e.ok("PendingCorrections", err)
	if len(pending) != 2 || pending[0].ID != c.ID || pending[1].ID != d.ID {
		e.Fatalf("the pending corrections are %+v", pending)
	}
	a, err := e.p.AbandonCorrection(e.ctx, c.ID, "the server stopped before it was published", at(600))
	e.ok("AbandonCorrection", err)
	if a.State != record.StateAbandoned || a.StateReason == "" || !a.Ended.Equal(at(600)) {
		e.Fatalf("an abandoned delete is %+v", a)
	}
	if again, err := e.p.AbandonCorrection(e.ctx, c.ID, "again", at(601)); err != nil || again.StateReason != a.StateReason {
		e.Fatalf("an abandon asked again answers %+v, %v", again, err)
	}
	if e.totals() != before {
		e.Fatal("an abandoned delete changed the catalog")
	}
	if m := e.matches(tenant.DeletedProbe{StudyUID: uidOf(1, 1)}); m.Study != "" {
		e.Fatal("an abandoned delete still matches")
	}
	pending, err = e.p.PendingCorrections(e.ctx, "", 10)
	e.ok("PendingCorrections", err)
	if len(pending) != 2 || pending[0].ID != c.ID || pending[0].RecordsState != "" {
		e.Fatalf("an abandoned delete whose records lag is not pending: %+v", pending)
	}
	e.ok("MarkRecords", e.p.MarkRecords(e.ctx, c.ID, record.StateAbandoned))
	pending, err = e.p.PendingCorrections(e.ctx, "", 10)
	e.ok("PendingCorrections", err)
	if len(pending) != 1 || pending[0].ID != d.ID {
		e.Fatalf("after its records are marked the pending corrections are %+v", pending)
	}
	if pending, err = e.p.PendingCorrections(e.ctx, d.ID, 10); err != nil || len(pending) != 0 {
		e.Fatalf("the pending corrections after the last are %+v, %v", pending, err)
	}
}

// testListCorrections guards the list: newest first, filtered by kind and
// state, paged with a cursor bound to its filter.
func testListCorrections(e *cenv) {
	e.setupTwo()
	c := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, 500)
	d := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-2"}, 510)
	e.commit(d.ID, 600, nil)
	list, next, err := e.p.ListCorrections(e.ctx, tenant.CorrectionFilter{}, master.Page{Limit: 1})
	e.ok("ListCorrections", err)
	if len(list) != 1 || list[0].ID != d.ID || next == "" {
		e.Fatalf("the first page is %+v, %q", list, next)
	}
	list, next, err = e.p.ListCorrections(e.ctx, tenant.CorrectionFilter{}, master.Page{Limit: 1, Cursor: next})
	e.ok("ListCorrections", err)
	if len(list) != 1 || list[0].ID != c.ID || next != "" {
		e.Fatalf("the second page is %+v, %q", list, next)
	}
	list, _, err = e.p.ListCorrections(e.ctx, tenant.CorrectionFilter{States: []record.State{record.StatePrepared}}, master.Page{Limit: 10})
	e.ok("ListCorrections by state", err)
	if len(list) != 1 || list[0].ID != c.ID {
		e.Fatalf("the prepared corrections are %+v", list)
	}
	list, _, err = e.p.ListCorrections(e.ctx, tenant.CorrectionFilter{Kinds: []record.Kind{record.KindRelabel}}, master.Page{Limit: 10})
	e.ok("ListCorrections by kind", err)
	if len(list) != 0 {
		e.Fatalf("the relabels are %+v", list)
	}
	_, first, _ := e.p.ListCorrections(e.ctx, tenant.CorrectionFilter{}, master.Page{Limit: 1})
	_, _, err = e.p.ListCorrections(e.ctx, tenant.CorrectionFilter{Kinds: []record.Kind{record.KindDelete}}, master.Page{Limit: 1, Cursor: first})
	e.wantKind("a cursor of another filter", err, fault.Invalid)
	_, err = e.p.Correction(e.ctx, "01aaaaaaaaaaaaaaaaaaaaaaaa")
	e.wantKind("a correction not held", err, fault.NotFound)
	_, err = e.p.Correction(e.ctx, "not-an-id")
	e.wantKind("a correction id not a ULID", err, fault.Invalid)
	if _, err := layout.CorrectionKey(c.ID); err != nil {
		e.Fatalf("a new id is not an operation id: %v", err)
	}
}
