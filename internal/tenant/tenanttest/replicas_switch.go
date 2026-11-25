package tenanttest

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// serverAudit returns a new audit entry of a change the server makes by
// itself: no key.
func (e *renv) serverAudit() master.AuditEntry {
	a := e.audit()
	a.KeyID = ""
	return a
}

// findingOf returns the finding of id, cleared or not, failing the test when
// the tenant has none.
func (e *renv) findingOf(id string) tenant.ScrubFinding {
	e.Helper()
	all, _, err := e.p.ListScrubFindings(e.ctx, tenant.ScrubFindingFilter{IncludeCleared: true}, master.Page{Limit: master.MaxPageLimit})
	e.ok("ListScrubFindings", err)
	for _, f := range all {
		if f.ID == id {
			return f
		}
	}
	e.Fatalf("there is no finding %s", id)
	return tenant.ScrubFinding{}
}

// testRepairs guards the record of a repair: written before the good bytes
// are put, with the object, the finding, the replica and the damaged
// object's size and checksum, once whatever number of writes; ended by
// clearing the finding under the repair's job and lifting the instance's
// mark, with an audit entry, once; and refused for a storage that is a
// replica, a source that is not a replica of the storage, and a finding the
// tenant does not have.
func testRepairs(e *renv) {
	e.scrubSetup()
	e.attach("cold", "store", 50)
	e.createScrub("sj-1", "store", at(100))
	e.listScrub(at(100))
	w, scan, ins := e.scrubScan(at(110))
	f, _ := e.record(w, finding("sj-1", tenant.FindingDamaged, scan.Path, ins[0]), at(120))
	if got := fmt.Sprint(e.marked(scan.Path, ins[:1])); got != "[true]" {
		e.Fatalf("the damaged instance reads marked %s", got)
	}
	found := ins[0].Size + 1
	rec := tenant.Repair{ID: tenant.RepairID("rj-1", f.ID), Finding: f.ID, Storage: "store", From: "cold", Job: "rj-1",
		ExpectedSize: ins[0].Size, ExpectedSHA256: ins[0].ArchivedSHA256, FoundSize: &found, FoundSHA256: sum(9999)}
	got, made, err := e.p.RecordRepair(e.ctx, tenant.RepairRecord{Repair: rec, Now: at(130)})
	e.ok("RecordRepair", err)
	if !made || got.Key != ins[0].Key || got.SOPInstanceUID != ins[0].SOPInstanceUID || got.Revision != 1 || got.From != "cold" ||
		*got.FoundSize != found || got.FoundSHA256 != sum(9999) || !got.Recorded.Equal(at(130)) || got.Done() {
		e.Errorf("a repair recorded reads %+v (made %v)", got, made)
	}
	if _, made, err := e.p.RecordRepair(e.ctx, tenant.RepairRecord{Repair: rec, Now: at(131)}); err != nil || made {
		e.Errorf("a repair recorded again: made %v, %v", made, err)
	}
	wrong := rec
	wrong.ID, wrong.From = tenant.RepairID("rj-2", f.ID), "store"
	_, _, err = e.p.RecordRepair(e.ctx, tenant.RepairRecord{Repair: wrong, Now: at(132)})
	e.wantPrecondition("RecordRepair from a storage that is not a replica of it", err)
	wrong.Storage, wrong.From = "cold", "cold"
	_, _, err = e.p.RecordRepair(e.ctx, tenant.RepairRecord{Repair: wrong, Now: at(132)})
	e.wantPrecondition("RecordRepair of a replica", err)
	wrong = rec
	wrong.ID, wrong.Finding = tenant.RepairID("rj-3", "sf-none"), "sf-none"
	_, _, err = e.p.RecordRepair(e.ctx, tenant.RepairRecord{Repair: wrong, Now: at(132)})
	e.wantKind("RecordRepair of a finding the tenant does not have", err, fault.NotFound)
	start := e.now()
	entry := e.serverAudit()
	done, err := e.p.CompleteRepair(e.ctx, nil, rec.ID, at(140), entry)
	e.ok("CompleteRepair", err)
	if !done.Repaired.Equal(at(140)) {
		e.Errorf("the repair ended reads %+v", done)
	}
	if !e.auditIDs()[entry.ID] {
		e.Error("the end of a repair wrote no audit entry")
	}
	if c := e.findingOf(f.ID); c.Holds() || c.ClearJob != "rj-1" || !c.Cleared.Equal(at(140)) {
		e.Errorf("the finding a repair mended reads %+v", c)
	}
	if got := fmt.Sprint(e.marked(scan.Path, ins[:1])); got != "[false]" {
		e.Errorf("the repaired instance reads marked %s", got)
	}
	if events, _ := e.readAll(start, 100, tenant.EventScrubFinding); len(events) != 1 ||
		!events[0].Payload.(*tenant.ScrubFindingChanged).Cleared {
		e.Errorf("the end of a repair wrote the events %+v, want its finding's clear", events)
	}
	again, err := e.p.CompleteRepair(e.ctx, nil, rec.ID, at(150), e.serverAudit())
	e.ok("CompleteRepair again", err)
	if !again.Repaired.Equal(at(140)) {
		e.Errorf("a repair ended again reads %+v", again)
	}
	list, err := e.p.FindingRepairs(e.ctx, f.ID)
	e.ok("FindingRepairs", err)
	if len(list) != 1 || list[0].ID != rec.ID {
		e.Errorf("the finding's repairs are %+v", list)
	}
	_, err = e.p.CompleteRepair(e.ctx, nil, "rp-none", at(150), e.serverAudit())
	e.wantKind("CompleteRepair of a repair the tenant does not have", err, fault.NotFound)
}

