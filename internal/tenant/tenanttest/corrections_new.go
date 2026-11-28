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

// The corrections that publish a new locator: relabel and replace.

// prepareRelabel prepares a relabel of target to to at second s.
func (e *cenv) prepareRelabel(target tenant.CorrectionTarget, to string, s int) (tenant.Correction, bool, error) {
	return e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(s), Kind: record.KindRelabel, Target: target, ToLabel: to,
		Reason: "wrong subject id at the scanner", Now: at(s)}, e.audit(), nil)
}

// testRelabel guards a relabel: nothing changes while prepared; the
// publish moves the whole target under the new label at once, with the
// manifests given and the copies of the label beneath; a lookup by the old
// label finds nothing; the same relabel asked again answers it; its purge
// marks nothing deleted.
func testRelabel(e *cenv) {
	e.setupTwo()
	before := e.totals()
	target := tenant.CorrectionTarget{Subject: "sub-01"}
	c, made, err := e.prepareRelabel(target, "sub-02", 500)
	e.ok("PrepareCorrection of a relabel", err)
	if !made || c.Level != record.LevelSubject || c.ToLabel != "sub-02" || c.Counts.Scans != 3 || c.Counts.ObjectsSuperseded != 8+6 {
		e.Fatalf("a new relabel is %+v", c)
	}
	scans, err := e.p.CorrectionScans(e.ctx, c.ID)
	e.ok("CorrectionScans", err)
	for _, s := range scans {
		if s.NewPath.Subject != "sub-02" || s.NewPath.Session != s.Path.Session || s.Path.Subject != "sub-01" {
			e.Fatalf("a relabel's scan is %+v", s)
		}
	}
	if _, err := e.p.CatalogSubject(e.ctx, "sub-02"); fault.KindOf(err) != fault.NotFound {
		e.Fatalf("a prepared relabel shows its new label: %v", err)
	}
	e.ok("RecordCopied", e.p.RecordCopied(e.ctx, c.ID, 5))
	if got := e.wantState(c.ID, record.StatePrepared); got.Counts.ObjectsCopied != 5 {
		e.Fatalf("a relabel's copies are %d", got.Counts.ObjectsCopied)
	}
	again, made, err := e.prepareRelabel(target, "sub-02", 501)
	e.ok("PrepareCorrection of the same relabel", err)
	if made || again.ID != c.ID {
		e.Fatalf("the same relabel asked again answers %s, made %v", again.ID, made)
	}
	manifests := map[string]tenant.SHA256{}
	for i, s := range scans {
		manifests[s.SeriesUID] = sum(900 + i)
	}
	e.commit(c.ID, 600, manifests)
	if _, err := e.p.CatalogSubject(e.ctx, "sub-01"); fault.KindOf(err) != fault.NotFound {
		e.Fatalf("after the publish the old label is still found: %v", err)
	}
	if got := e.totals(); got != before {
		e.Fatalf("a relabel changed the totals to %+v", got)
	}
	for _, s := range scans {
		sc, err := e.p.CatalogScan(e.ctx, s.NewPath)
		e.ok("CatalogScan under the new label", err)
		if sc.ManifestSHA256 != manifests[s.SeriesUID] || sc.SubjectLabel != "sub-02" {
			e.Fatalf("after the publish the scan is %+v", sc)
		}
		held := allPages(e.penv, 1000, func(pg master.Page) ([]tenant.ScanInstance, string, error) {
			return e.p.ListHeldInstances(e.ctx, s.NewPath, pg)
		})
		want, _ := layout.InstanceKey(s.NewPath, held[0].SOPInstanceUID)
		if int64(len(held)) != s.Instances || held[0].Key != want {
			e.Fatalf("under the new label the scan lists %d instances, the first at %q, want %d at %q", len(held), held[0].Key,
				s.Instances, want)
		}
	}
	e.wantTotalsRecounted("after a relabel")
	ev := e.changedEvents()
	if len(ev) != 1 || ev[0].ToLabel != "sub-02" || ev[0].Kind != record.KindRelabel {
		e.Fatalf("the relabel's events are %+v", ev)
	}
	_, err = e.p.UndoCorrection(e.ctx, c.ID, "r", at(700), e.audit(), nil)
	e.wantKind("an undo of a committed relabel", err, fault.Precondition)
	_, err = e.p.StartPurge(e.ctx, c.ID, at(700), &master.AuditEntry{ID: e.id("a"), Time: at(700), Method: "PurgeCorrection",
		Outcome: master.OutcomeDone}, nil)
	e.ok("StartPurge of a relabel", err)
	f, err := e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: c.ID, Now: at(800)})
	e.ok("FinishPurge of a relabel", err)
	if f.State != record.StatePurged {
		e.Fatalf("a purged relabel is %s", f.State)
	}
	e.wantTotalsRecounted("after a relabel's purge")
}

