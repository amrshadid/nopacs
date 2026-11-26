package tenanttest

import (
	"slices"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/snapshot"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Corrections that reach every copy: the records owed to replicas, the
// purge's progress per copy, a relabel's copies on a replica in step, and
// the copies a detach abandons.

// commitDeleteOf prepares and publishes a delete of the scan of series at
// seconds s to s+2, with a snapshot of its manifest and metadata, and
// returns its id.
func (e *renv) commitDeleteOf(series string, s int) string {
	e.Helper()
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + series}
	p, err := e.p.PreviewDelete(e.ctx, target, at(s))
	e.ok("PreviewDelete", err)
	id, err := tenant.NewCorrectionID(at(s))
	e.ok("NewCorrectionID", err)
	_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: p.Confirm, Now: at(s + 1)}, e.audit(), nil)
	e.ok("PrepareCorrection", err)
	_, err = e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: id, Now: at(s + 2), Grace: time.Hour,
		Snapshots: map[string]tenant.ScanSnapshot{series: {Manifest: sum(700), Metadata: sum(701), ManifestSize: 10, MetadataSize: 20}}})
	e.ok("CommitCorrection", err)
	return id
}

// startPurge starts the purge of the correction of id at second s, as an
// admin asks it.
func (e *renv) startPurge(id string, s int) {
	e.Helper()
	a := e.audit()
	_, err := e.p.StartPurge(e.ctx, id, at(s), &a, nil)
	e.ok("StartPurge", err)
}

// correction reads the correction of id.
func (e *renv) correction(id string) tenant.Correction {
	e.Helper()
	c, err := e.p.Correction(e.ctx, id)
	e.ok("Correction", err)
	return c
}

// removal is what a copy answered of one object: removed, at second s.
func removal(id, storage string, t tenant.PurgeTarget, s int) tenant.PurgedObject {
	key, _ := t.Key()
	return tenant.PurgedObject{ID: id, Storage: storage, Key: key, Size: t.Size, SHA256: t.SHA256,
		Disposition: tenant.DispositionRemoved, Now: at(s)}
}

// testCorrectionRecordsOwed guards the records owed to replicas: the
// publish owes the record to each replica of the target's storages, in its
// own transaction; owing it again changes nothing; it is cleared only by a
// record written at the correction's state now; and while it is owed a
// switch to that replica is accepted and drained but not swapped.
func testCorrectionRecordsOwed(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	e.copyAll(20)
	id := e.commitDeleteOf(seriesB, 30)
	if c := e.correction(id); !slices.Equal(c.RecordsOwed, []string{"copy"}) {
		e.Fatalf("a delete published on a primary with a replica owes its record to %v, want [copy]", c.RecordsOwed)
	}
	e.ok("OweRecord", e.p.OweRecord(e.ctx, id, "copy", at(40)))
	owed, err := e.p.RecordsOwed(e.ctx, tenant.RecordOwed{}, 10)
	e.ok("RecordsOwed", err)
	if len(owed) != 1 || owed[0].Correction != id || owed[0].Replica != "copy" || !owed[0].Since.Equal(at(32)) {
		e.Errorf("the records owed are %+v, want the publish's one, since its commit", owed)
	}
	if more, err := e.p.RecordsOwed(e.ctx, owed[0], 10); err != nil || len(more) != 0 {
		e.Errorf("the records owed after the last are %+v, %v", more, err)
	}
	_, err = e.p.RecordsOwed(e.ctx, tenant.RecordOwed{}, 0)
	e.wantKind("RecordsOwed of none", err, fault.Invalid)
	facts, err := e.p.SwitchFacts(e.ctx, "store", "copy")
	e.ok("SwitchFacts", err)
	if facts.RecordsOwed != 1 {
		e.Errorf("the switch's facts count %d records owed, want 1", facts.RecordsOwed)
	}
	_, _, err = e.p.BeginSwitch(e.ctx, tenant.SwitchStart{ID: "sw-1", Primary: "store", Replica: "copy", Token: "tok-1", Now: at(41)},
		e.audit(), nil)
	e.ok("BeginSwitch while a record is owed", err)
	e.copyAll(42)
	_, err = e.p.SwapStorages(e.ctx, tenant.SwitchSwap{ID: "sw-1", Now: at(44)}, e.serverAudit())
	e.wantPrecondition("SwapStorages while a record is owed to the replica", err)
	if !strings.Contains(fault.Message(err), tenant.RefusalRecordsOwed) {
		e.Errorf("the swap refused while a record is owed says %q, want %q", fault.Message(err), tenant.RefusalRecordsOwed)
	}
	e.ok("MarkRecordWritten", e.p.MarkRecordWritten(e.ctx, id, "copy", record.StatePrepared))
	if c := e.correction(id); len(c.RecordsOwed) != 1 {
		e.Errorf("a record written at a state the correction left cleared what was owed: %v", c.RecordsOwed)
	}
	e.ok("MarkRecordWritten", e.p.MarkRecordWritten(e.ctx, id, "copy", record.StateCommitted))
	if c := e.correction(id); len(c.RecordsOwed) != 0 {
		e.Errorf("a record written at the correction's state left it owed to %v", c.RecordsOwed)
	}
	sw, err := e.p.SwapStorages(e.ctx, tenant.SwitchSwap{ID: "sw-1", Now: at(45)}, e.serverAudit())
	e.ok("SwapStorages once the record is written", err)
	if sw.State != tenant.SwitchSwapped {
		e.Errorf("the switch after the record was written is %s", sw.State)
	}
}

