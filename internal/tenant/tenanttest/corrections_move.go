package tenanttest

import (
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A move files a session under another subject, or merges a subject into
// another (tenant.Moves).

// setupMove archives setupTwo's subject sub-01 and a destination, sub-02,
// with one session ses-9 of one scan of two instances (study 9). It
// returns sub-01's two receptions.
func (e *cenv) setupMove() (archived, archived) {
	e.Helper()
	a, b := e.setupTwo()
	e.done(e.archiveStudy(studySpec{receiver: "recv", subject: "sub-02", session: "ses-9", study: 9, at: 300,
		scans: []scanSpec{{label: "scan-a", instances: 2}}}))
	return a, b
}

// done takes a reception archiveStudy left identified to done.
func (e *cenv) done(r tenant.Reception) {
	e.Helper()
	r, err := e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: r.State, Event: reception.Archive, At: r.StepTime.Add(time.Minute)}, nil)
	e.ok("StepReception archive", err)
	e.finish(archived{r: r})
}

// prepareMove previews a move of target under to at second s and prepares
// it with the preview's token a second later.
func (e *cenv) prepareMove(target tenant.CorrectionTarget, to string, s int) (tenant.Correction, bool, error) {
	e.Helper()
	p, err := e.p.PreviewMove(e.ctx, target, to, at(s))
	if err != nil {
		return tenant.Correction{}, false, err
	}
	return e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(s), Kind: record.KindMove, Target: target, ToLabel: to,
		Reason: "filed under the wrong patient", Confirm: p.Confirm, Now: at(s + 1)}, e.audit(), nil)
}

// commitMove publishes a move at second s with a manifest and snapshot of
// each scan, and returns the manifests written and the snapshots read.
func (e *cenv) commitMove(c tenant.Correction, s int) (map[string]tenant.SHA256, map[string]tenant.ScanSnapshot) {
	e.Helper()
	scans, err := e.p.CorrectionScans(e.ctx, c.ID)
	e.ok("CorrectionScans", err)
	manifests, snaps := map[string]tenant.SHA256{}, map[string]tenant.ScanSnapshot{}
	for i, sc := range scans {
		manifests[sc.SeriesUID] = sum(900 + i)
		snaps[sc.SeriesUID] = tenant.ScanSnapshot{Manifest: sum(950 + i), ManifestSize: 100, Metadata: sum(970 + i), MetadataSize: 50}
	}
	got, err := e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: c.ID, Now: at(s), Grace: grace, Manifests: manifests,
		Snapshots: snaps})
	e.ok("CommitCorrection of a move", err)
	if got.State != record.StateCommitted {
		e.Fatalf("a published move is %s", got.State)
	}
	return manifests, snaps
}

// moveEvents returns the payloads of the session_moved and subject_merged
// events of the feed, in order.
func (e *cenv) moveEvents() []tenant.EventPayload {
	e.Helper()
	all, _ := e.readAll("", 1000, tenant.EventSessionMoved, tenant.EventSubjectMerged)
	out := make([]tenant.EventPayload, 0, len(all))
	for _, ev := range all {
		out = append(out, ev.Payload)
	}
	return out
}

// wantMoveUse fails unless the tenant's use (tenant.Quotas) holds what it
// held before and counts instances objects of bytes bytes awaiting purge:
// a move keeps its old tree until its purge, so it counts until then.
func (e *cenv) wantMoveUse(when string, before tenant.QuotaUse, instances, bytes int64) {
	e.Helper()
	want := before
	want.PurgeInstances, want.PurgeBytes = instances, bytes
	e.wantUse(when, want)
}

// sessionCount reads how many sessions the catalog gives a subject.
func (e *cenv) sessionCount(subject string) int64 {
	e.Helper()
	s, err := e.p.CatalogSubject(e.ctx, subject)
	e.ok("CatalogSubject "+subject, err)
	return s.Sessions
}

