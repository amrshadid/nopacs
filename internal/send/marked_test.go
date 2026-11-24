package send

import (
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestAMarkedInstanceIsNeverSent guards the mark a scrub leaves on an
// instance whose bytes it found damaged: the send refuses it as
// damaged_in_storage before a byte of it is read, though its object on the
// storage is whole, so no part of it reaches the peer, and sends the
// others.
func TestAMarkedInstanceIsNeverSent(t *testing.T) {
	f := newFixture(t)
	peer := &dimsePeer{}
	addr := startPeer(t, peer)
	files := generated(t, 3, 64<<10)
	f.archive("ses-1", scanFiles{"mr", files})
	marked := uidOf(t, files[1])
	k, err := layout.Parse(f.keys[marked])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.CreateJob(f.ctx, tenant.NewJob{ID: f.id("scrub"), Kind: tenant.JobScrub, Storage: "main", Created: f.clock.Now()},
		f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	w, err := f.p.ClaimWork(f.ctx, tenant.ScrubWorkKind, "scrubber", f.clock.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(files[1]))
	fd := tenant.ScrubFinding{Kind: tenant.FindingMissing, Storage: "main", Key: f.keys[marked], Path: k.Path, SOPInstanceUID: marked,
		Revision: 1, ExpectedSize: &size}
	fd.ID = tenant.ScrubFindingID("scrub", fd.Storage, fd.Key, fd.Kind)
	if _, _, err := f.p.RecordScrubFinding(f.ctx, tenant.ScrubFindingRecord{Fence: w.Fence(), Finding: fd, Now: f.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	f.addDIMSE("peer", addr)
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	j := f.waitJob(s, job.ID, 30*time.Second, ended)
	if j.State != tenant.JobPartial || j.Counts.InstancesSent != 2 || j.Counts.InstancesRefused != 1 {
		t.Fatalf("job %s %+v, want partial with 2 sent and the marked one refused", j.State, j.Counts)
	}
	fl := f.failures(job.ID)
	if len(fl) != 1 || fl[0].SOPInstanceUID != marked || fl[0].Code != tenant.FailureDamagedInStorage {
		t.Errorf("failures %+v, want damaged_in_storage of the marked instance", fl)
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if peer.count[marked] != 0 || peer.cut != 0 {
		t.Errorf("the peer answered the marked instance %d times and saw %d data sets cut, want neither", peer.count[marked], peer.cut)
	}
}