// testCorrectionPurgeCopies guards the purge's record per copy: the copies
// a correction reaches; an object one copy removed and another did not
// stays to settle, naming the copy done; and the dispositions count each
// copy apart, a replica naming its primary.
func testCorrectionPurgeCopies(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	e.copyAll(20)
	id := e.commitDeleteOf(seriesB, 30)
	copies, err := e.p.CorrectionCopies(e.ctx, id)
	e.ok("CorrectionCopies", err)
	if len(copies) != 1 || copies[0].Replica != "copy" || copies[0].Primary != "store" || len(copies[0].InStep) != 0 {
		e.Errorf("the copies of a delete of a scan it hid are %+v, want copy of store, in step for nothing", copies)
	}
	e.startPurge(id, 40)
	targets, err := e.p.PurgeTargets(e.ctx, id, 100)
	e.ok("PurgeTargets", err)
	if len(targets) != 4 {
		e.Fatalf("the delete owns %d objects, want its 2 instances, its metadata and its manifest", len(targets))
	}
	err = e.p.SettlePurgeTargets(e.ctx, id, []tenant.PurgeSettled{{Target: targets[0], Outcome: tenant.OutcomePending}})
	e.wantKind("SettlePurgeTargets pending with no removal", err, fault.Invalid)
	e.ok("SettlePurgeTargets pending", e.p.SettlePurgeTargets(e.ctx, id, []tenant.PurgeSettled{{Target: targets[0],
		Outcome: tenant.OutcomePending, Removals: []tenant.PurgedObject{removal(id, "store", targets[0], 41)}}}))
	again, err := e.p.PurgeTargets(e.ctx, id, 100)
	e.ok("PurgeTargets", err)
	if len(again) != 4 || !slices.Equal(again[0].Done, []string{"store"}) || len(again[1].Done) != 0 {
		e.Fatalf("after one copy removed an object the targets are %+v, want all 4, the first done on store", again)
	}
	var settled []tenant.PurgeSettled
	for i, t := range again {
		s := tenant.PurgeSettled{Target: t, Outcome: tenant.OutcomePurged, Removals: []tenant.PurgedObject{removal(id, "copy", t, 42)}}
		if i > 0 {
			s.Removals = append(s.Removals, removal(id, "store", t, 42))
		}
		settled = append(settled, s)
	}
	e.ok("SettlePurgeTargets", e.p.SettlePurgeTargets(e.ctx, id, settled))
	if left, err := e.p.PurgeTargets(e.ctx, id, 100); err != nil || len(left) != 0 {
		e.Errorf("after every copy removed every object the targets left are %+v, %v", left, err)
	}
	c := e.correction(id)
	want := []tenant.StorageDisposition{{Storage: "copy", ReplicaOf: "store", Removed: 4}, {Storage: "store", Removed: 4}}
	if !slices.EqualFunc(c.Dispositions, want, func(a, b tenant.StorageDisposition) bool {
		return a.Storage == b.Storage && a.ReplicaOf == b.ReplicaOf && a.Removed == b.Removed && a.Unknown == b.Unknown &&
			a.SwitchGeneration == b.SwitchGeneration
	}) {
		e.Errorf("the dispositions are %+v, want each copy's 4 removed, the replica naming its primary", c.Dispositions)
	}
	e.ok("NotePurge", e.p.NotePurge(e.ctx, id, tenant.StateReasonStorage))
	if c := e.correction(id); c.StateReason != tenant.StateReasonStorage {
		e.Errorf("a purge noted waiting for a storage reads %q", c.StateReason)
	}
	e.wantKind("NotePurge of another reason", e.p.NotePurge(e.ctx, id, "tired"), fault.Invalid)
	e.ok("NotePurge clearing", e.p.NotePurge(e.ctx, id, ""))
	if c := e.correction(id); c.StateReason != "" {
		e.Errorf("a purge's reason cleared reads %q", c.StateReason)
	}
}

