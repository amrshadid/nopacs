package tenanttest

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// runReplication runs the tests of replica storages, their state per scan
// and the queue of copies, recalls, repairs and switches.
func runReplication(t *testing.T, open OpenEvents, scratch Scratch) {
	tests := []struct {
		name string
		fn   func(*renv)
	}{
		{"ReplicaStorages", testReplicaStorages},
		{"ReplicaTiersAndDeclarations", testReplicaTiers},
		{"ReplicaAttachDetach", testReplicaAttachDetach},
		{"ReplicaDetachCountsCorrections", testReplicaDetachCorrections},
		{"ReplicaQueue", testReplicaQueue},
		{"ReplicaQueueFences", testReplicaQueueFences},
		{"ReplicaDamage", testReplicaDamage},
		{"ReplicaFollowsCorrections", testReplicaFollowsCorrections},
		{"ReplicaFollowsPurge", testReplicaFollowsPurge},
		{"ReplicaFollowsRestoreHide", testReplicaFollowsRestoreHide},
		{"ReplicaFollowsRebuild", testReplicaFollowsRebuild},
		{"ReplicaBackfill", testReplicaBackfill},
		{"Recalls", testRecalls},
		{"RecallsRefused", testRecallsRefused},
		{"Repairs", testRepairs},
		{"SwitchSwaps", testSwitchSwaps},
		{"SwitchRefusals", testSwitchRefusals},
		{"SwitchSecrets", testSwitchSecrets},
		{"SwitchHoldsChanges", testSwitchHoldsChanges},
		{"RepairAcrossSwap", testRepairAcrossSwap},
		{"RepairHoldsScan", testRepairHoldsScan},
		{"RepairHoldsCorrectionSteps", testRepairHoldsCorrectionSteps},
		{"RepairReleasedAndCanceled", testRepairReleasedAndCanceled},
		{"RepairWaitsForEarlierAttempt", testRepairWaitsForEarlierAttempt},
		{"RepairRefusedWhileSwitching", testRepairRefusedWhileSwitching},
		{"RepairJobCounts", testRepairJobCounts},
		{"CorrectionRecordsOwed", testCorrectionRecordsOwed},
		{"CorrectionPurgeCopies", testCorrectionPurgeCopies},
		{"RelabelKeepsReplicaInStep", testRelabelKeepsReplicaInStep},
		{"DetachAbandonsCopies", testDetachAbandonsCopies},
		{"CorrectionCopiesSnapshot", testCorrectionCopiesSnapshot},
		{"SwitchExcludesPurge", testSwitchExcludesPurge},
		{"AttachOwesRecords", testAttachOwesRecords},
		{"PurgeStepsOweRecords", testPurgeStepsOweRecords},
		{"DropRecordOwed", testDropRecordOwed},
		{"DetachAbandonsUnseeded", testDetachAbandonsUnseeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := open(t, SuiteTenant)
			e := &renv{senv: &senv{eenv: &eenv{penv: &penv{env: &env{T: t, s: s, ctx: context.Background()}, p: s}, ev: s, openEv: open}}}
			if scratch != nil {
				e.scratch = scratch(t)
				e.another = func() string { return scratch(t) }
			}
			tt.fn(e)
		})
	}
}

// The series of the two scans replicaSetup archives.
var (
	seriesA = uidOf(2, 100)
	seriesB = uidOf(2, 101)
)

// replicaSetup archives, on the storage "store" through the receiver recv,
// one session of two scans of two instances (series seriesA and seriesB,
// their manifests sum(0) and sum(1)), and adds the empty storage "copy".
func (e *renv) replicaSetup() {
	e.Helper()
	e.addReceiver("recv", "store", 11112)
	e.finish(e.archive("recv", 1, 2, 2, 0, at(0), "ses-1"))
	e.addStorage("copy")
}

