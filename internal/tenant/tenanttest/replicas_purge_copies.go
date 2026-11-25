package tenanttest

import (
	"slices"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What keeps a purge's copies exact: a switch and a purge excluding each
// other, an attach owing the records of the corrections before it, and the
// records owed again at each step of a purge.

// wantMessage fails unless err is a precondition failure saying phrase.
func (e *renv) wantMessage(what string, err error, phrase string) {
	e.Helper()
	e.wantPrecondition(what, err)
	if !strings.Contains(fault.Message(err), phrase) {
		e.Errorf("%s says %q, want %q", what, fault.Message(err), phrase)
	}
}

// testSwitchExcludesPurge guards that a switch and a purge exclude each
// other: a switch is neither begun nor previewed ready while a purge of its
// primary has an object not settled; once every object is settled it is
// begun, and while it runs the purge's objects are neither read, settled,
// added to nor recorded, so a removal is recorded at the generation it was
// made at.
func testSwitchExcludesPurge(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	e.copyAll(20)
	id := e.commitDeleteOf(seriesB, 30)
	e.startPurge(id, 40)
	start := tenant.SwitchStart{ID: "sw-1", Primary: "store", Replica: "copy", Token: "tok-1", Now: at(41)}
	_, _, err := e.p.BeginSwitch(e.ctx, start, e.audit(), nil)
	e.wantMessage("BeginSwitch while a purge has objects not settled", err, tenant.BlockerPurgeUnfinished)
	facts, err := e.p.SwitchFacts(e.ctx, "store", "copy")
	e.ok("SwitchFacts", err)
	if !slices.Contains(tenant.SwitchBlockers(facts), tenant.BlockerPurgeUnfinished) {
		e.Errorf("the switch's blockers while a purge is unfinished are %v", tenant.SwitchBlockers(facts))
	}
	targets, err := e.p.PurgeTargets(e.ctx, id, 100)
	e.ok("PurgeTargets", err)
	var settled []tenant.PurgeSettled
	for _, t := range targets {
		settled = append(settled, tenant.PurgeSettled{Target: t, Outcome: tenant.OutcomePurged,
			Removals: []tenant.PurgedObject{removal(id, "store", t, 42), removal(id, "copy", t, 42)}})
	}
	e.ok("SettlePurgeTargets", e.p.SettlePurgeTargets(e.ctx, id, settled))
	_, _, err = e.p.BeginSwitch(e.ctx, start, e.audit(), nil)
	e.ok("BeginSwitch once every object is settled", err)
	_, err = e.p.PurgeTargets(e.ctx, id, 100)
	e.wantPrecondition("PurgeTargets while a switch runs", err)
	_, err = e.p.PurgedTargets(e.ctx, id, tenant.PurgeTarget{}, 100)
	e.wantPrecondition("PurgedTargets while a switch runs", err)
	e.wantPrecondition("SettlePurgeTargets while a switch runs", e.p.SettlePurgeTargets(e.ctx, id, nil))
	e.wantPrecondition("AddPurgeTargets while a switch runs", e.p.AddPurgeTargets(e.ctx, id, targets[:1]))
	e.wantPrecondition("RecordPurgedObject while a switch runs", e.p.RecordPurgedObject(e.ctx, removal(id, "store", targets[0], 43)))
	_, err = e.p.RefuseSwitch(e.ctx, "sw-1", tenant.RefusalRestarted, at(44), e.serverAudit())
	e.ok("RefuseSwitch", err)
	done, err := e.p.PurgedTargets(e.ctx, id, tenant.PurgeTarget{}, 2)
	e.ok("PurgedTargets once the switch ended", err)
	rest, err := e.p.PurgedTargets(e.ctx, id, done[len(done)-1], 100)
	e.ok("PurgedTargets after a page", err)
	if len(done) != 2 || len(rest) != len(targets)-2 || rest[0].Ord < done[1].Ord {
		e.Errorf("the purged targets read a page of 2 then the rest are %d and %d, want 2 and %d in order", len(done), len(rest),
			len(targets)-2)
	}
}

// testAttachOwesRecords guards that an attach owes the replica the record
// of every committed correction on its primary in its own transaction, so
// a switch to it waits for the record, and that no replica is attached
// while a correction on the primary is purging.
func testAttachOwesRecords(e *renv) {
	e.replicaSetup()
	e.addStorage("copy2")
	id := e.commitDeleteOf(seriesB, 30)
	e.attach("copy", "store", 40)
	if c := e.correction(id); !slices.Equal(c.RecordsOwed, []string{"copy"}) {
		e.Fatalf("a replica attached after a delete's publish is owed %v, want [copy]", c.RecordsOwed)
	}
	facts, err := e.p.SwitchFacts(e.ctx, "store", "copy")
	e.ok("SwitchFacts", err)
	if facts.RecordsOwed != 1 {
		e.Errorf("a switch to a replica attached after a delete counts %d records owed, want 1", facts.RecordsOwed)
	}
	e.startPurge(id, 50)
	_, _, _, err = e.p.AttachReplica(e.ctx, "copy2", "store", at(51), e.audit(), nil)
	e.wantMessage("AttachReplica while a correction on the primary is purging", err, tenant.RefusalAttachPurging)
	_, err = e.p.AddStorage(e.ctx, tenant.Storage{Name: "copy3", Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "fast", Path: "copy3"}, ReplicaOf: "store"}, at(52), e.audit(), nil)
	e.wantMessage("AddStorage of a replica while a correction on the primary is purging", err, tenant.RefusalAttachPurging)
}