// testRelabelRefusals guards what a relabel refuses: a new label held at
// its level (Conflict); an instance named; a target that changed while it
// was prepared (abandoned by the caller); and that an undo of a prepared
// relabel abandons it.
func testRelabelRefusals(e *cenv) {
	e.setupTwo()
	_, _, err := e.prepareRelabel(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, "SES-2", 500)
	e.wantKind("a relabel to a label held in another case", err, fault.Conflict)
	_, _, err = e.prepareRelabel(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + uidOf(2, 100),
		Instance: uidOf(3, 0)}, "x", 500)
	e.wantKind("a relabel of an instance", err, fault.Invalid)
	_, _, err = e.prepareRelabel(tenant.CorrectionTarget{Subject: "sub-01"}, "sub-01", 500)
	e.wantKind("a relabel to its own label", err, fault.Conflict)
	c, _, err := e.prepareRelabel(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-2"}, "ses-3", 510)
	e.ok("PrepareCorrection", err)
	_, _, err = e.prepareRelabel(tenant.CorrectionTarget{Subject: "sub-01"}, "sub-03", 511)
	e.wantKind("a relabel overlapping one prepared", err, fault.Precondition)
	_, _, err = e.prepareRelabel(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, "SES-3", 512)
	e.wantKind("a relabel to the new label of one prepared, in another case", err, fault.Precondition)
	// While it copies, its old and new labels are held for arrivals at its
	// level: a scan landing in the session, or under the new label, would
	// be written where the copy or the purge works.
	for _, p := range []label.Path{{Subject: "sub-01", Session: "ses-2", Scan: "scan-late"}, {Subject: "SUB-01", Session: "SES-3", Scan: "x"}} {
		h, err := e.p.HeldByCorrection(e.ctx, p, uidOf(1, 7), uidOf(2, 7))
		e.ok("HeldByCorrection", err)
		if h.Correction != c.ID || h.Level != record.LevelSession {
			e.Fatalf("%v is held by %+v, want the relabel %s at the session", p, h, c.ID)
		}
	}
	if h, err := e.p.HeldByCorrection(e.ctx, label.Path{Subject: "sub-01", Session: "ses-1", Scan: "x"}, uidOf(1, 7), uidOf(2, 7)); err != nil ||
		h.Correction != "" {
		e.Fatalf("another session of the subject is held by %+v, %v", h, err)
	}
	u, err := e.p.UndoCorrection(e.ctx, c.ID, "stop it", at(610), e.audit(), nil)
	e.ok("UndoCorrection of a prepared relabel", err)
	if u.State != record.StateAbandoned || u.StateReason != tenant.StateReasonStopped {
		e.Fatalf("an undone prepared relabel is %+v", u)
	}
	if _, err := e.p.CatalogSession(e.ctx, "sub-01", "ses-2"); err != nil {
		e.Fatalf("an abandoned relabel moved its session: %v", err)
	}
	if h, err := e.p.HeldByCorrection(e.ctx, label.Path{Subject: "sub-01", Session: "ses-2", Scan: "scan-late"}, uidOf(1, 7), uidOf(2, 7)); err != nil ||
		h.Correction != "" {
		e.Fatalf("an abandoned relabel still holds its session: %+v, %v", h, err)
	}
	if u.Swept {
		e.Fatal("an abandoned relabel is swept before its copies are removed")
	}
	e.ok("MarkSwept", e.p.MarkSwept(e.ctx, c.ID))
	if got := e.wantState(c.ID, record.StateAbandoned); !got.Swept {
		e.Fatal("MarkSwept did not record the sweep")
	}
}