// testSwitchSwaps guards a switch: accepted once per token, a step
// recorded, one running at a time, the replica not detached meanwhile; then
// the one transaction that swaps the two storages' places (the primary's
// name on the replica's former place, the replica's on the primary's),
// their owner records' generations and the primary's open findings with
// them, the marks those findings set lifted, both generations raised, every
// scan in step on the replica's new place but the one a moved finding names,
// which is damaged there, and its event; and the switch finished once.
func testSwitchSwaps(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	e.copyAll(20)
	rs, ok := e.p.(tenant.Restoring)
	if !ok {
		e.Fatal("the store does not implement tenant.Restoring")
	}
	e.ok("SetStorageOwner", rs.SetStorageOwner(e.ctx, "store", 3, at(21)))
	e.ok("SetStorageOwner", rs.SetStorageOwner(e.ctx, "copy", 5, at(22)))
	// The scrub after the scans' commits, which are at their step time.
	e.createScrub("sj-1", "store", at(200))
	e.listScrub(at(200))
	w, scan, ins := e.scrubScan(at(210))
	f, _ := e.record(w, finding("sj-1", tenant.FindingDamaged, scan.Path, ins[0]), at(220))
	facts, err := e.p.SwitchFacts(e.ctx, "store", "copy")
	e.ok("SwitchFacts", err)
	if facts.ScansNotInStep != 0 || facts.ReplicaFindings != 0 || facts.Running != nil || !slices.Equal(facts.Receivers, []string{"recv"}) ||
		!facts.BackfillDone.Equal(at(21)) || len(tenant.SwitchBlockers(facts)) != 0 {
		e.Errorf("the facts of a switch that could start are %+v, blockers %v", facts, tenant.SwitchBlockers(facts))
	}
	start := e.now()
	sw, made, err := e.p.BeginSwitch(e.ctx, tenant.SwitchStart{ID: "sw-1", Primary: "store", Replica: "copy", Token: "tok-1", Now: at(30)},
		e.audit(), nil)
	e.ok("BeginSwitch", err)
	if !made || sw.State != tenant.SwitchAccepted || sw.Generation != 0 || sw.KeyID == "" || !sw.Accepted.Equal(at(30)) {
		e.Errorf("a switch begun reads %+v (made %v)", sw, made)
	}
	if again, made, err := e.p.BeginSwitch(e.ctx, tenant.SwitchStart{ID: "sw-x", Primary: "store", Replica: "copy", Token: "tok-1",
		Now: at(31)}, e.audit(), nil); err != nil || made || again.ID != "sw-1" {
		e.Errorf("a switch begun again with its token = %+v, %v, %v; want sw-1 answered", again, made, err)
	}
	_, _, err = e.p.BeginSwitch(e.ctx, tenant.SwitchStart{ID: "sw-2", Primary: "store", Replica: "copy", Token: "tok-2", Now: at(31)},
		e.audit(), nil)
	e.wantPrecondition("BeginSwitch while another runs", err)
	stepped, err := e.p.SwitchStep(e.ctx, "sw-1", tenant.SwitchDraining, at(32))
	e.ok("SwitchStep", err)
	if stepped.Step != tenant.SwitchDraining {
		e.Errorf("the switch's step is %q", stepped.Step)
	}
	_, _, err = e.p.DetachReplica(e.ctx, tenant.ReplicaDetach{Replica: "copy", Now: at(33)}, e.audit(), nil)
	e.wantPrecondition("DetachReplica while a switch of its primary runs", err)
	sw, err = e.p.SwapStorages(e.ctx, tenant.SwitchSwap{ID: "sw-1", ScansDrained: 2, AssociationsRefused: 3, Pause: 2 * time.Second,
		Now: at(40)}, e.serverAudit())
	e.ok("SwapStorages", err)
	if sw.State != tenant.SwitchSwapped || !sw.Swapped.Equal(at(40)) || sw.ScansDrained != 2 || sw.AssociationsRefused != 3 ||
		sw.Pause != 2*time.Second {
		e.Errorf("a switch swapped reads %+v", sw)
	}
	primary, err := e.p.Storage(e.ctx, "store")
	e.ok("Storage", err)
	replica, err := e.p.Storage(e.ctx, "copy")
	e.ok("Storage", err)
	if primary.Directory.Path != "copy" || replica.Directory.Path != "store" || primary.SwitchGeneration != 1 ||
		replica.SwitchGeneration != 1 || replica.ReplicaOf != "store" || primary.ReplicaOf != "" {
		e.Errorf("after the swap the primary is %+v and the replica %+v", primary, replica)
	}
	owners, err := rs.StorageOwners(e.ctx)
	e.ok("StorageOwners", err)
	if len(owners) != 2 || owners[0].Storage != "copy" || owners[0].Generation != 3 || owners[1].Storage != "store" ||
		owners[1].Generation != 5 {
		e.Errorf("the owner generations after the swap are %+v; want each with its place", owners)
	}
	if moved := e.findingOf(f.ID); moved.Storage != "copy" || !moved.Holds() {
		e.Errorf("the primary's finding after the swap reads %+v; want it on the replica's name", moved)
	}
	if got := fmt.Sprint(e.marked(scan.Path, ins[:1])); got != "[false]" {
		e.Errorf("the mark of a finding moved with its place still reads %s", got)
	}
	damaged := scan.Path.Scan[len("scan-"):]
	if st := e.rowState("copy", damaged); st != tenant.ReplicaDamaged {
		e.Errorf("the scan a moved finding names is %s on the replica, want damaged", st)
	}
	e.wantReplicaCounts("after the swap", "copy", 0, 0, 1, 1, 1)
	events, _ := e.readAll(start, 100, tenant.EventStorageSwitched)
	if len(events) != 1 {
		e.Fatalf("the swap wrote %d switch events, want 1", len(events))
	}
	if p := events[0].Payload.(*tenant.StorageSwitched); p.Refused || p.SwitchGeneration != 1 || p.ScansDrained != 2 ||
		p.PauseMicros != 2e6 || p.Primary != "store" || p.Replica != "copy" {
		e.Errorf("the switch's event is %+v", p)
	}
	if again, err := e.p.SwapStorages(e.ctx, tenant.SwitchSwap{ID: "sw-1", Now: at(41)}, e.serverAudit()); err != nil ||
		again.State != tenant.SwitchSwapped || !again.Swapped.Equal(at(40)) {
		e.Errorf("a swap sent again = %+v, %v", again, err)
	}
	_, err = e.p.RefuseSwitch(e.ctx, "sw-1", tenant.RefusalRestarted, at(42), e.serverAudit())
	e.wantPrecondition("RefuseSwitch of a switch swapped", err)
	if left, err := e.p.UnfinishedSwitches(e.ctx); err != nil || len(left) != 1 || left[0].ID != "sw-1" {
		e.Errorf("the switches unfinished after the swap = %+v, %v", left, err)
	}
	done, err := e.p.FinishSwitch(e.ctx, "sw-1", at(43))
	e.ok("FinishSwitch", err)
	if done.State != tenant.SwitchDone || !done.Ended.Equal(at(43)) {
		e.Errorf("a switch finished reads %+v", done)
	}
	if left, err := e.p.UnfinishedSwitches(e.ctx); err != nil || len(left) != 0 {
		e.Errorf("the switches unfinished after the end = %+v, %v", left, err)
	}
	if last, found, err := e.p.LastSwitch(e.ctx, "copy"); err != nil || !found || last.ID != "sw-1" {
		e.Errorf("the replica's last switch = %+v, %v, %v", last, found, err)
	}
	if _, found, err := e.p.LastSwitch(e.ctx, "recv-none"); err != nil || found {
		e.Errorf("a storage never switched has a last switch: %v, %v", found, err)
	}
}

