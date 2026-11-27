package tenanttest

import (
	"errors"
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// editsOf returns the store as it keeps edits.
func (e *cenv) editsOf() tenant.Edits {
	e.Helper()
	ed, ok := e.p.(tenant.Edits)
	if !ok {
		e.Fatal("the driver's store keeps no edits (tenant.Edits)")
	}
	return ed
}

// editConfirm is a token of an edit's preview issued at second s: its
// form is a delete's, the store reads only its issue time.
func editConfirm(s int) string {
	return "c1." + strconv.FormatInt(at(s).UnixMicro(), 10) + "." + strings.Repeat("e", 64)
}

// prepareEdit prepares an edit of target at second s, of the instances
// EditTargets answers, each with a line to write back.
func (e *cenv) prepareEdit(target tenant.CorrectionTarget, s int) (tenant.Correction, []tenant.EditTarget, error) {
	e.Helper()
	targets, err := e.editsOf().EditTargets(e.ctx, target, tenant.MaxEditInstances+1)
	e.ok("EditTargets", err)
	edits := make([]tenant.EditPrepared, len(targets))
	for i, t := range targets {
		edits[i] = tenant.EditPrepared{UID: t.UID, SeriesUID: t.SeriesUID, OldRevision: t.Revision, OldSHA256: t.SHA256,
			RestoreLine: []byte(`{"sop_instance_uid":"` + t.UID + `"}`)}
	}
	c, _, err := e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(s), Kind: record.KindEdit, Target: target,
		Confirm: editConfirm(s), Reason: "series named wrongly", Edits: edits, Now: at(s)}, e.audit(), nil)
	return c, targets, err
}

// commitEdit publishes an edit at second s: each instance's new revision
// of 900 bytes, the scan's description set, its manifest's digest given.
func (e *cenv) commitEdit(c tenant.Correction, targets []tenant.EditTarget, s int) tenant.Correction {
	e.Helper()
	cc := tenant.CorrectionCommit{ID: c.ID, Now: at(s), Grace: grace, Manifests: map[string]tenant.SHA256{},
		Values: map[string]tenant.EditValues{}}
	for i, t := range targets {
		cc.Edits = append(cc.Edits, tenant.EditWritten{UID: t.UID, SHA256: sum(8000 + s + i), Size: 900, Line: []byte(`{"new":"` + t.UID + `"}`)})
		cc.Manifests[t.SeriesUID] = sum(9000 + s)
		cc.Values[t.SeriesUID] = tenant.EditValues{SeriesDescription: "Scout view", SetDescription: true}
	}
	got, err := e.p.CommitCorrection(e.ctx, cc)
	e.ok("CommitCorrection of an edit", err)
	if got.State != record.StateCommitted {
		e.Fatalf("a published edit is %s", got.State)
	}
	return got
}