// testRelabelKeepsReplicaInStep guards a relabel's publish on a replica: a
// scan the relabel copied to in step, at the new manifest's digest, stays
// in step there at that digest, so a read may turn to it at once; a scan
// whose replica manifest is not the new one is queued; and the metadata
// digest the publish is told is kept with the new manifest, none for a scan
// it is not told of.
func testRelabelKeepsReplicaInStep(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	e.copyAll(20)
	id, err := tenant.NewCorrectionID(at(30))
	e.ok("NewCorrectionID", err)
	_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindRelabel,
		Target: tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, ToLabel: "ses-2", Reason: "wrong visit", Now: at(30)},
		e.audit(), nil)
	e.ok("PrepareCorrection", err)
	copies, err := e.p.CorrectionCopies(e.ctx, id)
	e.ok("CorrectionCopies", err)
	if len(copies) != 1 || !slices.Equal(copies[0].InStep, []string{seriesA, seriesB}) || !copies[0].HoldsScan(seriesA) {
		e.Fatalf("the copies of a relabel of scans in step are %+v", copies)
	}
	_, err = e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: id, Now: at(31), Grace: time.Hour,
		Manifests:        map[string]tenant.SHA256{seriesA: sum(500), seriesB: sum(501)},
		Sidecars:         map[string]tenant.SHA256{seriesA: sum(600)},
		ReplicaManifests: map[string]map[string]tenant.SHA256{"copy": {seriesA: sum(500), seriesB: sum(999)}}})
	e.ok("CommitCorrection", err)
	// Each metadata file written with a new manifest keeps its digest; one
	// not told keeps none.
	for series, want := range map[string]tenant.SHA256{seriesA: sum(600), seriesB: {}} {
		sc, err := e.p.CatalogScan(e.ctx, label.Path{Subject: "sub-01", Session: "ses-2", Scan: "scan-" + series})
		e.ok("CatalogScan", err)
		if sc.SidecarSHA256 != want {
			e.Errorf("the relabeled scan of %s reads metadata digest %x, want %x", series, sc.SidecarSHA256[:4], want[:4])
		}
	}
	a, err := e.p.ReplicaScan(e.ctx, "copy", seriesA)
	e.ok("ReplicaScan", err)
	if a.State != tenant.ReplicaInStep || a.ManifestSHA256 != sum(500) {
		e.Errorf("the scan the relabel copied in step reads %s at %x, want in step at the new manifest", a.State, a.ManifestSHA256[:4])
	}
	if st := e.rowState("copy", seriesB); st != tenant.ReplicaPending {
		e.Errorf("the scan whose replica manifest is not the new one is %s, want pending", st)
	}
	e.wantReplicaCounts("after the relabel", "copy", 1, 0, 1, 0, 0)
	if r, err := e.p.InStepReplicas(e.ctx, []string{seriesA}); err != nil || !slices.Equal(r[seriesA], []string{"copy"}) {
		e.Errorf("a read of the relabeled scan finds the replicas %v, %v; want copy at once", r, err)
	}
}

