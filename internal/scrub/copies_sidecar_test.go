package scrub

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestReplicaSidecarIsCheckedAgainstTheCatalogsDigest guards that, for a
// scan whose metadata file's digest the catalog keeps, a scrub reads the
// replica's metadata file against it: the file in step reads good; other
// bytes are the replica's damage, with a finding that it disagrees on the
// replica's name; and once the file is the catalog's again a scrub clears
// both.
func TestReplicaSidecarIsCheckedAgainstTheCatalogsDigest(t *testing.T) {
	r := newReplicaRig(t)
	p, series, _ := r.archiveTwo("s1", "1.2.826.0.1.3680043.10.541")
	sk, _ := layout.MetadataKey(p)
	sidecar := []byte("{\"a metadata file\":1}\n")
	r.putOn(r.store, sk, sidecar)
	if ok, err := r.p.RecordSidecarRewritten(r.ctx, series, "main", tenant.SHA256(sha256.Sum256(sidecar)), time.Now()); err != nil || !ok {
		t.Fatalf("RecordSidecarRewritten = %v, %v", ok, err)
	}
	copy1 := r.addReplica("copy")
	r.putOn(copy1, sk, sidecar)
	r.copyAll()

	r.scrubAll(r.config())
	if row := r.row("copy", series); row.State != tenant.ReplicaInStep {
		t.Fatalf("after a scrub of a replica whose metadata file is the catalog's its row is %s", row.State)
	}
	if got := r.findingsOn("copy"); len(got) != 0 {
		t.Fatalf("the scrub holds findings %v on the replica; want none", got)
	}

	r.putOn(copy1, sk, []byte("{\"another metadata file\":2}\n"))
	j := r.scrubAll(r.config())
	if row := r.row("copy", series); row.State != tenant.ReplicaDamaged || row.Damaged != 1 {
		t.Errorf("after a scrub that read another metadata file on the replica its row is %s with %d damaged; want damaged with 1",
			row.State, row.Damaged)
	}
	if got := r.findingsOn("copy"); len(got) != 1 || got[0] != "disagreeing "+sk {
		t.Errorf("the scrub holds findings %v on the replica; want its metadata file disagreeing", got)
	}
	if got := r.tallies(j.ID)[TallyReplicaDamaged]; got != 1 {
		t.Errorf("the scrub tallied %d objects damaged on the replica; want 1", got)
	}

	r.putOn(copy1, sk, sidecar)
	r.scrubAll(r.config())
	if row := r.row("copy", series); row.State != tenant.ReplicaInStep || row.Damaged != 0 {
		t.Errorf("after a scrub that read the metadata file good again the row is %s with %d damaged", row.State, row.Damaged)
	}
	if got := r.findingsOn("copy"); len(got) != 0 {
		t.Errorf("after a scrub that read the metadata file good again the findings on the replica are %v; want none", got)
	}
}

// TestReplicaSidecarDuringAPurge guards a scrub between the start of an
// instance delete's purge and its end: the purge puts the scan's metadata
// file written without the instance on the primary and every replica
// before its end records anything, and the scrub then records no damage
// of the replica's file, which is the purge's.
func TestReplicaSidecarDuringAPurge(t *testing.T) {
	r := newReplicaRig(t)
	p, series, _ := r.archiveTwo("s1", "1.2.826.0.1.3680043.10.543")
	sk, _ := layout.MetadataKey(p)
	old := []byte("{\"a metadata file\":1}\n")
	r.putOn(r.store, sk, old)
	if ok, err := r.p.RecordSidecarRewritten(r.ctx, series, "main", tenant.SHA256(sha256.Sum256(old)), time.Now()); err != nil || !ok {
		t.Fatalf("RecordSidecarRewritten = %v, %v", ok, err)
	}
	copy1 := r.addReplica("copy")
	r.putOn(copy1, sk, old)
	r.copyAll()
	uid := r.held(p)[0].SOPInstanceUID
	target := tenant.CorrectionTarget{Subject: p.Subject, Session: p.Session, Scan: p.Scan, Instance: uid}
	now := time.Now()
	pv, err := r.p.PreviewDelete(r.ctx, target, now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := tenant.NewCorrectionID(now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.p.PrepareCorrection(r.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: pv.Confirm, Now: now}, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.p.CommitCorrection(r.ctx, tenant.CorrectionCommit{ID: id, Now: now, Grace: time.Hour}); err != nil {
		t.Fatal(err)
	}
	r.copyAll()
	a := r.audit()
	if _, err := r.p.StartPurge(r.ctx, id, time.Now(), &a, nil); err != nil {
		t.Fatal(err)
	}
	newer := []byte("{\"the metadata file without the instance\":2}\n")
	r.putOn(r.store, sk, newer)
	r.putOn(copy1, sk, newer)
	r.scrubAll(r.config())
	if row := r.row("copy", series); row.State != tenant.ReplicaInStep || row.Damaged != 0 {
		t.Errorf("after a scrub during the purge the replica's row is %s with %d damaged; want in step with none", row.State, row.Damaged)
	}
	for _, f := range r.findingsOn("copy") {
		if f == "disagreeing "+sk {
			t.Errorf("the scrub during the purge holds findings %v on the replica; want none of its metadata file", r.findingsOn("copy"))
		}
	}
}

// TestReplicaSidecarRewrittenDuringAScrub guards a scan whose metadata file
// is rewritten, its new digest recorded and the replica copied again,
// after the scrub planned its unit and before it read the replica's file:
// the replica holds the catalog's new file, so nothing is recorded of it,
// though the file is not the one the unit planned with.
func TestReplicaSidecarRewrittenDuringAScrub(t *testing.T) {
	r := newReplicaRig(t)
	p, series, _ := r.archiveTwo("s1", "1.2.826.0.1.3680043.10.542")
	sk, _ := layout.MetadataKey(p)
	first := []byte("{\"a metadata file\":1}\n")
	r.putOn(r.store, sk, first)
	if ok, err := r.p.RecordSidecarRewritten(r.ctx, series, "main", tenant.SHA256(sha256.Sum256(first)), time.Now()); err != nil || !ok {
		t.Fatalf("RecordSidecarRewritten = %v, %v", ok, err)
	}
	copy1 := r.addReplica("copy")
	r.putOn(copy1, sk, first)
	r.copyAll()
	before := r.row("copy", series).Attempt

	second := []byte("{\"the metadata file rewritten\":2}\n")
	rewritten := false
	cfg := withRead(r.config(), func(key string, _ int) {
		if key != sk || rewritten {
			return
		}
		rewritten = true
		r.putOn(r.store, sk, second)
		r.putOn(copy1, sk, second)
		if ok, err := r.p.RecordSidecarRewritten(r.ctx, series, "main", tenant.SHA256(sha256.Sum256(second)), time.Now()); err != nil || !ok {
			t.Errorf("RecordSidecarRewritten during the scrub = %v, %v", ok, err)
		}
		r.copyAll()
	})
	r.scrubAll(cfg)
	if !rewritten {
		t.Fatal("the scrub read no metadata file")
	}
	if row := r.row("copy", series); row.State != tenant.ReplicaInStep || row.Damaged != 0 || row.Attempt == before {
		t.Errorf("after the scrub the replica's row is %s with %d damaged at attempt %d (before %d); want in step, none damaged, copied again",
			row.State, row.Damaged, row.Attempt, before)
	}
	if got := r.findingsOn("copy"); len(got) != 0 {
		t.Errorf("the scrub holds findings %v on the replica; want none", got)
	}
}