// attach makes replica a replica of primary at second s, failing the test
// on an error, and returns the scans it queued.
func (e *renv) attach(replica, primary string, s int) int64 {
	e.Helper()
	_, n, attached, err := e.p.AttachReplica(e.ctx, replica, primary, at(s), e.audit(), nil)
	e.ok("AttachReplica", err)
	if !attached {
		e.Fatalf("AttachReplica of %s answered it as it stood", replica)
	}
	return n
}

// status returns the replica's status, failing the test when it is not
// listed.
func (e *renv) status(replica string) tenant.ReplicaStatus {
	e.Helper()
	list, _, err := e.p.ListReplicas(e.ctx, "", master.Page{Limit: master.MaxPageLimit})
	e.ok("ListReplicas", err)
	for _, s := range list {
		if s.Replica == replica {
			return s
		}
	}
	e.Fatalf("the replica %s is not listed in %+v", replica, list)
	return tenant.ReplicaStatus{}
}

// wantReplicaCounts fails unless the replica's counts are these.
func (e *renv) wantReplicaCounts(what, replica string, pending, copying, inStep, damaged, objects int64) {
	e.Helper()
	s := e.status(replica)
	if s.Pending != pending || s.Copying != copying || s.InStep != inStep || s.Damaged != damaged || s.ObjectsDamaged != objects {
		e.Errorf("%s: the counts of %s are pending %d, copying %d, in step %d, damaged %d, objects %d; want %d, %d, %d, %d, %d",
			what, replica, s.Pending, s.Copying, s.InStep, s.Damaged, s.ObjectsDamaged, pending, copying, inStep, damaged, objects)
	}
}

// rowState returns the state of the replica's row of the series, or "" when
// it has none.
func (e *renv) rowState(replica, series string) tenant.ReplicaScanState {
	e.Helper()
	r, err := e.p.ReplicaScan(e.ctx, replica, series)
	if fault.KindOf(err) == fault.NotFound {
		return ""
	}
	e.ok("ReplicaScan", err)
	return r.State
}

// claimAll claims every row due at second s.
func (e *renv) claimAll(s int) []tenant.ReplicaClaim {
	e.Helper()
	c, err := e.p.ClaimReplicaScans(e.ctx, "copier", at(s), time.Minute, tenant.MaxReplicaClaim)
	e.ok("ClaimReplicaScans", err)
	return c
}

// copyAll claims every row due at second s and ends each at its claim's
// manifest, as a copy that put and verified everything does.
func (e *renv) copyAll(s int) {
	e.Helper()
	for _, c := range e.claimAll(s) {
		state, err := e.p.EndReplicaScan(e.ctx, c.Fence, c.ManifestSHA256, at(s+1))
		e.ok("EndReplicaScan", err)
		if state != tenant.ReplicaInStep {
			e.Fatalf("a copy ended at its claim's manifest left the scan %s", state)
		}
	}
}