// testDetachAbandonsCopies guards a detach with abandon_purges: each
// correction that counted the replica names it among its copies
// abandoned, its objects not removed there are recorded unknown, an object
// the replica had removed stays removed, and what the replica was owed is
// dropped; and a preview lists the replicas the purge would reach, its
// token changing when one is attached.
func testDetachAbandonsCopies(e *renv) {
	e.replicaSetup()
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + seriesB}
	before, err := e.p.PreviewDelete(e.ctx, target, at(5))
	e.ok("PreviewDelete", err)
	e.attach("copy", "store", 10)
	after, err := e.p.PreviewDelete(e.ctx, target, at(5))
	e.ok("PreviewDelete", err)
	if len(before.Replicas) != 0 || !slices.Equal(after.Replicas, []tenant.ReplicaCopy{{Replica: "copy", Primary: "store"}}) ||
		before.Confirm == after.Confirm {
		e.Errorf("the previews before and after an attach list %v and %v with tokens equal %v; want copy listed after, a new token",
			before.Replicas, after.Replicas, before.Confirm == after.Confirm)
	}
	e.copyAll(20)
	id := e.commitDeleteOf(seriesB, 30)
	e.startPurge(id, 40)
	targets, err := e.p.PurgeTargets(e.ctx, id, 100)
	e.ok("PurgeTargets", err)
	e.ok("SettlePurgeTargets pending", e.p.SettlePurgeTargets(e.ctx, id, []tenant.PurgeSettled{{Target: targets[0],
		Outcome: tenant.OutcomePending, Removals: []tenant.PurgedObject{removal(id, "store", targets[0], 41), removal(id, "copy", targets[0], 41)}}}))
	_, abandoned, err := e.p.DetachReplica(e.ctx, tenant.ReplicaDetach{Replica: "copy", Abandon: true, Reason: "the disk is gone",
		Now: at(50)}, e.audit(), nil)
	e.ok("DetachReplica abandoning", err)
	if !slices.Equal(abandoned, []string{id}) {
		e.Errorf("the detach abandoned %v, want [%s]", abandoned, id)
	}
	c := e.correction(id)
	if !slices.Equal(c.CopiesAbandoned, []string{"copy"}) || len(c.RecordsOwed) != 0 {
		e.Errorf("after the detach the correction abandons %v and is owed by %v; want copy abandoned, nothing owed", c.CopiesAbandoned,
			c.RecordsOwed)
	}
	var onCopy tenant.StorageDisposition
	for _, d := range c.Dispositions {
		if d.Storage == "copy" {
			onCopy = d
		}
	}
	if onCopy.Removed != 1 || onCopy.Unknown != 3 || onCopy.ReplicaOf != "store" {
		e.Errorf("the abandoned replica's disposition is %+v, want 1 removed and 3 unknown, naming its primary", onCopy)
	}
	if owed, err := e.p.RecordsOwed(e.ctx, tenant.RecordOwed{}, 10); err != nil || len(owed) != 0 {
		e.Errorf("the records owed after the detach are %+v, %v", owed, err)
	}
}

// memSnapshot is a snapshot held in memory: a Sink that keeps each table's
// rows, and a Source that gives them back in order.
type memSnapshot struct {
	tables []snapshot.Table
	rows   [][][]any
	seqs   map[string]int64
	at     int
	row    int
}

// Table implements snapshot.Sink.
func (m *memSnapshot) Table(t snapshot.Table) error {
	m.tables, m.rows = append(m.tables, t), append(m.rows, nil)
	return nil
}

// Row implements snapshot.Sink.
func (m *memSnapshot) Row(values []any) error {
	m.rows[len(m.rows)-1] = append(m.rows[len(m.rows)-1], slices.Clone(values))
	return nil
}

// Sequences implements snapshot.Sink; the Source's is sequences'.
func (m *memSnapshot) Sequences(seqs map[string]int64) error {
	m.seqs = seqs
	return nil
}

