package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/amrshadid/nopacs/internal/testkit"
)

// benchSearchPreload is how many scans a tenant holds before the write cost
// of a search table is measured: the tables a commit writes into are loaded,
// as a tenant's are.
const benchSearchPreload = 5000

// BenchmarkCommitScanSearch measures the catalog commit of one scan of one
// instance, one transaction each, into a tenant of 5,000 scans: first with
// no search row (the bare catalog commit), then with the row of all 22
// attributes. The difference of the two is the write cost of keeping the
// search table; each reports commits a second as well.
func BenchmarkCommitScanSearch(b *testing.B) {
	benchCommitSearch(b, benchSearchPreload)
}

// BenchmarkScaleCommitScanSearch is BenchmarkCommitScanSearch into a tenant
// of NOPACS_SCALE_INSTANCES scans, the size the search is measured at. It
// is skipped unless that variable is set.
func BenchmarkScaleCommitScanSearch(b *testing.B) {
	benchCommitSearch(b, int(testkit.ScaleInstances(b)))
}

// benchCommitSearch preloads a tenant with scans and runs the commit
// benchmark without and with the search row.
func benchCommitSearch(b *testing.B, preload int) {
	b.Helper()
	ctx := context.Background()
	s, err := OpenTenant(ctx, filepath.Join(b.TempDir(), "tenant.db"), "bench")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		b.Fatal(err)
	}
	profile := testkit.FileProfile{SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1", Size: 524288, Modality: "MR"}
	if preload > 0 {
		// The preload is written with commits not synced and a large page
		// cache: it only makes the tenant large. The measured commits are
		// synced, as a server's are.
		if err := s.setBulkWrites(ctx, true, 1024); err != nil {
			b.Fatal(err)
		}
		if _, err := testkit.GenerateTenant(ctx, s, testkit.TenantShape{Instances: int64(preload), ScanInstances: 1, SearchRows: true, Seed: 1,
			Profiles: []testkit.FileProfile{profile}, Workers: 4}); err != nil {
			b.Fatal(err)
		}
		if err := s.setBulkWrites(ctx, false, 0); err != nil {
			b.Fatal(err)
		}
	}
	for _, c := range []struct {
		name string
		with bool
		seed uint64
	}{{"bare", false, 100}, {"with search row", true, 101}} {
		b.Run(c.name, func(b *testing.B) { testkit.BenchCommitScan(b, s, c.with, c.seed) })
	}
}