// installationKeys returns the keys of a new installation key file made in
// a directory of the test's own.
func (e *renv) installationKeys() *secret.Keys {
	e.Helper()
	dir := e.TempDir()
	// LoadKeyFile refuses a key file in a directory others may change.
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit to be entered.
		e.Fatal(err)
	}
	path := filepath.Join(dir, "nopacs.key")
	e.ok("CreateKeyFile", secret.CreateKeyFile(path))
	keys, err := secret.LoadKeyFile(path)
	e.ok("LoadKeyFile", err)
	return keys
}

// opens fails the test unless the sealed secret the store keeps for the S3
// storage named opens, under that name, with keys, to want.
func (e *renv) opens(keys *secret.Keys, storage, want string) {
	e.Helper()
	sealed, err := e.p.StorageSecret(e.ctx, storage)
	e.ok("StorageSecret", err)
	plain, err := keys.Open(place.SecretContext(SuiteTenant, storage), sealed)
	if err != nil || string(plain) != want {
		e.Errorf("the secret of %s opens to %q, %v; want %q", storage, plain, err, want)
	}
}

// testSwitchSecrets guards the swap of two S3 storages: a seal is bound to
// its storage's name, so a swap sent without each S3 place's secret sealed
// again for the name it comes to is refused, changing nothing, and one sent
// with them leaves each name's secret opening, under that name, to the
// secret of the place it now names.
func testSwitchSecrets(e *renv) {
	keys := e.installationKeys()
	add := func(name, prefix, replicaOf, plain string) tenant.Storage {
		s := s3Storage(name, prefix)
		s.ReplicaOf = replicaOf
		sealed, err := place.SealSecret(keys, SuiteTenant, name, plain)
		e.ok("SealSecret", err)
		s.S3.SealedSecret = sealed
		got, err := e.p.AddStorage(e.ctx, s, at(1), e.audit(), nil)
		e.ok("AddStorage", err)
		return got
	}
	primary := add("bucket-a", "a/", "", "secret-of-a")
	replica := add("bucket-b", "b/", "bucket-a", "secret-of-b")
	_, _, err := e.p.BeginSwitch(e.ctx, tenant.SwitchStart{ID: "sw-1", Primary: "bucket-a", Replica: "bucket-b", Token: "tok-1",
		Now: at(10)}, e.audit(), nil)
	e.ok("BeginSwitch", err)
	_, err = e.p.SwapStorages(e.ctx, tenant.SwitchSwap{ID: "sw-1", Now: at(20)}, e.serverAudit())
	e.wantPrecondition("SwapStorages of two S3 storages without their secrets sealed for their new names", err)
	forPrimary, forReplica, err := place.SwapSecrets(e.ctx, keys, place.TenantOf(SuiteTenant, "lab", e.p), primary, replica)
	e.ok("SwapSecrets", err)
	_, err = e.p.SwapStorages(e.ctx, tenant.SwitchSwap{ID: "sw-1", PrimarySecret: forPrimary, Now: at(20)}, e.serverAudit())
	e.wantPrecondition("SwapStorages without the primary's secret sealed for the replica's name", err)
	if sw, err := e.p.Switch(e.ctx, "sw-1"); err != nil || sw.State != tenant.SwitchAccepted {
		e.Errorf("a switch whose swap was refused reads %+v, %v", sw, err)
	}
	if s, _ := e.p.Storage(e.ctx, "bucket-a"); s.S3.Prefix != "a/" || s.SwitchGeneration != 0 {
		e.Errorf("a refused swap changed the primary: %+v", s)
	}
	e.opens(keys, "bucket-a", "secret-of-a")
	e.opens(keys, "bucket-b", "secret-of-b")
	sw, err := e.p.SwapStorages(e.ctx, tenant.SwitchSwap{ID: "sw-1", PrimarySecret: forPrimary, ReplicaSecret: forReplica, Now: at(21)},
		e.serverAudit())
	e.ok("SwapStorages", err)
	if sw.State != tenant.SwitchSwapped {
		e.Errorf("the switch after its swap reads %+v", sw)
	}
	if a, _ := e.p.Storage(e.ctx, "bucket-a"); a.S3.Prefix != "b/" || !a.S3.SecretSet {
		e.Errorf("after the swap the primary is %+v", a)
	}
	e.opens(keys, "bucket-a", "secret-of-b")
	e.opens(keys, "bucket-b", "secret-of-a")
}

