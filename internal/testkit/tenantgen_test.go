package testkit_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// profiles reads the public test files the generator makes instances from.
func profiles(t *testing.T) []testkit.FileProfile {
	t.Helper()
	var out []testkit.FileProfile
	for _, name := range []string{"MR_small.dcm", "ct5n-2062.dcm"} {
		b, err := os.ReadFile(filepath.Join("..", "receive", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, testkit.ProfileOf(t, b))
	}
	return out
}

// openSQLite opens a migrated SQLite tenant in a temporary directory.
func openSQLite(t *testing.T) tenant.Pipeline {
	t.Helper()
	return migrated(t, driver.SQLiteTenants{DataDir: t.TempDir()}, "")
}

// migrated opens the tenant "synthetic" with tenants at address, migrates
// it, and closes it when t ends.
func migrated(t *testing.T, tenants interface {
	Open(ctx context.Context, id, name, address string) (tenant.Store, error)
}, address string) tenant.Pipeline {
	t.Helper()
	ctx := context.Background()
	s, err := tenants.Open(ctx, "synthetic", "synthetic", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	p, ok := s.(tenant.Pipeline)
	if !ok {
		t.Fatal("the tenant's store keeps no pipeline")
	}
	return p
}

// scanContents returns every instance of the scan of seriesUID.
func scanContents(t *testing.T, s tenant.Pipeline, seriesUID string) []tenant.ScanInstance {
	t.Helper()
	var out []tenant.ScanInstance
	page := master.Page{Limit: 1000}
	for {
		got, next, err := s.ScanInstances(context.Background(), seriesUID, page)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, got...)
		if next == "" {
			return out
		}
		page.Cursor = next
	}
}

// receptionsOf returns every reception of the store, newest first.
func receptionsOf(t *testing.T, s tenant.Pipeline) []tenant.Reception {
	t.Helper()
	var out []tenant.Reception
	page := master.Page{Limit: 1000}
	for {
		got, next, err := s.ListReceptions(context.Background(), tenant.ReceptionFilter{}, page)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, got...)
		if next == "" {
			return out
		}
		page.Cursor = next
	}
}

// TestGenerateTenant guards the generator's promises on both databases: the
// tenant holds exactly the instances asked for, every reception it made is
// done with its files archived, and the catalog's totals agree with what
// it reports; a tenant made in two parts (FirstStudy) holds the studies of
// the tenant made at once, but for the two the parts cut, so a measurement
// can add to a large tenant without a UID clashing; and the
// same seed gives the same UIDs, labels and keys on SQLite and Postgres.
func TestGenerateTenant(t *testing.T) {
	if testing.Short() {
		t.Skip("thousands of rows written through two databases: skipped under -short")
	}
	ctx := context.Background()
	shape := testkit.TenantShape{Instances: 8000, Seed: 7, Profiles: profiles(t), Workers: 3}

	whole := openSQLite(t)
	got, err := testkit.GenerateTenant(ctx, whole, shape)
	if err != nil {
		t.Fatal(err)
	}
	if got.Instances != shape.Instances || got.Studies != got.NextStudy {
		t.Fatalf("generated %+v, want %d instances in every study planned", got, shape.Instances)
	}
	totals, err := whole.CatalogTotals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if totals.Instances != shape.Instances || totals.Sessions != int64(got.Studies) || totals.Scans != got.Scans || totals.Subjects < 1 ||
		totals.Subjects > totals.Sessions {
		t.Errorf("catalog totals %+v, generator reported %+v", totals, got)
	}
	recs := receptionsOf(t, whole)
	if len(recs) != got.Studies {
		t.Fatalf("%d receptions, want one per study (%d)", len(recs), got.Studies)
	}
	var archived int64
	for _, r := range recs {
		if r.State != "done" {
			t.Errorf("reception %s is %s, want done", r.ID, r.State)
		}
		archived += r.Counts.FilesArchived
	}
	if archived != shape.Instances {
		t.Errorf("receptions archived %d files, want %d", archived, shape.Instances)
	}

	parts := openSQLite(t)
	half := shape
	half.Instances = 4000
	first, err := testkit.GenerateTenant(ctx, parts, half)
	if err != nil {
		t.Fatal(err)
	}
	rest := shape
	rest.Instances, rest.FirstStudy = 3000, first.NextStudy
	second, err := testkit.GenerateTenant(ctx, parts, rest)
	if err != nil {
		t.Fatal(err)
	}
	if first.Instances+second.Instances != 7000 || second.NextStudy <= first.NextStudy {
		t.Fatalf("parts wrote %+v and %+v", first, second)
	}
	byID := map[string]tenant.Reception{}
	for _, r := range recs {
		byID[r.ID] = r
	}
	cut := 0
	for _, r := range receptionsOf(t, parts) {
		w, ok := byID[r.ID]
		if !ok || w.StudyUID != r.StudyUID || w.SubjectLabel != r.SubjectLabel || w.SessionLabel != r.SessionLabel {
			t.Fatalf("reception %s of the tenant made in parts is not the one made at once", r.ID)
		}
		if w.Counts.FilesArchived != r.Counts.FilesArchived {
			cut++
		}
	}
	if cut > 2 {
		t.Errorf("%d studies differ between the tenant made in parts and at once; only the two cut may", cut)
	}

	address, ok := testkit.PostgresSchema(t)
	if !ok {
		t.Log("Postgres is not set: the same tenant on both databases is not compared")
		return
	}
	pg := migrated(t, driver.PostgresTenants{}, address)
	if _, err := testkit.GenerateTenant(ctx, pg, shape); err != nil {
		t.Fatal(err)
	}
	pgRecs := receptionsOf(t, pg)
	if len(pgRecs) != len(recs) {
		t.Fatalf("%d receptions on Postgres, %d on SQLite", len(pgRecs), len(recs))
	}
	for i, r := range recs {
		p := pgRecs[i]
		if p.ID != r.ID || p.StudyUID != r.StudyUID || p.SubjectLabel != r.SubjectLabel || p.SessionLabel != r.SessionLabel {
			t.Fatalf("reception %d differs: %+v on Postgres, %+v on SQLite", i, p, r)
		}
		scans, _, err := whole.ReceptionScans(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		pgScans, _, err := pg.ReceptionScans(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(scans) != len(pgScans) {
			t.Fatalf("reception %s has %d scans on SQLite, %d on Postgres", r.ID, len(scans), len(pgScans))
		}
		for j, sc := range scans {
			if pgScans[j] != sc {
				t.Fatalf("scan %+v on Postgres, %+v on SQLite", pgScans[j], sc)
			}
			a, b := scanContents(t, whole, sc.SeriesUID), scanContents(t, pg, sc.SeriesUID)
			if len(a) != len(b) {
				t.Fatalf("scan %s holds %d instances on SQLite, %d on Postgres", sc.Label, len(a), len(b))
			}
			for n := range a {
				if a[n].SOPInstanceUID != b[n].SOPInstanceUID || a[n].Key != b[n].Key || a[n].SHA256 != b[n].SHA256 {
					t.Fatalf("instance %d of scan %s differs between the databases", n, sc.Label)
				}
			}
		}
	}
}