// testDeleteHoldsItsLabels guards what a committed delete holds for
// arrivals: its hidden rows' labels at each level (case folded) and their
// UIDs, refused by the commit as a label conflict naming it; nothing once
// it is undone; and a relabel back and forth, each asked anew once the one
// before has ended.
func testDeleteHoldsItsLabels(e *cenv) {
	a, _ := e.setupTwo()
	c := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, 500)
	e.commit(c.ID, 600, nil)
	for _, x := range []struct {
		path          label.Path
		study, series string
		level         string
	}{
		{label.Path{Subject: "sub-01", Session: "SES-1", Scan: "new"}, uidOf(1, 9), uidOf(2, 9), record.LevelSession},
		{label.Path{Subject: "sub-01", Session: "ses-9", Scan: "new"}, uidOf(1, 1), uidOf(2, 9), record.LevelSession},
		{label.Path{Subject: "sub-01", Session: "ses-9", Scan: "new"}, uidOf(1, 9), uidOf(2, 100), record.LevelScan},
	} {
		h, err := e.p.HeldByCorrection(e.ctx, x.path, x.study, x.series)
		e.ok("HeldByCorrection", err)
		if h.Correction != c.ID || h.Level != x.level {
			e.Fatalf("%v of study %s series %s is held by %+v, want the delete at the %s", x.path, x.study, x.series, h, x.level)
		}
	}
	if h, err := e.p.HeldByCorrection(e.ctx, label.Path{Subject: "sub-01", Session: "ses-9", Scan: "new"}, uidOf(1, 9), uidOf(2, 9)); err != nil ||
		h.Correction != "" {
		e.Fatalf("a new session of the subject is held by %+v, %v", h, err)
	}
	// A study sent again under the deleted session's labels: the commit
	// refuses it, naming the delete.
	r, _, err := e.p.OpenReception(e.ctx, "recv", uidOf(1, 1), e.id("rc"), at(610))
	e.ok("OpenReception", err)
	f := file(r, 1, 90000)
	f.SeriesUID = uidOf(2, 100)
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{f}, at(610), nil)
	e.ok("AddReceivedFiles", err)
	for _, st := range []tenant.ReceptionStep{{ID: r.ID, From: reception.Receiving, Event: reception.Quiet, At: at(611)},
		{ID: r.ID, From: reception.Complete, Event: reception.Identify, At: at(611), SubjectLabel: "sub-01", SessionLabel: "ses-1",
			Storage: "store"}} {
		_, err = e.p.StepReception(e.ctx, st, nil)
		e.ok("StepReception", err)
	}
	_, err = e.p.ClaimInstances(e.ctx, r.ID, []tenant.InstanceClaim{{SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID,
		SeriesUID: f.SeriesUID, SHA256: f.SHA256}}, at(611), nil)
	e.ok("ClaimInstances", err)
	into := label.Path{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + uidOf(2, 100)}
	key, _ := layout.InstanceKey(into, f.SOPInstanceUID)
	_, err = e.p.CommitScan(e.ctx, tenant.ScanCommit{ReceptionID: r.ID, StudyUID: r.StudyUID, SeriesUID: f.SeriesUID, SubjectLabel: into.Subject,
		SessionLabel: into.Session, ScanLabel: into.Scan, Storage: "store", At: at(611),
		Instances: []tenant.ConfirmedInstance{{SOPInstanceUID: f.SOPInstanceUID, Key: key, Size: f.Size, Length: f.Size, ArchivedSHA256: f.SHA256}}})
	var lc *tenant.LabelConflict
	if !errors.As(err, &lc) || lc.Fault != tenant.FaultHeldByCorrection || lc.Correction != c.ID || fault.KindOf(err) != fault.Conflict {
		e.Fatalf("a commit into a deleted session answered %v, want a label conflict naming %s", err, c.ID)
	}
	_, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Identified, Event: reception.Quarantine, At: at(612)}, nil)
	e.ok("StepReception quarantine", err)
	_ = a
	_, err = e.p.UndoCorrection(e.ctx, c.ID, "the wrong session", at(700), e.audit(), nil)
	e.ok("UndoCorrection", err)
	if h, err := e.p.HeldByCorrection(e.ctx, label.Path{Subject: "sub-01", Session: "ses-1", Scan: "x"}, uidOf(1, 1), uidOf(2, 9)); err != nil ||
		h.Correction != "" {
		e.Fatalf("an undone delete still holds its session: %+v, %v", h, err)
	}
	// A relabel there and back, then there again: the third is a new
	// relabel, not the first one answered again.
	var ids []string
	for i, to := range []string{"sub-02", "sub-01", "sub-02"} {
		from := map[bool]string{true: "sub-01", false: "sub-02"}[i%2 == 0]
		rc, made, err := e.prepareRelabel(tenant.CorrectionTarget{Subject: from}, to, 800+100*i)
		e.ok("PrepareCorrection of a relabel", err)
		if !made {
			e.Fatalf("relabel %d (%s to %s) answered %s, an earlier one", i, from, to, rc.ID)
		}
		e.commit(rc.ID, 801+100*i, nil)
		_, err = e.p.StartPurge(e.ctx, rc.ID, at(802+100*i), &master.AuditEntry{ID: e.id("a"), Time: at(802 + 100*i),
			Method: "PurgeCorrection", Outcome: master.OutcomeDone}, nil)
		e.ok("StartPurge", err)
		_, err = e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: rc.ID, Now: at(803 + 100*i)})
		e.ok("FinishPurge", err)
		ids = append(ids, rc.ID)
	}
	if ids[0] == ids[2] {
		e.Fatal("the third relabel answered the first")
	}
	if _, err := e.p.CatalogSubject(e.ctx, "sub-02"); err != nil {
		e.Fatalf("after three relabels the subject is not sub-02: %v", err)
	}
	// Its delete holds every label it had, and says so before.
	p, err := e.p.PreviewDelete(e.ctx, tenant.CorrectionTarget{Subject: "sub-02"}, at(2000))
	e.ok("PreviewDelete", err)
	if len(p.HeldAfterPurge) != 2 || p.HeldAfterPurge[0] != "sub-02" || p.HeldAfterPurge[1] != "sub-01" {
		e.Fatalf("the preview holds %v after the purge, want sub-02 then its earlier label sub-01", p.HeldAfterPurge)
	}
	d := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-02"}, 2000)
	if m := e.matches(tenant.DeletedProbe{Subject: "sub-01"}); m.Subject != d.ID {
		e.Fatalf("an arrival under the subject's earlier label matches %q, want the delete %s", m.Subject, d.ID)
	}
}