// testSwitchHoldsChanges guards that a switch, once begun, holds what
// would write to its primary's place or read the catalog from it until it
// ends: a correction of a scan on the primary and a rebuild are refused
// while it runs, and taken again once it is refused.
func testSwitchHoldsChanges(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	_, _, err := e.p.BeginSwitch(e.ctx, tenant.SwitchStart{ID: "sw-1", Primary: "store", Replica: "copy", Token: "tok-1", Now: at(20)},
		e.audit(), nil)
	e.ok("BeginSwitch", err)
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + seriesB}
	relabel := func(s int) error {
		id, err := tenant.NewCorrectionID(at(s))
		e.ok("NewCorrectionID", err)
		_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindRelabel, Target: target,
			ToLabel: "scan-new", Reason: "wrong label at the scanner", Now: at(s)}, e.audit(), nil)
		return err
	}
	e.wantPrecondition("PrepareCorrection of a scan on the primary while a switch runs", relabel(21))
	_, err = e.p.CreateJob(e.ctx, tenant.NewJob{ID: "rb-1", Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare,
		Scratch: e.scratch}, Created: at(22)}, e.audit(), nil)
	e.wantPrecondition("CreateJob of a rebuild while a switch runs", err)
	if facts, err := e.p.SwitchFacts(e.ctx, "store", "copy"); err != nil || facts.CorrectionPrepared || facts.RebuildRunning {
		e.Errorf("the refused changes were recorded: %+v, %v", facts, err)
	}
	_, err = e.p.RefuseSwitch(e.ctx, "sw-1", tenant.RefusalNotInStep, at(30), e.serverAudit())
	e.ok("RefuseSwitch", err)
	e.ok("PrepareCorrection once the switch is refused", relabel(31))
}

