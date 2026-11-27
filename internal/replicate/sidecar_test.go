package replicate

import (
	"bytes"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestSidecarIsPutAgainstTheCatalogsDigest guards a scan whose sidecar's
// digest the catalog keeps: its copy puts the sidecar the catalog names
// and ends in step; and a sidecar on the primary that is not the catalog's
// (a reception's next one, put before its commit, its manifest not yet
// written) is refused as a change, so the replica keeps the sidecar that
// goes with its manifest and the scan is not recorded in step with another.
func TestSidecarIsPutAgainstTheCatalogsDigest(t *testing.T) {
	f := newFixture(t)
	f.sidecarDigest = true
	s := f.archive(1, "scan-a", synthetic(1, 1, 2, 700))
	f.attach()
	w := f.worker(f.config())
	if p := f.runOnce(w); p.InStep != 1 {
		t.Fatalf("the pass is %+v, want the scan in step", p)
	}
	sk := mustKey(layout.MetadataKey(s.path))
	committed := f.read(f.replica, sk)
	f.wantSameTrees()

	f.putPrimary(sk, []byte("a sidecar a reception put before its commit\n"))
	if _, _, err := f.p.BackfillReplica(f.ctx, tenant.ReplicaBackfill{Replica: "copy", JobID: f.id("job"), Now: f.clock.Now()}, f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if p := f.runOnce(w); p.InStep != 0 || p.Failed != 1 {
		t.Fatalf("the pass over a sidecar not the catalog's is %+v, want the copy failed as a change", p)
	}
	if r := f.row("scan-a"); r.State == tenant.ReplicaInStep {
		t.Errorf("the scan was recorded in step with a sidecar the catalog does not name: %+v", r)
	}
	if got := f.read(f.replica, sk); !bytes.Equal(got, committed) {
		t.Errorf("the replica's sidecar is %q; want the committed one kept", got)
	}
}

// read returns the whole object at key on s.
func (f *fixture) read(s *store, key string) []byte {
	f.t.Helper()
	rc, err := s.Store.Read(f.ctx, key, 0, -1)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	var b bytes.Buffer
	if _, err := b.ReadFrom(rc); err != nil {
		f.t.Fatal(err)
	}
	return b.Bytes()
}