// testMoveSession guards a session's move: the preview's counts and token;
// nothing changes while prepared, though the session's old labels and its
// labels under the destination are held for arrivals and the destination's
// own are not; the publish files the session and its scans under the
// destination at once, with their manifests, the keys of later revisions,
// both subjects' counts and its events; a lookup by the old labels answers
// where it went; the undo files it back as it was, the old manifests named
// again, and its copies are swept.
func testMoveSession(e *cenv) {
	e.setupMove()
	before := e.totals()
	use, err := e.quotaOf().QuotaUse(e.ctx)
	e.ok("QuotaUse", err)
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}
	p, err := e.p.PreviewMove(e.ctx, target, "sub-02", at(500))
	e.ok("PreviewMove", err)
	if p.Counts.Sessions != 1 || p.Counts.Scans != 2 || p.Counts.Instances != 6 || len(p.Sessions) != 1 || p.Sessions[0] != "ses-1" ||
		len(p.Storages) != 1 || len(p.Holds) != 0 || len(p.Clashes) != 0 || p.Confirm == "" {
		e.Fatalf("a move's preview is %+v", p)
	}
	c, made, err := e.prepareMove(target, "sub-02", 500)
	e.ok("PrepareCorrection of a move", err)
	if !made || c.Kind != record.KindMove || c.Level != record.LevelSession || c.ToLabel != "sub-02" || c.Counts.Scans != 2 ||
		c.Counts.ObjectsSuperseded != 6+4 {
		e.Fatalf("a new move is %+v", c)
	}
	again, made, err := e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(502), Kind: record.KindMove, Target: target,
		ToLabel: "sub-02", Reason: "filed under the wrong patient", Confirm: p.Confirm, Now: at(502)}, e.audit(), nil)
	e.ok("PrepareCorrection with the same token", err)
	if made || again.ID != c.ID {
		e.Fatalf("the same move asked again answers %s, made %v", again.ID, made)
	}
	scans, err := e.p.CorrectionScans(e.ctx, c.ID)
	e.ok("CorrectionScans", err)
	for _, s := range scans {
		if s.Path.Subject != "sub-01" || s.NewPath != (label.Path{Subject: "sub-02", Session: s.Path.Session, Scan: s.Path.Scan}) {
			e.Fatalf("a move's scan is %+v", s)
		}
	}
	for _, h := range []struct {
		p    label.Path
		held bool
	}{{label.Path{Subject: "sub-01", Session: "ses-1", Scan: "x"}, true}, {label.Path{Subject: "SUB-02", Session: "SES-1", Scan: "x"}, true},
		{label.Path{Subject: "sub-02", Session: "ses-9", Scan: "x"}, false}, {label.Path{Subject: "sub-01", Session: "ses-2", Scan: "x"}, false}} {
		got, err := e.p.HeldByCorrection(e.ctx, h.p, uidOf(1, 77), uidOf(2, 77))
		e.ok("HeldByCorrection", err)
		if (got.Correction == c.ID) != h.held {
			e.Fatalf("%v is held by %+v while the move is prepared; held want %v", h.p, got, h.held)
		}
	}
	if e.sessionCount("sub-02") != 1 {
		e.Fatal("a prepared move changed the destination")
	}
	manifests, snaps := e.commitMove(c, 600)
	e.wantMoveUse("after a session's move", use, c.Counts.Instances, c.Counts.Bytes)
	if _, err := e.p.CatalogSession(e.ctx, "sub-01", "ses-1"); fault.KindOf(err) != fault.NotFound {
		e.Fatalf("after the publish the session is still under its old subject: %v", err)
	}
	if e.sessionCount("sub-01") != 1 || e.sessionCount("sub-02") != 2 {
		e.Fatalf("after the publish the subjects hold %d and %d sessions", e.sessionCount("sub-01"), e.sessionCount("sub-02"))
	}
	if got := e.totals(); got != before {
		e.Fatalf("a move changed the totals to %+v", got)
	}
	for _, s := range scans {
		sc, err := e.p.CatalogScan(e.ctx, s.NewPath)
		e.ok("CatalogScan under the destination", err)
		if sc.ManifestSHA256 != manifests[s.SeriesUID] || sc.SubjectLabel != "sub-02" {
			e.Fatalf("after the publish the scan is %+v", sc)
		}
		held := allPages(e.penv, 1000, func(pg master.Page) ([]tenant.ScanInstance, string, error) {
			return e.p.ListHeldInstances(e.ctx, s.NewPath, pg)
		})
		want, _ := layout.InstanceKey(s.NewPath, held[0].SOPInstanceUID)
		if int64(len(held)) != s.Instances || held[0].Key != want {
			e.Fatalf("under the destination the scan lists %d instances, the first at %q, want %d at %q", len(held), held[0].Key, s.Instances, want)
		}
	}
	e.wantTotalsRecounted("after a session's move")
	got, found, err := e.p.RelabeledTo(e.ctx, scans[0].Path)
	e.ok("RelabeledTo", err)
	if !found || got.Correction != c.ID || got.To != scans[0].NewPath {
		e.Fatalf("a lookup by the moved scan's old labels answers %+v, %v", got, found)
	}
	if got, found, err = e.p.RelabeledTo(e.ctx, label.Path{Subject: "sub-01", Session: "ses-1"}); err != nil || !found ||
		got.To != (label.Path{Subject: "sub-02", Session: "ses-1"}) {
		e.Fatalf("a lookup by the moved session's old labels answers %+v, %v, %v", got, found, err)
	}
	ev := e.moveEvents()
	if m, ok := ev[0].(*tenant.SessionMoved); len(ev) != 1 || !ok || m.Correction != c.ID || m.FromSubject != "sub-01" || m.ToSubject != "sub-02" ||
		m.SessionLabel != "ses-1" || m.Scans != 2 || m.Instances != 6 || m.State != record.StateCommitted {
		e.Fatalf("the move's events are %+v", ev)
	}
	if ch := e.changedEvents(); len(ch) != 1 || ch[0].Kind != record.KindMove || ch[0].ToLabel != "sub-02" {
		e.Fatalf("the move's catalog_changed events are %+v", ch)
	}
	u, err := e.p.UndoCorrection(e.ctx, c.ID, "it was the right patient", at(700), e.audit(), nil)
	e.ok("UndoCorrection of a move", err)
	if u.State != record.StateUndone || u.Swept || u.Counts.ObjectsOwned != 0 {
		e.Fatalf("an undone move is %+v", u)
	}
	e.wantMoveUse("after a move's undo", use, 0, 0)
	for _, s := range scans {
		sc, err := e.p.CatalogScan(e.ctx, s.Path)
		e.ok("CatalogScan under the old labels", err)
		if sc.ManifestSHA256 != snaps[s.SeriesUID].Manifest {
			e.Fatalf("after the undo the scan names manifest %x, want the old one %x", sc.ManifestSHA256, snaps[s.SeriesUID].Manifest)
		}
	}
	if e.sessionCount("sub-01") != 2 || e.sessionCount("sub-02") != 1 || e.totals() != before {
		e.Fatal("the undo did not file the session back as it was")
	}
	e.wantTotalsRecounted("after a move's undo")
	if _, found, err := e.p.RelabeledTo(e.ctx, scans[0].Path); err != nil || found {
		e.Fatalf("after the undo a lookup by the old labels answers found %v, %v", found, err)
	}
	if ev := e.moveEvents(); len(ev) != 2 || ev[1].(*tenant.SessionMoved).State != record.StateUndone {
		e.Fatalf("the undo's events are %+v", ev)
	}
	pending, err := e.p.PendingCorrections(e.ctx, "", 100)
	e.ok("PendingCorrections", err)
	if len(pending) != 1 || pending[0].ID != c.ID {
		e.Fatalf("an undone move whose copies are not swept is not pending: %+v", pending)
	}
	e.ok("MarkSwept", e.p.MarkSwept(e.ctx, c.ID))
	if pending, _ = e.p.PendingCorrections(e.ctx, "", 100); len(pending) != 0 && pending[0].ID == c.ID && pending[0].RecordsState == pending[0].State {
		e.Fatalf("a swept undone move is still pending: %+v", pending)
	}
	again, err = e.p.UndoCorrection(e.ctx, c.ID, "it was the right patient", at(710), e.audit(), nil)
	e.ok("UndoCorrection asked again", err)
	if again.State != record.StateUndone {
		e.Fatalf("an undo asked again answers %s", again.State)
	}
}