// testReplicaStorages guards the rules of replica storages that AddStorage,
// SetStorage, RemoveStorage and AddReceiver keep: a storage added with
// ReplicaOf is a replica from the start, with every scan of its primary
// pending for it as its backfill; a primary has at most two replicas; a
// replica of a replica, and of a primary the tenant does not have, are
// refused; a receiver may not name a replica; where a replica's files are,
// or a primary's with replicas, does not change; and neither a replica nor
// a primary with one is removed.
func testReplicaStorages(e *renv) {
	e.replicaSetup()
	r := dirStorage("copy2", "copy2")
	r.ReplicaOf = "store"
	got, err := e.p.AddStorage(e.ctx, r, at(10), e.audit(), nil)
	e.ok("AddStorage of a replica", err)
	if got.ReplicaOf != "store" || got.Tier != tenant.TierOnline || got.SwitchGeneration != 0 {
		e.Errorf("the replica added is %+v", got)
	}
	st := e.status("copy2")
	if st.Primary != "store" || st.Pending != 2 || st.BackfillPending != 2 || !st.Attached.Equal(at(10)) ||
		!st.OldestQueued.Equal(at(10)) || !st.BackfillDone.IsZero() || st.SharesBucket {
		e.Errorf("the status of a replica added with data on its primary is %+v", st)
	}
	for _, s := range []string{seriesA, seriesB} {
		row, err := e.p.ReplicaScan(e.ctx, "copy2", s)
		e.ok("ReplicaScan", err)
		if row.State != tenant.ReplicaPending || !row.Backfill || !row.Queued.Equal(at(10)) || !row.Due.Equal(at(10)) {
			e.Errorf("a scan queued by an attach is %+v", row)
		}
	}
	c := s3Storage("cold", "cold/")
	c.ReplicaOf, c.Tier, c.S3.RecallDays, c.S3.RecallSpeed = "store", tenant.TierArchive, 3, tenant.RecallBulk
	got, err = e.p.AddStorage(e.ctx, c, at(11), e.audit(), nil)
	e.ok("AddStorage of an archive-tier replica", err)
	if got.Tier != tenant.TierArchive || got.S3.RecallDays != 3 || got.S3.RecallSpeed != tenant.RecallBulk {
		e.Errorf("the archive-tier replica added is %+v", got)
	}
	third := dirStorage("third", "third")
	third.ReplicaOf = "store"
	_, err = e.p.AddStorage(e.ctx, third, at(12), e.audit(), nil)
	e.wantPrecondition("AddStorage of a third replica", err)
	deep := dirStorage("deep", "deep")
	deep.ReplicaOf = "copy2"
	_, err = e.p.AddStorage(e.ctx, deep, at(12), e.audit(), nil)
	e.wantPrecondition("AddStorage of a replica of a replica", err)
	deep.ReplicaOf = "nowhere"
	_, err = e.p.AddStorage(e.ctx, deep, at(12), e.audit(), nil)
	e.wantPrecondition("AddStorage of a replica of a storage the tenant does not have", err)
	if _, err := e.p.Storage(e.ctx, "deep"); fault.KindOf(err) != fault.NotFound {
		e.Errorf("a refused replica was added: %v", err)
	}
	_, err = e.p.AddReceiver(e.ctx, receiverOf("to-copy", "copy2", 11113), at(13), e.audit(), nil)
	e.wantPrecondition("AddReceiver archiving to a replica", err)
	_, err = e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "recv", Storage: "cold"}, []tenant.ReceiverField{tenant.FieldStorage},
		at(13), e.audit(), nil)
	e.wantPrecondition("SetReceiver archiving to an archive-tier replica", err)
	_, err = e.p.SetStorage(e.ctx, tenant.Storage{Name: "copy2", Directory: tenant.DirectoryStorage{Path: "elsewhere"}},
		[]tenant.StorageField{tenant.FieldDirectoryPath}, at(14), e.audit(), nil)
	e.wantPrecondition("SetStorage moving a replica's files", err)
	// A primary with a replica holds its place even while it holds nothing.
	e.addStorage("p2")
	r2 := dirStorage("r2", "r2")
	r2.ReplicaOf = "p2"
	_, err = e.p.AddStorage(e.ctx, r2, at(15), e.audit(), nil)
	e.ok("AddStorage of an empty primary's replica", err)
	if st := e.status("r2"); st.Pending != 0 || st.BackfillPending != 0 || !st.BackfillDone.Equal(at(15)) {
		e.Errorf("the replica of an empty primary is %+v; want its backfill done at once", st)
	}
	_, err = e.p.SetStorage(e.ctx, tenant.Storage{Name: "p2", Directory: tenant.DirectoryStorage{Path: "moved"}},
		[]tenant.StorageField{tenant.FieldDirectoryPath}, at(16), e.audit(), nil)
	e.wantPrecondition("SetStorage moving a primary's files while it has a replica", err)
	_, err = e.p.SetStorage(e.ctx, tenant.Storage{Name: "p2", CapacityBytes: 1 << 30}, []tenant.StorageField{tenant.FieldCapacityBytes},
		at(16), e.audit(), nil)
	e.ok("SetStorage of a primary's capacity", err)
	_, err = e.p.RemoveStorage(e.ctx, "r2", e.audit(), nil)
	e.wantPrecondition("RemoveStorage of a replica", err)
	_, err = e.p.RemoveStorage(e.ctx, "p2", e.audit(), nil)
	e.wantPrecondition("RemoveStorage of a primary with a replica", err)
	list, next, err := e.p.ListReplicas(e.ctx, "", master.Page{Limit: 1})
	e.ok("ListReplicas", err)
	var names []string
	for len(list) > 0 {
		names = append(names, list[0].Primary+"/"+list[0].Replica)
		if next == "" {
			break
		}
		list, next, err = e.p.ListReplicas(e.ctx, "", master.Page{Limit: 1, Cursor: next})
		e.ok("ListReplicas, next page", err)
	}
	if want := []string{"p2/r2", "store/cold", "store/copy2"}; !slices.Equal(names, want) {
		e.Errorf("the replicas listed page by page are %v, want %v", names, want)
	}
	only, _, err := e.p.ListReplicas(e.ctx, "p2", master.Page{Limit: 10})
	e.ok("ListReplicas of one primary", err)
	if len(only) != 1 || only[0].Replica != "r2" {
		e.Errorf("the replicas of p2 are %+v", only)
	}
	if _, _, err := e.p.ListReplicas(e.ctx, "store", master.Page{Limit: 10, Cursor: next}); err == nil && next != "" {
		e.Error("a cursor of another filter was taken")
	}
}