// testClockAndPurgeTargets guards what a purge may remove and when: the
// objects a delete's publish fixed (its instances, and each scan's metadata
// and manifest with the snapshot given), each settled once; an object a
// live row resolves to answered shared; the purge refused while the clock
// reads earlier than a time recorded, an automatic one paused when the
// clock jumped past the step since it was trusted, an admin's trusting the
// clock; and ClearWithdrawn removing the target's quarantined files and
// blanking its ended receptions.
func testClockAndPurgeTargets(e *cenv) {
	e.setupTwo()
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-2"}
	q := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineWasDeleted, Receiver: "recv", Size: 5, SHA256: sum(31),
		SOPClassUID: "1.2.3", SOPInstanceUID: uidOf(3, 777), StudyUID: uidOf(1, 2), SeriesUID: uidOf(2, 200), TransferSyntaxUID: "1.2.840.10008.1.2.1",
		Incoming: "quarantine/qx", ContentKept: true, Quarantined: at(300), Details: map[string]string{"correction": e.newID(1)}}
	_, err := e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{q}, nil)
	e.ok("HoldFiles", err)
	p, err := e.p.PreviewDelete(e.ctx, target, at(400))
	e.ok("PreviewDelete", err)
	if p.Quarantined != 1 || p.Receptions != 1 || len(p.HeldAfterPurge) != 0 {
		e.Fatalf("the preview clears %d quarantined files and %d receptions and holds %v, want 1 and 1 and none", p.Quarantined, p.Receptions,
			p.HeldAfterPurge)
	}
	c := e.prepareDelete(target, 401)
	scans, err := e.p.CorrectionScans(e.ctx, c.ID)
	e.ok("CorrectionScans", err)
	snaps := map[string]tenant.ScanSnapshot{scans[0].SeriesUID: {Manifest: sum(61), ManifestSize: 61, Metadata: sum(62), MetadataSize: 62}}
	if _, err := e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: c.ID, Now: at(600), Grace: grace, Snapshots: snaps}); err != nil {
		e.Fatalf("CommitCorrection: %v", err)
	}
	_, err = e.p.StartPurge(e.ctx, c.ID, at(599), &master.AuditEntry{ID: e.id("a"), Time: at(599), Method: "PurgeCorrection",
		Outcome: master.OutcomeDone}, nil)
	e.wantKind("a purge at a clock earlier than the commit", err, fault.Precondition)
	if !errors.Is(err, tenant.ErrClock) {
		e.Fatalf("the refusal is %v, want the clock's", err)
	}
	e.ok("CheckPurgeClock", e.p.CheckPurgeClock(e.ctx, at(700)))
	err = e.p.CheckPurgeClock(e.ctx, at(700).Add(tenant.MaxClockStep+time.Second))
	e.wantKind("an automatic purge after the clock jumped", err, fault.Precondition)
	if rc, err := e.p.RetentionClock(e.ctx); err != nil || !rc.Paused || rc.Reason != tenant.PauseClockJumped {
		e.Fatalf("after an automatic purge saw the clock jump, retention is %+v (%v), want paused", rc, err)
	}
	_, err = e.p.StartPurge(e.ctx, c.ID, at(700).Add(grace+tenant.MaxClockStep), nil, nil)
	e.wantKind("a server's purge after the clock jumped", err, fault.Precondition)
	when := at(700).Add(grace + tenant.MaxClockStep)
	_, err = e.p.StartPurge(e.ctx, c.ID, when, &master.AuditEntry{ID: e.id("a"), Time: when, Method: "PurgeCorrection",
		Outcome: master.OutcomeDone}, nil)
	e.ok("an admin's purge, trusting the clock", err)
	// The jump paused retention, so automatic purges wait for a person to
	// confirm the time, an admin's purge notwithstanding.
	e.wantKind("an automatic purge before the time is confirmed", e.p.CheckPurgeClock(e.ctx, when.Add(time.Hour)), fault.Precondition)
	if _, _, err := e.p.ConfirmClock(e.ctx, tenant.ClockConfirm{Observed: when.Add(time.Hour), Now: when.Add(time.Hour), Reason: "checked"},
		e.audit(), nil); err != nil {
		e.Fatalf("ConfirmClock: %v", err)
	}
	e.ok("CheckPurgeClock after the time is confirmed", e.p.CheckPurgeClock(e.ctx, when.Add(time.Hour)))
	targets, err := e.p.PurgeTargets(e.ctx, c.ID, 1000)
	e.ok("PurgeTargets", err)
	if len(targets) != 4 || targets[0].Ord != tenant.TargetInstance || targets[2].Ord != tenant.TargetMetadata ||
		targets[2].SHA256 != sum(62) || targets[3].Ord != tenant.TargetManifest || targets[3].Size != 61 || targets[0].Shared {
		e.Fatalf("the delete's objects are %+v, want its two instances, its metadata, its manifest", targets)
	}
	e.ok("SettlePurgeTargets", e.p.SettlePurgeTargets(e.ctx, c.ID, []tenant.PurgeSettled{{Target: targets[0], Outcome: tenant.OutcomeKeptShared},
		{Target: targets[1], Outcome: tenant.OutcomePurged, Removals: []tenant.PurgedObject{{ID: c.ID, Storage: "store", Key: "data/x/1.dcm",
			Size: 3, Disposition: tenant.DispositionRemoved, Now: when}}}}))
	if left, _ := e.p.PurgeTargets(e.ctx, c.ID, 1000); len(left) != 2 || left[0].Ord != tenant.TargetMetadata {
		e.Fatalf("after two are settled %+v are left, want the metadata and the manifest", left)
	}
	paths, err := e.p.ClearWithdrawn(e.ctx, c.ID)
	e.ok("ClearWithdrawn", err)
	if len(paths) == 0 || paths[0] != "quarantine/qx" {
		e.Fatalf("ClearWithdrawn answered %q, want the held file's path first", paths)
	}
	if _, err := e.p.QuarantinedFile(e.ctx, q.ID); fault.KindOf(err) != fault.NotFound {
		e.Fatalf("the deleted session's held file is still recorded: %v", err)
	}
	if again, err := e.p.ClearWithdrawn(e.ctx, c.ID); err != nil || len(again) != 0 {
		e.Fatalf("ClearWithdrawn run again answered %q, %v", again, err)
	}
	rs := allPages(e.penv, 1000, func(pg master.Page) ([]tenant.Reception, string, error) {
		return e.p.ListReceptions(e.ctx, tenant.ReceptionFilter{}, pg)
	})
	for _, r := range rs {
		if r.StudyUID == uidOf(1, 2) || r.SessionLabel == "ses-2" {
			e.Fatalf("a reception of the deleted session still names it: %+v", r)
		}
	}
	f, err := e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: c.ID, Now: when.Add(2 * time.Hour)})
	e.ok("FinishPurge", err)
	if f.Counts.ObjectsKept != 1 || f.Counts.ObjectsOwned != 4 || f.Counts.ObjectsRemoved != 1 {
		e.Fatalf("the purged delete counts %+v, want 1 kept and 1 removed of 4 owned", f.Counts)
	}
	_, err = e.p.StartPurge(e.ctx, c.ID, when, nil, nil)
	e.ok("StartPurge of a purged delete", err)
	err = e.p.CheckPurgeClock(e.ctx, when.Add(time.Hour))
	e.wantKind("an automatic purge at a clock earlier than a purge's end", err, fault.Precondition)
}