// testMergeSubjects guards a merge: every session filed under the
// destination at once, the emptied subject gone from the catalog with the
// totals, its label held for arrivals until the merge ends and answering
// where its data went; its purge ends it; and, for a second merge, the undo
// makes the subject again with its sessions.
func testMergeSubjects(e *cenv) {
	e.setupMove()
	before := e.totals()
	use, err := e.quotaOf().QuotaUse(e.ctx)
	e.ok("QuotaUse", err)
	target := tenant.CorrectionTarget{Subject: "sub-01"}
	c, made, err := e.prepareMove(target, "sub-02", 500)
	e.ok("PrepareCorrection of a merge", err)
	if !made || c.Level != record.LevelSubject || c.Counts.Sessions != 2 || c.Counts.Scans != 3 || c.Counts.Subjects != 1 {
		e.Fatalf("a new merge is %+v", c)
	}
	e.commitMove(c, 600)
	e.wantMoveUse("after a merge", use, c.Counts.Instances, c.Counts.Bytes)
	if _, err := e.p.CatalogSubject(e.ctx, "sub-01"); fault.KindOf(err) != fault.NotFound {
		e.Fatalf("after a merge the merged subject is still found: %v", err)
	}
	if e.sessionCount("sub-02") != 3 {
		e.Fatalf("after a merge the destination holds %d sessions", e.sessionCount("sub-02"))
	}
	if got := e.totals(); got.Subjects != before.Subjects-1 || got.Sessions != before.Sessions || got.Instances != before.Instances {
		e.Fatalf("after a merge the totals are %+v, before %+v", got, before)
	}
	e.wantTotalsRecounted("after a merge")
	h, err := e.p.HeldByCorrection(e.ctx, label.Path{Subject: "SUB-01", Session: "ses-new", Scan: "x"}, uidOf(1, 77), uidOf(2, 77))
	e.ok("HeldByCorrection", err)
	if h.Correction != c.ID || h.Level != record.LevelSubject {
		e.Fatalf("the merged subject's label is held by %+v, want the merge at the subject", h)
	}
	if got, found, err := e.p.RelabeledTo(e.ctx, label.Path{Subject: "sub-01", Session: "ses-2"}); err != nil || !found ||
		got.To != (label.Path{Subject: "sub-02", Session: "ses-2"}) {
		e.Fatalf("a lookup by a merged session's old labels answers %+v, %v, %v", got, found, err)
	}
	if ev := e.moveEvents(); len(ev) != 1 {
		e.Fatalf("the merge's events are %+v", ev)
	} else if m, ok := ev[0].(*tenant.SubjectMerged); !ok || m.FromSubject != "sub-01" || m.ToSubject != "sub-02" || m.Sessions != 2 ||
		m.Scans != 3 || m.Instances != 8 {
		e.Fatalf("the merge's event is %+v", ev[0])
	}
	_, err = e.p.StartPurge(e.ctx, c.ID, at(700), &master.AuditEntry{ID: e.id("a"), Time: at(700), Method: "PurgeCorrection",
		Outcome: master.OutcomeDone}, nil)
	e.ok("StartPurge of a merge", err)
	targets, err := e.p.PurgeTargets(e.ctx, c.ID, tenant.MaxPurgeTargets)
	e.ok("PurgeTargets", err)
	for _, t := range targets {
		if t.Path.Subject != "sub-01" || t.Shared {
			e.Fatalf("a merge's purge owns %+v; it owns the old tree only", t)
		}
	}
	if len(targets) != 8+3*2 {
		e.Fatalf("a merge's purge owns %d objects, want %d", len(targets), 8+3*2)
	}
	f, err := e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: c.ID, Now: at(800)})
	e.ok("FinishPurge of a merge", err)
	if f.State != record.StatePurged {
		e.Fatalf("a purged merge is %s", f.State)
	}
	e.wantMoveUse("after a merge's purge", use, 0, 0)
	if h, err := e.p.HeldByCorrection(e.ctx, label.Path{Subject: "sub-01", Session: "s", Scan: "x"}, uidOf(1, 78), uidOf(2, 78)); err != nil ||
		h.Correction != "" {
		e.Fatalf("a purged merge still holds its subject's label: %+v, %v", h, err)
	}
	_, err = e.p.UndoCorrection(e.ctx, c.ID, "r", at(900), e.audit(), nil)
	e.wantKind("an undo of a purged merge", err, fault.Precondition)
}