// testReplicaTiers guards the tier and the declaration of encryption: a
// directory is never of the archive tier, an S3 storage is of it only as a
// replica (added so, or changed so once a replica), the recall fields are
// kept only on the archive tier, the empty tier reads as online, and only a
// directory takes a declaration of encryption, which a change sets.
func testReplicaTiers(e *renv) {
	d := dirStorage("arch", "arch")
	d.Tier = tenant.TierArchive
	_, err := e.p.AddStorage(e.ctx, d, at(1), e.audit(), nil)
	e.wantKind("AddStorage of an archive-tier directory", err, fault.Invalid)
	s := s3Storage("lone", "lone/")
	s.Tier = tenant.TierArchive
	_, err = e.p.AddStorage(e.ctx, s, at(1), e.audit(), nil)
	e.wantPrecondition("AddStorage of an archive-tier storage that is not a replica", err)
	s = s3Storage("warm", "warm/")
	s.Tier, s.S3.RecallDays, s.S3.RecallSpeed = "", 9, tenant.RecallExpedited
	got, err := e.p.AddStorage(e.ctx, s, at(2), e.audit(), nil)
	e.ok("AddStorage of an online S3 storage", err)
	if got.Tier != tenant.TierOnline || got.S3.RecallDays != 0 || got.S3.RecallSpeed != "" {
		e.Errorf("an online storage added with recall fields reads %+v; want online, no recall fields", got)
	}
	_, err = e.p.SetStorage(e.ctx, tenant.Storage{Name: "warm", Tier: tenant.TierArchive}, []tenant.StorageField{tenant.FieldTier},
		at(3), e.audit(), nil)
	e.wantPrecondition("SetStorage of the archive tier on a storage that is not a replica", err)
	e.addStorage("main")
	r := s3Storage("deep", "deep/")
	r.ReplicaOf = "main"
	_, err = e.p.AddStorage(e.ctx, r, at(4), e.audit(), nil)
	e.ok("AddStorage of an online S3 replica", err)
	got, err = e.p.SetStorage(e.ctx, tenant.Storage{Name: "deep", Tier: tenant.TierArchive, S3: tenant.S3Storage{RecallDays: 30}},
		[]tenant.StorageField{tenant.FieldTier, tenant.FieldS3RecallDays}, at(5), e.audit(), nil)
	e.ok("SetStorage of a replica to the archive tier", err)
	if got.Tier != tenant.TierArchive || got.S3.RecallDays != 30 {
		e.Errorf("the replica changed to the archive tier reads %+v", got)
	}
	_, err = e.p.SetStorage(e.ctx, tenant.Storage{Name: "deep", S3: tenant.S3Storage{RecallDays: 366}},
		[]tenant.StorageField{tenant.FieldS3RecallDays}, at(6), e.audit(), nil)
	e.wantKind("SetStorage of 366 recall days", err, fault.Invalid)
	got, err = e.p.SetStorage(e.ctx, tenant.Storage{Name: "deep", Tier: tenant.TierOnline}, []tenant.StorageField{tenant.FieldTier},
		at(7), e.audit(), nil)
	e.ok("SetStorage of a replica back to online", err)
	if got.S3.RecallDays != 0 {
		e.Errorf("a storage back to online keeps %d recall days", got.S3.RecallDays)
	}
	enc := dirStorage("enc", "enc")
	enc.EncryptedAtRestDeclared = true
	got, err = e.p.AddStorage(e.ctx, enc, at(8), e.audit(), nil)
	e.ok("AddStorage of a directory declared encrypted", err)
	want := enc
	want.Created, want.Updated = at(8), at(8)
	if !reflect.DeepEqual(got, want) {
		e.Errorf("the directory declared encrypted reads %+v, want %+v", got, want)
	}
	got, err = e.p.SetStorage(e.ctx, tenant.Storage{Name: "enc"}, []tenant.StorageField{tenant.FieldEncryptionDeclared}, at(9), e.audit(), nil)
	e.ok("SetStorage of the declaration", err)
	if got.EncryptedAtRestDeclared {
		e.Error("the declaration set false still reads true")
	}
	s = s3Storage("sealed", "sealed/")
	s.EncryptedAtRestDeclared = true
	_, err = e.p.AddStorage(e.ctx, s, at(10), e.audit(), nil)
	e.wantKind("AddStorage of an S3 storage declared encrypted", err, fault.Invalid)
	_, err = e.p.SetStorage(e.ctx, tenant.Storage{Name: "warm", EncryptedAtRestDeclared: true},
		[]tenant.StorageField{tenant.FieldEncryptionDeclared}, at(11), e.audit(), nil)
	e.wantKind("SetStorage of a declaration on an S3 storage", err, fault.Invalid)
}