// testEdit guards an edit through the port (tenant.Edits): EditTargets
// answers a scan's instances in order and refuses a target the catalog
// does not hold; the prepare records each instance at a later revision and
// refuses one whose revision changed since it was read; the publish puts
// every instance at its new revision's key, keeps the superseded revision
// (ResolveInstances), and gives the scan the values and manifest set; an
// overlapping edit is refused while it is committed; BeginEditUndo marks
// it, and is answered again as it stands; UndoEdit puts every instance
// and the scan's values back; while its new revisions are not swept a new
// edit is refused, and once swept a new edit numbers its revisions above
// the undone ones.
func testEdit(e *cenv) {
	e.setupTwo()
	ed := e.editsOf()
	scans, err := e.p.ListScans(e.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 100})
	e.ok("ListScans", err)
	var sc tenant.CatalogScan
	for _, s := range scans.Items {
		if s.Instances == 3 {
			sc = s
			break
		}
	}
	target := tenant.CorrectionTarget{Subject: sc.SubjectLabel, Session: sc.SessionLabel, Scan: sc.Label}
	path := target.Path()

	targets, err := ed.EditTargets(e.ctx, target, 2)
	e.ok("EditTargets", err)
	if len(targets) != 2 || targets[0].UID >= targets[1].UID || targets[0].Revision != 1 || targets[0].Path != path ||
		targets[0].SeriesUID != sc.SeriesUID {
		e.Fatalf("EditTargets with a limit of 2 answers %+v", targets)
	}
	_, err = ed.EditTargets(e.ctx, tenant.CorrectionTarget{Subject: "sub-none"}, 10)
	e.wantKind("EditTargets of a subject the catalog does not hold", err, fault.NotFound)

	// An instance whose checksum is not the one read is refused.
	all, err := ed.EditTargets(e.ctx, target, 10)
	e.ok("EditTargets", err)
	stale := make([]tenant.EditPrepared, len(all))
	for i, t := range all {
		stale[i] = tenant.EditPrepared{UID: t.UID, SeriesUID: t.SeriesUID, OldRevision: t.Revision, OldSHA256: sum(1), RestoreLine: []byte("{}")}
	}
	_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(490), Kind: record.KindEdit, Target: target,
		Confirm: editConfirm(490), Reason: "r", Edits: stale, Now: at(490)}, e.audit(), nil)
	if !errors.Is(err, tenant.ErrTargetChanged) {
		e.Fatalf("an edit of instances other than the catalog's answered %v, want the target changed", err)
	}

	c, targets, err := e.prepareEdit(target, 500)
	e.ok("PrepareCorrection of an edit", err)
	if c.State != record.StatePrepared || len(c.Edits) != 3 {
		e.Fatalf("a new edit is %s with %d instances, want prepared with 3", c.State, len(c.Edits))
	}
	listed, err := ed.CorrectionEdits(e.ctx, c.ID)
	e.ok("CorrectionEdits", err)
	for i, in := range listed {
		if in.UID != targets[i].UID || in.OldRevision != 1 || in.NewRevision != 2 || in.OldKey != targets[i].Key ||
			in.Storage != targets[i].Storage || in.Path != path || len(in.RestoreLine) == 0 || !in.NewSHA256.IsZero() ||
			in.Before.SeriesDescription != sc.Values.SeriesDescription {
			e.Fatalf("a prepared edit's instance %d is %+v", i, in)
		}
	}

	c = e.commitEdit(c, targets, 600)
	for i, t := range targets {
		h, err := e.p.HeldInstance(e.ctx, path, t.UID)
		e.ok("HeldInstance", err)
		key, _ := layout.RevisionKey(path, t.UID, 2)
		if h.Key != key || h.Size != 900 || h.ArchivedSHA256 != sum(8600+i) {
			e.Fatalf("an edited instance is %+v, want revision 2 at %s", h, key)
		}
	}
	got, err := e.p.CatalogScan(e.ctx, path)
	e.ok("CatalogScan", err)
	if got.Values.SeriesDescription != "Scout view" || got.ManifestSHA256 != sum(9600) {
		e.Fatalf("the edited scan is %+v", got)
	}
	e.wantTotalsRecounted("after an edit")
	res, err := e.p.ResolveInstances(e.ctx, []tenant.InstanceRef{{UID: targets[0].UID, Revision: 1}})
	e.ok("ResolveInstances", err)
	if res[0].Disposition != tenant.ResolvedSuperseded || res[0].Correction != c.ID || res[0].CurrentRevision != 2 {
		e.Fatalf("an edited instance's first revision resolves %+v", res[0])
	}
	_, _, err = e.prepareEdit(target, 610)
	e.wantKind("an edit of a scan another committed edit holds", err, fault.Precondition)

	if published, err := ed.CorrectionEdits(e.ctx, c.ID); err != nil || string(published[0].NewLine) != `{"new":"`+targets[0].UID+`"}` ||
		published[0].After.SeriesDescription != "Scout view" {
		e.Fatalf("a published edit's instance is %+v (%v), want its new line and the scan's values now", published[0], err)
	}
	b, err := ed.BeginEditUndo(e.ctx, c.ID, "the description was right", at(700), e.audit(), nil)
	e.ok("BeginEditUndo", err)
	if b.State != record.StateCommitted || b.StateReason != tenant.StateReasonUndoing {
		e.Fatalf("an edit marked for its undo is %s (%q)", b.State, b.StateReason)
	}
	// Marked, it is never purged: its undo may have written the files that
	// name the revisions a purge removes.
	audit := e.audit()
	_, err = e.p.StartPurge(e.ctx, c.ID, at(705), &audit, nil)
	e.wantKind("StartPurge of an edit being undone", err, fault.Precondition)
	due, err := e.p.DueCorrections(e.ctx, at(600).Add(2*grace), 100)
	e.ok("DueCorrections", err)
	for _, d := range due {
		if d.ID == c.ID {
			e.Fatal("an edit being undone is listed as due for its purge")
		}
	}
	e.ok("CheckEditUndo", ed.CheckEditUndo(e.ctx, c.ID, at(706)))
	if again, err := ed.BeginEditUndo(e.ctx, c.ID, "the description was right", at(701), e.audit(), nil); err != nil ||
		again.StateReason != tenant.StateReasonUndoing {
		e.Fatalf("BeginEditUndo again answered %+v, %v", again, err)
	}
	u, err := ed.UndoEdit(e.ctx, tenant.EditUndo{ID: c.ID, Now: at(710), Manifests: map[string]tenant.SHA256{sc.SeriesUID: sum(9710)}})
	e.ok("UndoEdit", err)
	if u.State != record.StateUndone || u.Swept {
		e.Fatalf("an undone edit is %s swept %v, want undone and not swept", u.State, u.Swept)
	}
	for _, t := range targets {
		h, err := e.p.HeldInstance(e.ctx, path, t.UID)
		e.ok("HeldInstance", err)
		if h.Key != t.Key || h.ArchivedSHA256 != t.SHA256 {
			e.Fatalf("after the undo an instance is %+v, want its first revision at %s", h, t.Key)
		}
	}
	if got, _ := e.p.CatalogScan(e.ctx, path); got.Values.SeriesDescription != sc.Values.SeriesDescription || got.ManifestSHA256 != sum(9710) {
		e.Fatalf("after the undo the scan is %+v", got)
	}
	e.wantTotalsRecounted("after an edit's undo")
	_, _, err = e.prepareEdit(target, 720)
	e.wantKind("an edit while an undone edit's new revisions are not swept", err, fault.Precondition)
	e.ok("MarkSwept", e.p.MarkSwept(e.ctx, c.ID))
	next, _, err := e.prepareEdit(target, 730)
	e.ok("PrepareCorrection of an edit after the sweep", err)
	for _, in := range next.Edits {
		if in.OldRevision != 1 || in.NewRevision != 3 {
			e.Fatalf("an edit after an undone one to revision 2 gives revision %d to %d, want 1 to 3", in.OldRevision, in.NewRevision)
		}
	}
}