// testMergeUndo guards a merge's undo: the merged subject made again under
// its label with every session it had, the totals as before.
func testMergeUndo(e *cenv) {
	e.setupMove()
	before := e.totals()
	use, err := e.quotaOf().QuotaUse(e.ctx)
	e.ok("QuotaUse", err)
	c, _, err := e.prepareMove(tenant.CorrectionTarget{Subject: "sub-01"}, "sub-02", 500)
	e.ok("PrepareCorrection of a merge", err)
	e.commitMove(c, 600)
	_, err = e.p.UndoCorrection(e.ctx, c.ID, "two patients after all", at(700), e.audit(), nil)
	e.ok("UndoCorrection of a merge", err)
	e.wantMoveUse("after a merge's undo", use, 0, 0)
	if e.sessionCount("sub-01") != 2 || e.sessionCount("sub-02") != 1 || e.totals() != before {
		e.Fatalf("after a merge's undo the subjects hold %d and %d sessions, totals %+v (before %+v)", e.sessionCount("sub-01"),
			e.sessionCount("sub-02"), e.totals(), before)
	}
	e.wantTotalsRecounted("after a merge's undo")
	if ev := e.moveEvents(); len(ev) != 2 || ev[1].(*tenant.SubjectMerged).State != record.StateUndone {
		e.Fatalf("the merge's events are %+v", ev)
	}
}