// testRepairAcrossSwap guards that a repair recorded before a switch's swap
// moved its finding to the other name does not clear the finding there: the
// end of the repair is refused, the finding still holds.
func testRepairAcrossSwap(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	e.copyAll(20)
	e.createScrub("sj-1", "store", at(200))
	e.listScrub(at(200))
	w, scan, ins := e.scrubScan(at(210))
	f, _ := e.record(w, finding("sj-1", tenant.FindingDamaged, scan.Path, ins[0]), at(220))
	rec := tenant.Repair{ID: tenant.RepairID("rj-1", f.ID), Finding: f.ID, Storage: "store", From: "copy", Job: "rj-1",
		ExpectedSize: ins[0].Size, ExpectedSHA256: ins[0].ArchivedSHA256}
	_, _, err := e.p.RecordRepair(e.ctx, tenant.RepairRecord{Repair: rec, Now: at(230)})
	e.ok("RecordRepair", err)
	_, _, err = e.p.BeginSwitch(e.ctx, tenant.SwitchStart{ID: "sw-1", Primary: "store", Replica: "copy", Token: "tok-1", Now: at(240)},
		e.audit(), nil)
	e.ok("BeginSwitch", err)
	_, err = e.p.SwapStorages(e.ctx, tenant.SwitchSwap{ID: "sw-1", Now: at(250)}, e.serverAudit())
	e.ok("SwapStorages", err)
	_, err = e.p.CompleteRepair(e.ctx, nil, rec.ID, at(260), e.serverAudit())
	e.wantPrecondition("CompleteRepair of a finding a swap moved to the replica's name", err)
	if moved := e.findingOf(f.ID); moved.Storage != "copy" || !moved.Holds() {
		e.Errorf("the finding a swap moved reads %+v after the repair's end; want it holding on copy", moved)
	}
	if r, err := e.p.Repair(e.ctx, rec.ID); err != nil || r.Done() {
		e.Errorf("the repair refused reads %+v, %v; want it not ended", r, err)
	}
}