// testAbandonEditUndo guards an undo that may not be published: a hold
// placed over an edit being undone makes CheckEditUndo a precondition
// failure; AbandonEditUndo without its audit entry is invalid; with it, it
// gives each scan the digests given, clears the mark and the undo reason
// (the edit committed with its instances at their new revisions), and
// writes the entry and one catalog_changed event naming the edit; asked
// again it is answered as it stands. An edit no longer being undone makes
// CheckEditUndo answer ErrNotBeingUndone.
func testAbandonEditUndo(e *cenv) {
	e.setupTwo()
	ed := e.editsOf()
	scans, err := e.p.ListScans(e.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 100})
	e.ok("ListScans", err)
	sc := scans.Items[0]
	target := tenant.CorrectionTarget{Subject: sc.SubjectLabel, Session: sc.SessionLabel, Scan: sc.Label}
	c, targets, err := e.prepareEdit(target, 500)
	e.ok("PrepareCorrection of an edit", err)
	c = e.commitEdit(c, targets, 600)
	_, err = ed.BeginEditUndo(e.ctx, c.ID, "the description was right", at(700), e.audit(), nil)
	e.ok("BeginEditUndo", err)
	_, _, err = e.p.PlaceHold(e.ctx, tenant.HoldPlace{ID: e.newID(710), Target: tenant.HoldTarget{Subject: sc.SubjectLabel}, Reason: "audit",
		Written: []string{"store"}, Now: at(710)}, e.audit(), nil)
	e.ok("PlaceHold", err)
	e.wantKind("CheckEditUndo under a hold", ed.CheckEditUndo(e.ctx, c.ID, at(720)), fault.Precondition)
	u := tenant.EditUndo{ID: c.ID, Now: at(730), Manifests: map[string]tenant.SHA256{sc.SeriesUID: sum(7101)},
		Sidecars: map[string]tenant.SHA256{sc.SeriesUID: sum(7102)}}
	_, err = ed.AbandonEditUndo(e.ctx, u)
	e.wantKind("AbandonEditUndo without its audit entry", err, fault.Invalid)
	entry := master.AuditEntry{ID: e.id("srv"), Time: at(730), LocalUser: "server", Method: "UndoCorrection", TenantName: "lab",
		Object: c.ID, Outcome: master.OutcomeFailed, Reason: "undo taken back: a hold covers the revisions the undo would remove (HELD)"}
	u.Audit = &entry
	before := len(e.changedEvents())
	got, err := ed.AbandonEditUndo(e.ctx, u)
	e.ok("AbandonEditUndo", err)
	if got.State != record.StateCommitted || got.StateReason != "" || got.UndoReason != "" {
		e.Fatalf("an abandoned undo left the edit %s (%q, undo reason %q)", got.State, got.StateReason, got.UndoReason)
	}
	if !e.auditIDs()[entry.ID] {
		e.Fatal("an abandoned undo wrote no audit entry")
	}
	if evs := e.changedEvents(); len(evs) != before+1 || evs[len(evs)-1].Correction != c.ID {
		e.Fatalf("an abandoned undo wrote %d catalog_changed events, want one naming the edit", len(evs)-before)
	}
	s, err := e.p.CatalogScan(e.ctx, target.Path())
	e.ok("CatalogScan", err)
	if s.ManifestSHA256 != sum(7101) || s.SidecarSHA256 != sum(7102) {
		e.Fatalf("after the abandoned undo the scan keeps %s and %s", s.ManifestSHA256, s.SidecarSHA256)
	}
	h, err := e.p.HeldInstance(e.ctx, target.Path(), targets[0].UID)
	e.ok("HeldInstance", err)
	if key, _ := layout.RevisionKey(target.Path(), targets[0].UID, 2); h.Key != key {
		e.Fatalf("after the abandoned undo an instance is at %s, want its new revision %s", h.Key, key)
	}
	if again, err := ed.AbandonEditUndo(e.ctx, u); err != nil || again.StateReason != "" {
		e.Fatalf("AbandonEditUndo again answered %+v, %v", again, err)
	}
	if err := ed.CheckEditUndo(e.ctx, c.ID, at(740)); !errors.Is(err, tenant.ErrNotBeingUndone) || fault.KindOf(err) != fault.Precondition {
		e.Fatalf("CheckEditUndo of an edit no longer being undone answered %v, want ErrNotBeingUndone", err)
	}
}