// testPurgeStepsOweRecords guards that the start and the end of a purge owe
// the record at the new state to every replica in their own transactions,
// so a stop before a replica's record is rewritten leaves it owed.
func testPurgeStepsOweRecords(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	e.copyAll(20)
	id := e.commitDeleteOf(seriesB, 30)
	e.ok("MarkRecordWritten", e.p.MarkRecordWritten(e.ctx, id, "copy", record.StateCommitted))
	e.startPurge(id, 40)
	if c := e.correction(id); !slices.Equal(c.RecordsOwed, []string{"copy"}) {
		e.Errorf("after the purge's start the record is owed to %v, want [copy]", c.RecordsOwed)
	}
	e.ok("MarkRecordWritten", e.p.MarkRecordWritten(e.ctx, id, "copy", record.StatePurging))
	_, err := e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: id, Now: at(50)})
	e.ok("FinishPurge", err)
	if c := e.correction(id); c.State != record.StatePurged || !slices.Equal(c.RecordsOwed, []string{"copy"}) {
		e.Errorf("after the purge's end the correction is %s, its record owed to %v; want purged, owed to [copy]", c.State,
			c.RecordsOwed)
	}
}

// testDropRecordOwed guards that a record owed to a storage that is not a
// copy of the correction's storages is dropped, and one owed to a replica
// is kept.
func testDropRecordOwed(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	id := e.commitDeleteOf(seriesB, 30)
	e.ok("OweRecord", e.p.OweRecord(e.ctx, id, "elsewhere", at(31)))
	if dropped, err := e.p.DropRecordOwed(e.ctx, id, "copy"); err != nil || dropped {
		e.Errorf("DropRecordOwed of a replica = %v, %v; want kept", dropped, err)
	}
	if dropped, err := e.p.DropRecordOwed(e.ctx, id, "elsewhere"); err != nil || !dropped {
		e.Errorf("DropRecordOwed of a storage that is not a copy = %v, %v; want dropped", dropped, err)
	}
	if c := e.correction(id); !slices.Equal(c.RecordsOwed, []string{"copy"}) {
		e.Errorf("after the drops the record is owed to %v, want [copy]", c.RecordsOwed)
	}
}

// testDetachAbandonsUnseeded guards a detach with abandon_purges of a
// replica a delete counts whose objects its purge has not yet fixed (one
// known only from its record): the scan's manifest and metadata are
// recorded unknown on the replica, so nothing claims the replica held none
// of the delete.
func testDetachAbandonsUnseeded(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	rs, ok := e.p.(tenant.Restoring)
	if !ok {
		e.Fatal("the store does not implement tenant.Restoring")
	}
	id, err := tenant.NewCorrectionID(at(30))
	e.ok("NewCorrectionID", err)
	rec := record.Correction{ID: id, Format: record.Format, Kind: record.KindDelete, Level: record.LevelScan, State: record.StateCommitted,
		ReasonSHA256: record.ReasonDigest("consent withdrawn"), Created: at(30), CommittedAt: at(31), PurgeAfter: at(31).Add(time.Hour),
		Scans: []record.Scan{{Subject: "sub-01", Session: "ses-1", Scan: "scan-gone", StudyUID: uidOf(1, 1), SeriesUID: uidOf(2, 900)}}}
	_, err = rs.AdoptCorrection(e.ctx, rec, "store", at(32))
	e.ok("AdoptCorrection", err)
	_, abandoned, err := e.p.DetachReplica(e.ctx, tenant.ReplicaDetach{Replica: "copy", Abandon: true, Reason: "the disk is gone",
		Now: at(40)}, e.audit(), nil)
	e.ok("DetachReplica abandoning", err)
	if !slices.Contains(abandoned, id) {
		e.Fatalf("the detach abandoned %v, want %s among them", abandoned, id)
	}
	var onCopy tenant.StorageDisposition
	for _, d := range e.correction(id).Dispositions {
		if d.Storage == "copy" {
			onCopy = d
		}
	}
	if onCopy.Unknown != 2 || onCopy.Removed != 0 || onCopy.ReplicaOf != "store" {
		e.Errorf("the abandoned replica's disposition of a delete not seeded is %+v, want its manifest and metadata unknown", onCopy)
	}
}