// testReplicaAttachDetach guards AttachReplica and DetachReplica: a storage
// that holds data never becomes a replica; an attach queues every scan of
// the primary as its backfill and a repeat answers the replica as it
// stands, queuing nothing and writing its audit entry; a detach drops the
// replica's rows and counts and touches nothing else, and a repeat finds a
// storage that is not a replica.
func testReplicaAttachDetach(e *renv) {
	e.replicaSetup()
	_, _, _, err := e.p.AttachReplica(e.ctx, "store", "copy", at(10), e.audit(), nil)
	e.wantPrecondition("AttachReplica of a storage that holds archived files", err)
	_, _, _, err = e.p.AttachReplica(e.ctx, "gone", "store", at(10), e.audit(), nil)
	e.wantKind("AttachReplica of a storage the tenant does not have", err, fault.NotFound)
	_, _, _, err = e.p.AttachReplica(e.ctx, "copy", "gone", at(10), e.audit(), nil)
	e.wantPrecondition("AttachReplica to a primary the tenant does not have", err)
	_, _, _, err = e.p.AttachReplica(e.ctx, "copy", "copy", at(10), e.audit(), nil)
	e.wantPrecondition("AttachReplica of a storage to itself", err)
	if n := e.attach("copy", "store", 20); n != 2 {
		e.Errorf("an attach queued %d scans, want the primary's 2", n)
	}
	again := e.audit()
	s, n, attached, err := e.p.AttachReplica(e.ctx, "copy", "store", at(30), again, nil)
	e.ok("AttachReplica again", err)
	if attached || n != 0 || s.ReplicaOf != "store" {
		e.Errorf("an attach repeated answered %+v, %d, %v; want the replica as it stands", s, n, attached)
	}
	if !e.auditIDs()[again.ID] {
		e.Error("an attach repeated wrote no audit entry")
	}
	if st := e.status("copy"); !st.Attached.Equal(at(20)) || st.Pending != 2 {
		e.Errorf("an attach repeated changed the replica's status: %+v", st)
	}
	_, err = e.p.BackfillReplica(e.ctx, "store", at(31), e.audit(), nil)
	e.wantPrecondition("BackfillReplica of a storage that is not a replica", err)
	_, _, err = e.p.DetachReplica(e.ctx, tenant.ReplicaDetach{Replica: "copy", Abandon: true, Now: at(40)}, e.audit(), nil)
	e.wantKind("DetachReplica abandoning with no reason", err, fault.Invalid)
	_, _, err = e.p.DetachReplica(e.ctx, tenant.ReplicaDetach{Replica: "copy", Reason: "why", Now: at(40)}, e.audit(), nil)
	e.wantKind("DetachReplica with a reason and no abandon", err, fault.Invalid)
	d, abandoned, err := e.p.DetachReplica(e.ctx, tenant.ReplicaDetach{Replica: "copy", Now: at(40)}, e.audit(), nil)
	e.ok("DetachReplica", err)
	if d.ReplicaOf != "" || len(abandoned) != 0 {
		e.Errorf("the detached storage is %+v, abandoned %v", d, abandoned)
	}
	if st := e.rowState("copy", seriesA); st != "" {
		e.Errorf("a detached replica keeps a row of a scan, %s", st)
	}
	if list, _, err := e.p.ListReplicas(e.ctx, "", master.Page{Limit: 10}); err != nil || len(list) != 0 {
		e.Errorf("ListReplicas after the detach = %+v, %v", list, err)
	}
	_, _, err = e.p.DetachReplica(e.ctx, tenant.ReplicaDetach{Replica: "copy", Now: at(41)}, e.audit(), nil)
	e.wantPrecondition("DetachReplica repeated", err)
	// Detached, it is an ordinary storage again: removed, as one that holds
	// nothing the catalog names.
	_, err = e.p.RemoveStorage(e.ctx, "copy", e.audit(), nil)
	e.ok("RemoveStorage of a detached replica", err)
}