// testKeepRolledBack guards KeepRolledBack: for a prepared edit it gives
// the scan the manifest and metadata digests a rollback wrote; a scan not
// the edit's, and an edit no longer prepared, are refused.
func testKeepRolledBack(e *cenv) {
	e.setupTwo()
	scans, err := e.p.ListScans(e.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 100})
	e.ok("ListScans", err)
	sc, other := scans.Items[0], scans.Items[1]
	target := tenant.CorrectionTarget{Subject: sc.SubjectLabel, Session: sc.SessionLabel, Scan: sc.Label}
	c, _, err := e.prepareEdit(target, 500)
	e.ok("PrepareCorrection of an edit", err)
	e.ok("KeepRolledBack", e.p.KeepRolledBack(e.ctx, c.ID, sc.SeriesUID, sum(7001), sum(7002)))
	got, err := e.p.CatalogScan(e.ctx, target.Path())
	e.ok("CatalogScan", err)
	if got.ManifestSHA256 != sum(7001) || got.SidecarSHA256 != sum(7002) {
		e.Fatalf("after KeepRolledBack the scan keeps %s and %s", got.ManifestSHA256, got.SidecarSHA256)
	}
	e.wantKind("KeepRolledBack of a scan not the edit's", e.p.KeepRolledBack(e.ctx, c.ID, other.SeriesUID, sum(1), sum(2)), fault.Precondition)
	_, err = e.p.AbandonCorrection(e.ctx, c.ID, "stopped", at(510))
	e.ok("AbandonCorrection", err)
	e.wantKind("KeepRolledBack of an abandoned edit", e.p.KeepRolledBack(e.ctx, c.ID, sc.SeriesUID, sum(1), sum(2)), fault.Precondition)
}
