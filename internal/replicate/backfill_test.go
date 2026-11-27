package replicate

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// replicateJob returns the replica's replicate job still running, failing
// the test when there is none.
func (f *fixture) replicateJob() tenant.Job {
	f.t.Helper()
	j, open, err := f.p.ReplicateJob(f.ctx, "copy")
	if err != nil || !open {
		f.t.Fatalf("the replica's replicate job: open %v, %v", open, err)
	}
	return j
}

// TestBackfillCopiesWhatThePrimaryHeldAndNothingPurging guards the copy an
// attach queues and the job that follows it: the job counts the scans the
// primary held when the replica was attached, a scan archived after is
// copied but not counted, and the replica receives the objects of the
// scans that predate it and of that arrival, but never the instance a
// purging delete withdraws nor the manifest it is replacing (the purge
// took its copies when it began, so an object put there now would outlive
// it). The job stays running while that scan is not in step, its scans
// done counting the one copied whole.
func TestBackfillCopiesWhatThePrimaryHeldAndNothingPurging(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-a", synthetic(1, 1, 2, 600))
	b := synthetic(2, 1, 3, 700)
	sb := f.archive(2, "scan-b", b)
	f.attach()
	j := f.replicateJob()
	if j.Replicate == nil || !j.Replicate.Scheduled || j.Replicate.Primary != "main" || j.Counts.Scans != 2 || j.State != tenant.JobRunning {
		t.Fatalf("the attach's job is %+v, want the server's running job of the 2 scans the primary held", j)
	}
	// Before any copy, an instance of scan-b is deleted and its purge
	// begins.
	corr, ok := f.p.(tenant.Corrections)
	if !ok {
		t.Fatal("the fixture's store does not implement tenant.Corrections")
	}
	target := tenant.CorrectionTarget{Subject: sb.path.Subject, Session: sb.path.Session, Scan: sb.path.Scan, Instance: b[1].uid}
	pv, err := corr.PreviewDelete(f.ctx, target, f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	id, err := tenant.NewCorrectionID(f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := corr.PrepareCorrection(f.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: pv.Confirm, Now: f.clock.Now()}, f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := corr.CommitCorrection(f.ctx, tenant.CorrectionCommit{ID: id, Now: f.clock.Now(), Grace: time.Hour}); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(time.Second)
	a := f.audit()
	if _, err := corr.StartPurge(f.ctx, id, f.clock.Now(), &a, nil); err != nil {
		t.Fatal(err)
	}

	// An arrival after the attach: copied as it comes, not the backfill's.
	f.archive(3, "scan-c", synthetic(3, 1, 1, 500))

	w := f.worker(f.config())
	f.clock.Add(time.Second)
	if p := f.runOnce(w); p.InStep != 2 {
		t.Fatalf("the first pass is %+v, want scan-a and scan-c in step", p)
	}
	f.clock.Add(time.Second)
	f.runOnce(w)
	// scan-a and scan-c whole; of scan-b, its instances the delete does
	// not hide, and neither the deleted instance nor the manifest the purge
	// is replacing.
	var want []string
	for _, name := range []string{"scan-a", "scan-b", "scan-c"} {
		s := f.scans[name]
		for _, c := range s.confirmed {
			if c.SOPInstanceUID != b[1].uid {
				want = append(want, c.Key)
			}
		}
		if name != "scan-b" {
			want = append(want, mustKey(layout.ManifestKey(s.path)), mustKey(layout.MetadataKey(s.path)))
		}
	}
	got := f.replica.putKeys()
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the replica was given %q, want exactly %q", got, want)
	}
	for _, k := range got {
		if strings.Contains(k, b[1].uid) || strings.HasPrefix(k, "meta/") && strings.Contains(k, "/"+sb.path.Scan+"/") {
			t.Errorf("the replica was given %s, which the purge withdraws or replaces", k)
		}
	}
	j = f.replicateJob()
	if j.Counts.Scans != 2 || j.Counts.ScansDone != 1 || j.State != tenant.JobRunning {
		t.Errorf("the job while the purging scan is owed is %+v, want running with 1 of 2 scans done", j)
	}
}

// TestBackfillOutlivesAnEarlierClaim guards a backfill made while a copy
// claimed before it runs, a claim that is not the backfill's: that copy did
// not verify what the replica already holds as a backfill's does, so its
// end must not count for the backfill. It ends not in step, the row is
// claimed again as the backfill's, and the job ends done only once that
// claim's copy ends.
func TestBackfillOutlivesAnEarlierClaim(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-a", synthetic(1, 1, 2, 600))
	f.attach()
	w := f.worker(f.config())
	if p := f.runOnce(w); p.InStep != 1 {
		t.Fatalf("the attach's pass is %+v, want the scan in step", p)
	}
	// An arrival makes the scan pending again; a copier claims it, not as a
	// backfill's.
	f.clock.Add(time.Second)
	f.archive(1, "scan-a", synthetic(1, 3, 1, 600))
	early, err := f.p.ClaimReplicaScans(f.ctx, "early", f.clock.Now(), time.Minute, 10)
	if err != nil || len(early) != 1 || early[0].Backfill {
		t.Fatalf("the early claim is %+v, %v; want one claim, not a backfill's", early, err)
	}
	f.clock.Add(time.Second)
	j, made, err := f.p.BackfillReplica(f.ctx, tenant.ReplicaBackfill{Replica: "copy", JobID: f.id("job"), Now: f.clock.Now()}, f.audit(), nil)
	if err != nil || !made || j.Counts.Scans != 1 {
		t.Fatalf("the backfill answered %+v (made %v), %v; want a job of the one scan", j, made, err)
	}
	f.clock.Add(time.Second)
	state, err := f.p.EndReplicaScan(f.ctx, early[0].Fence, early[0].ManifestSHA256, f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if state == tenant.ReplicaInStep {
		t.Errorf("the claim made before the backfill ended in step")
	}
	if after, err := f.p.Job(f.ctx, j.ID); err != nil || after.State != tenant.JobRunning {
		t.Errorf("after the early claim's end the backfill's job is %+v, %v; want running", after, err)
	}
	f.clock.Add(time.Second)
	again, err := f.p.ClaimReplicaScans(f.ctx, "later", f.clock.Now(), time.Minute, 10)
	if err != nil || len(again) != 1 || !again[0].Backfill {
		t.Fatalf("the claim after it is %+v, %v; want the row claimed again as the backfill's", again, err)
	}
	f.clock.Add(time.Second)
	if state, err := f.p.EndReplicaScan(f.ctx, again[0].Fence, again[0].ManifestSHA256, f.clock.Now()); err != nil || state != tenant.ReplicaInStep {
		t.Fatalf("the backfill's claim ended %s, %v; want in step", state, err)
	}
	if done, err := f.p.Job(f.ctx, j.ID); err != nil || done.State != tenant.JobDone || done.Counts.ScansDone != 1 {
		t.Errorf("after the backfill's claim ended the job is %+v, %v; want done with 1 scan", done, err)
	}
}

// TestBackfillReplicaVerifiesAndEnds guards a person's backfill of a
// replica already in step: a new replicate job counts every scan, the copy
// verifies what the replica holds against its own verified manifests and
// puts no instance's object again, and the job ends done once the replica
// owes its backfill nothing, every scan in step. A backfill while that job
// runs answers it.
func TestBackfillReplicaVerifiesAndEnds(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-a", synthetic(1, 1, 2, 600))
	f.archive(2, "scan-b", synthetic(2, 1, 2, 800))
	f.attach()
	w := f.worker(f.config())
	if p := f.runOnce(w); p.InStep != 2 {
		t.Fatalf("the attach's pass is %+v, want both scans in step", p)
	}
	sched, err := f.p.Job(f.ctx, tenant.ReplicateJobID("copy", f.clock.Now()))
	if err != nil || sched.State != tenant.JobDone || sched.Counts.ScansDone != 2 {
		t.Fatalf("the attach's job after its copy is %+v, %v; want done, 2 scans", sched, err)
	}
	before := len(f.replica.putKeys())

	f.clock.Add(time.Minute)
	j, made, err := f.p.BackfillReplica(f.ctx, tenant.ReplicaBackfill{Replica: "copy", JobID: f.id("job"), Now: f.clock.Now()}, f.audit(), nil)
	if err != nil || !made || j.State != tenant.JobRunning || j.Counts.Scans != 2 || j.Replicate.Scheduled {
		t.Fatalf("a backfill answered %+v (made %v), %v; want a running job of 2 scans", j, made, err)
	}
	again, made, err := f.p.BackfillReplica(f.ctx, tenant.ReplicaBackfill{Replica: "copy", JobID: f.id("job"), Now: f.clock.Now()}, f.audit(), nil)
	if err != nil || made || again.ID != j.ID {
		t.Fatalf("a backfill while one runs answered %s (made %v), %v; want %s", again.ID, made, err, j.ID)
	}
	f.clock.Add(time.Second)
	if p := f.runOnce(w); p.InStep != 2 {
		t.Fatalf("the backfill's pass is %+v, want both scans in step again", p)
	}
	for _, k := range f.replica.putKeys()[before:] {
		if k, err := layout.Parse(k); err == nil && k.Kind == layout.Instance {
			t.Errorf("the backfill put an instance's object the replica held verified: %+v", k)
		}
	}
	done, err := f.p.Job(f.ctx, j.ID)
	if err != nil || done.State != tenant.JobDone || done.Counts.ScansDone != 2 || done.Ended.IsZero() {
		t.Errorf("the backfill's job after its copy is %+v, %v; want done with 2 scans done", done, err)
	}
	if _, open, err := f.p.ReplicateJob(f.ctx, "copy"); err != nil || open {
		t.Errorf("a replicate job is open after the backfill (%v, %v)", open, err)
	}
	f.wantSameTrees()
}