// testReplace guards a replace: from a held different file of an instance
// the tenant holds in the same study and series; the instance's next
// revision at its revision key in use at the publish, the old revision
// kept as superseded, the scan's manifest given, the file released; the
// same file asked again answers the replace; ResolveInstances answers both
// revisions; its purge marks the old revision's bytes gone.
func testReplace(e *cenv) {
	a, _ := e.setupTwo()
	in := a.files[0]
	scan := label.Path{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + uidOf(2, 100)}
	q := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineDifferentFile, Receiver: "recv", ReceptionID: a.r.ID,
		Size: 777, SHA256: sum(4242), KeptSHA256: sum(4243), SOPClassUID: in.SOPClassUID, SOPInstanceUID: in.SOPInstanceUID,
		StudyUID: in.StudyUID, SeriesUID: in.SeriesUID, TransferSyntaxUID: in.TransferSyntaxUID, Incoming: "quarantine/q1",
		ContentKept: true, Quarantined: at(400)}
	_, err := e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{q}, nil)
	e.ok("HoldFiles", err)
	prep := func(s int) (tenant.Correction, bool, error) {
		return e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(s), Kind: record.KindReplace, Quarantine: q.ID,
			NewSHA256: sum(5000), NewSize: 780, RestoreLine: []byte(`{"line":1}`), Reason: "corrected re-send", Now: at(s)}, e.audit(), nil)
	}
	c, made, err := prep(500)
	e.ok("PrepareCorrection of a replace", err)
	if !made || c.Target.Instance != in.SOPInstanceUID || c.Target.Path() != scan || c.OldRevision != 1 || c.NewRevision != 2 ||
		c.OldSHA256 != in.SHA256 || c.NewSHA256 != sum(5000) || c.ReceivedSHA256 != sum(4242) || string(c.RestoreLine) != `{"line":1}` {
		e.Fatalf("a new replace is %+v", c)
	}
	if again, made, err := prep(501); err != nil || made || again.ID != c.ID {
		e.Fatalf("the same file replaced again answers %+v, %v, %v", again, made, err)
	}
	before := e.totals()
	// The new revision was archived as received, as the one it replaces.
	_, err = e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: c.ID, Now: at(600), Grace: grace,
		Manifests: map[string]tenant.SHA256{in.SeriesUID: sum(6000)}, Replaced: &tenant.InstanceProvenance{}})
	e.ok("CommitCorrection of a replace", err)
	got := e.wantState(c.ID, record.StateCommitted)
	if got.RestoreLine != nil {
		e.Fatal("a published replace keeps its line to write back")
	}
	h, err := e.p.HeldInstance(e.ctx, scan, in.SOPInstanceUID)
	e.ok("HeldInstance", err)
	key, _ := layout.RevisionKey(scan, in.SOPInstanceUID, 2)
	if h.Key != key || h.Size != 780 || h.ArchivedSHA256 != sum(5000) || h.SHA256 != sum(4242) {
		e.Fatalf("the replaced instance is %+v, want revision 2 at %s", h, key)
	}
	sc, err := e.p.CatalogScan(e.ctx, scan)
	e.ok("CatalogScan", err)
	if sc.ManifestSHA256 != sum(6000) {
		e.Fatal("the publish did not give the scan its manifest")
	}
	if t := e.totals(); t != before {
		e.Fatalf("a replace changed the totals to %+v", t)
	}
	e.wantTotalsRecounted("after a replace")
	qf, err := e.p.QuarantinedFile(e.ctx, q.ID)
	e.ok("QuarantinedFile", err)
	if qf.State != tenant.QuarantineReleased {
		e.Fatalf("the quarantined file is %s after the replace", qf.State)
	}
	r, err := e.p.ResolveInstances(e.ctx, []tenant.InstanceRef{{UID: in.SOPInstanceUID, Revision: 1}, {UID: in.SOPInstanceUID, Revision: 2},
		{UID: in.SOPInstanceUID, Revision: 3}})
	e.ok("ResolveInstances", err)
	if r[0].Disposition != tenant.ResolvedSuperseded || r[0].Correction != c.ID || !r[0].BytesKept || r[0].SHA256 != in.SHA256 ||
		r[0].CurrentRevision != 2 || r[1].Disposition != tenant.ResolvedCurrent || r[1].SHA256 != sum(5000) ||
		r[2].Disposition != tenant.ResolvedUnknown {
		e.Fatalf("resolve answers %+v", r)
	}
	_, err = e.p.StartPurge(e.ctx, c.ID, at(600).Add(grace), nil, nil)
	e.ok("StartPurge of a replace", err)
	_, err = e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: c.ID, Now: at(600).Add(grace)})
	e.ok("FinishPurge of a replace", err)
	r, err = e.p.ResolveInstances(e.ctx, []tenant.InstanceRef{{UID: in.SOPInstanceUID, Revision: 1}})
	e.ok("ResolveInstances after the purge", err)
	if r[0].Disposition != tenant.ResolvedSuperseded || r[0].BytesKept {
		e.Fatalf("after the purge resolve answers %+v", r[0])
	}
	_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(700), Kind: record.KindReplace, Quarantine: "q-none",
		NewSHA256: sum(1), NewSize: 1, RestoreLine: []byte("x"), Reason: "r", Now: at(700)}, e.audit(), nil)
	e.wantKind("a replace from a file not held", err, fault.NotFound)
}