// testMoveRefusals guards what a move refuses: a destination that is the
// source or that the tenant lacks; a session that is its subject's last; a
// session label the destination holds (Conflict); a hold on the
// destination or the source (HELD); a token whose preview no longer
// answers the same (data arrived); a destination another correction
// covers; a hold placed while it copied, at its publish; and that an edit
// of what a committed move moved is allowed, after which the move's undo
// is refused.
func testMoveRefusals(e *cenv) {
	e.setupMove()
	_, err := e.p.PreviewMove(e.ctx, tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, "SUB-01", at(500))
	e.wantKind("a move to the same subject", err, fault.Invalid)
	_, err = e.p.PreviewMove(e.ctx, tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, "sub-77", at(500))
	e.wantKind("a move to a subject the tenant lacks", err, fault.NotFound)
	_, err = e.p.PreviewMove(e.ctx, tenant.CorrectionTarget{Subject: "sub-02", Session: "ses-9"}, "sub-01", at(500))
	e.wantKind("a move of a subject's last session", err, fault.Precondition)
	// A destination holding a session of the label moved, in another case.
	e.done(e.archiveStudy(studySpec{receiver: "recv", subject: "sub-02", session: "SES-2", study: 8, at: 400,
		scans: []scanSpec{{label: "scan-b", instances: 1}}}))
	p, err := e.p.PreviewMove(e.ctx, tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-2"}, "sub-02", at(500))
	e.ok("PreviewMove of a clash", err)
	if len(p.Clashes) != 1 || p.Clashes[0] != "ses-2" {
		e.Fatalf("a clash's preview is %+v", p)
	}
	_, _, err = e.prepareMove(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-2"}, "sub-02", 500)
	e.wantKind("a move whose session label the destination holds", err, fault.Conflict)
	_, _, err = e.prepareMove(tenant.CorrectionTarget{Subject: "sub-01"}, "sub-02", 500)
	e.wantKind("a merge whose session label the destination holds", err, fault.Conflict)
	// A token whose preview answers otherwise: a scan arrived since.
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}
	p, err = e.p.PreviewMove(e.ctx, target, "sub-02", at(510))
	e.ok("PreviewMove", err)
	late := e.archive("recv", 1, 1, 1, 50000, at(520), "ses-1")
	e.finish(late)
	_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(530), Kind: record.KindMove, Target: target, ToLabel: "sub-02",
		Reason: "filed under the wrong patient", Confirm: p.Confirm, Now: at(530)}, e.audit(), nil)
	e.wantKind("a move with a stale token", err, fault.Precondition)
	// A hold on the destination, then on the source.
	for i, ht := range []tenant.HoldTarget{{Subject: "sub-02"}, {Subject: "sub-01", Session: "ses-1"}} {
		h, _, err := e.p.PlaceHold(e.ctx, tenant.HoldPlace{ID: e.newID(540 + i), Target: ht, Reason: "audit", Written: []string{"store"},
			Now: at(540 + i)}, e.audit(), nil)
		e.ok("PlaceHold", err)
		p, err := e.p.PreviewMove(e.ctx, target, "sub-02", at(550))
		e.ok("PreviewMove under a hold", err)
		if len(p.Holds) != 1 || p.Holds[0] != h.ID {
			e.Fatalf("a move's preview under a hold is %+v", p)
		}
		_, _, err = e.prepareMove(target, "sub-02", 550)
		var held *tenant.HeldRefusal
		if fault.KindOf(err) != fault.Precondition || !errors.As(err, &held) || held.Hold != h.ID {
			e.Fatalf("a move under a hold answers %v, want HELD by %s", err, h.ID)
		}
		_, err = e.p.ReleaseHold(e.ctx, tenant.HoldRelease{ID: h.ID, Reason: "done", Now: at(560 + i)}, e.audit(), nil)
		e.ok("ReleaseHold", err)
	}
	// A destination another correction covers: a relabel of it prepared.
	r, _, err := e.prepareRelabel(tenant.CorrectionTarget{Subject: "sub-02", Session: "ses-9"}, "ses-99", 600)
	e.ok("PrepareCorrection of a relabel", err)
	_, _, err = e.prepareMove(target, "sub-02", 610)
	e.wantKind("a move into a subject being corrected", err, fault.Precondition)
	_, err = e.p.AbandonCorrection(e.ctx, r.ID, "stopped", at(620))
	e.ok("AbandonCorrection", err)
	// A hold placed while it copied refuses its publish.
	c, _, err := e.prepareMove(target, "sub-02", 630)
	e.ok("PrepareCorrection of a move", err)
	h, _, err := e.p.PlaceHold(e.ctx, tenant.HoldPlace{ID: e.newID(640), Target: tenant.HoldTarget{Subject: "sub-02"}, Reason: "audit",
		Written: []string{"store"}, Now: at(640)}, e.audit(), nil)
	e.ok("PlaceHold", err)
	_, err = e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: c.ID, Now: at(650), Grace: grace})
	e.wantKind("a move's publish under a hold placed while it copied", err, fault.Precondition)
	_, err = e.p.ReleaseHold(e.ctx, tenant.HoldRelease{ID: h.ID, Reason: "done", Now: at(655)}, e.audit(), nil)
	e.ok("ReleaseHold", err)
	e.commitMove(c, 660)
	// An edit of what it moved is allowed while it is committed; its undo
	// is then refused.
	ed, _, err := e.prepareEdit(tenant.CorrectionTarget{Subject: "sub-02", Session: "ses-1"}, 670)
	e.ok("PrepareCorrection of an edit of a moved session", err)
	_, err = e.p.UndoCorrection(e.ctx, c.ID, "r", at(680), e.audit(), nil)
	e.wantKind("an undo of a move whose scans an edit touched since", err, fault.Precondition)
	_, err = e.p.AbandonCorrection(e.ctx, ed.ID, "stopped", at(690))
	e.ok("AbandonCorrection of the edit", err)
	// Data arrived in what it moved since its publish: its undo is refused.
	e.archiveUnder("sub-02", "ses-1", 1, 100, 60000, 700)
	_, err = e.p.UndoCorrection(e.ctx, c.ID, "r", at(710), e.audit(), nil)
	e.wantKind("an undo of a move whose scan took an instance since", err, fault.Precondition)
}