// testReplicaDetachCorrections guards that a detach is refused while a
// committed correction of a scan on the primary counts the replica among
// its copies, and goes ahead with abandon and a reason, answering that
// correction; and that the delete's publish dropped the replica's row of
// the scan it hid.
func testReplicaDetachCorrections(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + seriesB}
	p, err := e.p.PreviewDelete(e.ctx, target, at(20))
	e.ok("PreviewDelete", err)
	id, err := tenant.NewCorrectionID(at(20))
	e.ok("NewCorrectionID", err)
	_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: p.Confirm, Now: at(21)}, e.audit(), nil)
	e.ok("PrepareCorrection", err)
	_, err = e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: id, Now: at(22), Grace: time.Hour})
	e.ok("CommitCorrection", err)
	if st := e.rowState("copy", seriesB); st != "" {
		e.Errorf("the replica keeps a row, %s, of the scan a delete hid", st)
	}
	if st := e.rowState("copy", seriesA); st != tenant.ReplicaPending {
		e.Errorf("the replica's row of the scan kept is %s, want pending", st)
	}
	e.wantReplicaCounts("after the delete's publish", "copy", 1, 0, 0, 0, 0)
	if st := e.status("copy"); st.BackfillPending != 1 {
		e.Errorf("the backfill owes %d scans after the delete, want 1", st.BackfillPending)
	}
	_, _, err = e.p.DetachReplica(e.ctx, tenant.ReplicaDetach{Replica: "copy", Now: at(30)}, e.audit(), nil)
	e.wantPrecondition("DetachReplica while a committed delete counts it", err)
	_, abandoned, err := e.p.DetachReplica(e.ctx, tenant.ReplicaDetach{Replica: "copy", Abandon: true, Reason: "the disk is gone",
		Now: at(31)}, e.audit(), nil)
	e.ok("DetachReplica abandoning", err)
	if !slices.Equal(abandoned, []string{id}) {
		e.Errorf("the detach abandoned %v, want [%s]", abandoned, id)
	}
}