// NextTable implements snapshot.Source.
func (m *memSnapshot) NextTable() (snapshot.Table, bool, error) {
	if m.at >= len(m.tables) {
		return snapshot.Table{}, false, nil
	}
	m.at, m.row = m.at+1, 0
	return m.tables[m.at-1], true, nil
}

// NextRow implements snapshot.Source.
func (m *memSnapshot) NextRow(values []any) (bool, error) {
	rows := m.rows[m.at-1]
	if m.row >= len(rows) {
		return false, nil
	}
	copy(values, rows[m.row])
	m.row++
	return true, nil
}

// source gives the snapshot back from its start.
func (m *memSnapshot) source() *memSnapshot { m.at, m.row = 0, 0; return m }

// sequences is the Source's Sequences.
type sequences struct{ *memSnapshot }

// Sequences implements snapshot.Source.
func (s sequences) Sequences() (map[string]int64, error) { return s.seqs, nil }

// testCorrectionCopiesSnapshot guards what a backup carries of a
// correction's copies: a snapshot loaded into another database answers the
// same records owed, copies abandoned and dispositions per copy, so a
// restore goes on owing what was owed and claims no more than was done.
func testCorrectionCopiesSnapshot(e *renv) {
	if e.openEv == nil {
		e.Skip("the driver opens no second database")
	}
	e.replicaSetup()
	e.addStorage("copy2")
	e.attach("copy", "store", 10)
	e.attach("copy2", "store", 11)
	e.copyAll(20)
	id := e.commitDeleteOf(seriesB, 30)
	e.startPurge(id, 40)
	targets, err := e.p.PurgeTargets(e.ctx, id, 100)
	e.ok("PurgeTargets", err)
	e.ok("SettlePurgeTargets", e.p.SettlePurgeTargets(e.ctx, id, []tenant.PurgeSettled{{Target: targets[0], Outcome: tenant.OutcomePurged,
		Removals: []tenant.PurgedObject{removal(id, "store", targets[0], 41), removal(id, "copy", targets[0], 41)}}}))
	_, _, err = e.p.DetachReplica(e.ctx, tenant.ReplicaDetach{Replica: "copy", Abandon: true, Reason: "the disk is gone", Now: at(50)},
		e.audit(), nil)
	e.ok("DetachReplica", err)
	want := e.correction(id)
	if !slices.Equal(want.RecordsOwed, []string{"copy2"}) || len(want.Dispositions) != 2 {
		e.Fatalf("before the snapshot the correction is owed by %v with dispositions %+v", want.RecordsOwed, want.Dispositions)
	}
	src, ok := e.p.(snapshot.Database)
	if !ok {
		e.Fatal("the store does not implement snapshot.Database")
	}
	mem := &memSnapshot{}
	point, err := src.Snapshot(e.ctx, mem)
	e.ok("Snapshot", err)
	other := e.openEv(e.T, SuiteTenant)
	dst, ok := other.(snapshot.Database)
	if !ok {
		e.Fatal("the store does not implement snapshot.Database")
	}
	e.ok("Load", dst.Load(e.ctx, point.Version, sequences{mem.source()}))
	got, err := other.Correction(e.ctx, id)
	e.ok("Correction after the load", err)
	if !slices.Equal(got.RecordsOwed, want.RecordsOwed) || !slices.Equal(got.CopiesAbandoned, want.CopiesAbandoned) ||
		!slices.Equal(got.Dispositions, want.Dispositions) {
		e.Errorf("after a snapshot's load the correction is owed by %v, abandons %v, disposes %+v; want %v, %v, %+v", got.RecordsOwed,
			got.CopiesAbandoned, got.Dispositions, want.RecordsOwed, want.CopiesAbandoned, want.Dispositions)
	}
	left, err := other.PurgeTargets(e.ctx, id, 100)
	e.ok("PurgeTargets after the load", err)
	if len(left) != len(targets)-1 {
		e.Errorf("after a snapshot's load %d objects are left to purge, want %d", len(left), len(targets)-1)
	}
}