// archiveUnder archives one instance (uidOf(3, n)) of the series uidOf(2,
// series) of the study uidOf(1, study) under the subject and session
// labels given, at second s, as setupTwo's receptions do, and finishes the
// reception.
func (e *cenv) archiveUnder(subject, session string, study, series, n, s int) {
	e.Helper()
	r, _, err := e.p.OpenReception(e.ctx, "recv", uidOf(1, study), e.id("rc"), at(s))
	e.ok("OpenReception", err)
	f := file(r, study, n)
	f.SeriesUID = uidOf(2, series)
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{f}, at(s), nil)
	e.ok("AddReceivedFiles", err)
	r, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: r.State, Event: reception.Quiet, At: at(s + 1)}, nil)
	e.ok("StepReception quiet", err)
	r, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: r.State, Event: reception.Identify, At: at(s + 2),
		SubjectLabel: subject, SessionLabel: session, Storage: "store"}, nil)
	e.ok("StepReception identify", err)
	_, err = e.p.ClaimInstances(e.ctx, r.ID, []tenant.InstanceClaim{claimOf(f)}, at(s+3), nil)
	e.ok("ClaimInstances", err)
	p := label.Path{Subject: subject, Session: session, Scan: "scan-" + f.SeriesUID}
	key, err := layout.InstanceKey(p, f.SOPInstanceUID)
	e.ok("InstanceKey", err)
	_, err = e.p.CommitScan(e.ctx, tenant.ScanCommit{ReceptionID: r.ID, StudyUID: r.StudyUID, SeriesUID: f.SeriesUID, SubjectLabel: subject,
		SessionLabel: session, ScanLabel: p.Scan, Storage: "store", ManifestSHA256: sum(n), At: at(s + 3),
		Instances: []tenant.ConfirmedInstance{{SOPInstanceUID: f.SOPInstanceUID, Key: key, Length: f.Size, Size: f.Size, ArchivedSHA256: f.SHA256}}})
	e.ok("CommitScan", err)
	e.done(r)
}