// testSwitchRefusals guards what stops a switch: the blockers a preview
// answers, a replica of the archive tier refused at the start, a replica not
// in step refused at the swap (leaving the switch accepted), and the switch
// then refused with its phrase and event, once, after which neither a swap
// nor a finish takes it and a new switch may begin.
func testSwitchRefusals(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	facts, err := e.p.SwitchFacts(e.ctx, "store", "copy")
	e.ok("SwitchFacts", err)
	if b := tenant.SwitchBlockers(facts); !slices.Equal(b, []string{tenant.BlockerNotInStep(2)}) {
		e.Errorf("the blockers of a replica not in step are %v", b)
	}
	_, err = e.p.SwitchFacts(e.ctx, "store", "gone")
	e.wantKind("SwitchFacts of a storage the tenant does not have", err, fault.NotFound)
	cold := s3Storage("cold", "cold/")
	cold.ReplicaOf, cold.Tier = "store", tenant.TierArchive
	_, err = e.p.AddStorage(e.ctx, cold, at(11), e.audit(), nil)
	e.ok("AddStorage", err)
	_, _, err = e.p.BeginSwitch(e.ctx, tenant.SwitchStart{ID: "sw-0", Primary: "store", Replica: "cold", Token: "tok-0", Now: at(12)},
		e.audit(), nil)
	e.wantPrecondition("BeginSwitch to an archive-tier replica", err)
	_, _, err = e.p.BeginSwitch(e.ctx, tenant.SwitchStart{ID: "sw-0", Primary: "copy", Replica: "store", Token: "tok-0", Now: at(12)},
		e.audit(), nil)
	e.wantPrecondition("BeginSwitch the wrong way round", err)
	_, _, err = e.p.BeginSwitch(e.ctx, tenant.SwitchStart{ID: "sw-0", Primary: "store", Replica: "copy", Token: "tok-0",
		SkipVerification: true, Now: at(12)}, e.audit(), nil)
	e.wantKind("BeginSwitch skipping the checks with no reason", err, fault.Invalid)
	_, _, err = e.p.BeginSwitch(e.ctx, tenant.SwitchStart{ID: "sw-1", Primary: "store", Replica: "copy", Token: "tok-1",
		SkipVerification: true, SkipReason: "checked by hand", Now: at(20)}, e.audit(), nil)
	e.ok("BeginSwitch", err)
	_, err = e.p.SwapStorages(e.ctx, tenant.SwitchSwap{ID: "sw-1", Now: at(30)}, e.serverAudit())
	e.wantPrecondition("SwapStorages with the replica not in step", err)
	if sw, err := e.p.Switch(e.ctx, "sw-1"); err != nil || sw.State != tenant.SwitchAccepted || !sw.SkipVerification ||
		sw.SkipReason != "checked by hand" {
		e.Errorf("a switch whose swap was refused reads %+v, %v", sw, err)
	}
	if primary, _ := e.p.Storage(e.ctx, "store"); primary.Directory.Path != "store" || primary.SwitchGeneration != 0 {
		e.Errorf("a refused swap changed the primary: %+v", primary)
	}
	_, err = e.p.RefuseSwitch(e.ctx, "sw-1", "because", at(31), e.serverAudit())
	e.wantKind("RefuseSwitch with a phrase not the server's", err, fault.Invalid)
	start := e.now()
	sw, err := e.p.RefuseSwitch(e.ctx, "sw-1", tenant.RefusalNotInStep, at(32), e.serverAudit())
	e.ok("RefuseSwitch", err)
	if sw.State != tenant.SwitchRefused || sw.Refusal != tenant.RefusalNotInStep || !sw.Ended.Equal(at(32)) {
		e.Errorf("a switch refused reads %+v", sw)
	}
	if events, _ := e.readAll(start, 100, tenant.EventStorageSwitched); len(events) != 1 ||
		!events[0].Payload.(*tenant.StorageSwitched).Refused || events[0].Payload.(*tenant.StorageSwitched).SwitchGeneration != 0 {
		e.Errorf("the refusal wrote the events %+v", events)
	}
	if again, err := e.p.RefuseSwitch(e.ctx, "sw-1", tenant.RefusalRestarted, at(33), e.serverAudit()); err != nil ||
		again.Refusal != tenant.RefusalNotInStep {
		e.Errorf("a refusal sent again = %+v, %v", again, err)
	}
	_, err = e.p.SwapStorages(e.ctx, tenant.SwitchSwap{ID: "sw-1", Now: at(34)}, e.serverAudit())
	e.wantPrecondition("SwapStorages of a switch refused", err)
	_, err = e.p.FinishSwitch(e.ctx, "sw-1", at(34))
	e.wantPrecondition("FinishSwitch of a switch refused", err)
	_, made, err := e.p.BeginSwitch(e.ctx, tenant.SwitchStart{ID: "sw-2", Primary: "store", Replica: "copy", Token: "tok-2", Now: at(40)},
		e.audit(), nil)
	if err != nil || !made {
		e.Errorf("a new switch after a refusal = %v, %v", made, err)
	}
	_, err = e.p.Switch(e.ctx, "sw-none")
	e.wantKind("Switch of a switch the tenant does not have", err, fault.NotFound)
}
