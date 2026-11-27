package replicate

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// BenchmarkReplicateScan measures the copy of one scan of 16 files of 4 MiB
// from a directory storage to an empty one, each put read back: MB/s is
// the bytes of the scan's files a second.
func BenchmarkReplicateScan(b *testing.B) {
	f := newFixture(b)
	files := synthetic(1, 1, 16, 4<<20)
	f.archive(1, "scan-a", files)
	f.attach()
	cfg := f.config()
	cfg.Parallel = 4
	w := f.worker(cfg)
	var size int64
	for _, e := range files {
		size += e.size
	}
	b.SetBytes(size)
	root := b.TempDir()
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		if i > 0 {
			f.replica = &store{Store: openDir(b, filepath.Join(root, strconv.Itoa(i)))}
			if _, _, err := f.p.BackfillReplica(f.ctx, tenant.ReplicaBackfill{Replica: "copy", JobID: f.id("job"), Now: f.clock.Now()}, f.audit(), nil); err != nil {
				b.Fatal(err)
			}
		}
		b.StartTimer()
		p, err := w.RunOnce(f.ctx)
		if err != nil || p.InStep != 1 {
			b.Fatalf("the pass is %+v, %v", p, err)
		}
	}
	b.StopTimer()
	if r := f.row("scan-a"); r.State != tenant.ReplicaInStep {
		b.Fatalf("the scan is %s after the copy", r.State)
	}
}
