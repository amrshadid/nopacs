package rebuild

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// byName answers each storage by its name.
type byName map[string]blob.Store

// Get answers the storage named.
func (b byName) Get(_ context.Context, name string) (blob.Store, error) {
	s, ok := b[name]
	if !ok {
		return nil, fmt.Errorf("no storage %q", name)
	}
	return s, nil
}

// Close keeps them open: the test closes them.
func (byName) Close() {}

// compareFrom runs a compare that reads the storage named alone.
func (r *rig) compareFrom(cfg Config, from string) (tenant.Job, map[string]int64) {
	r.t.Helper()
	scratch := ""
	if r.address != "" {
		var ok bool
		if scratch, ok = testkit.PostgresScratch(r.t); !ok {
			r.t.Skip("no Postgres to make a scratch schema in")
		}
	}
	j, err := r.p.CreateJob(r.ctx, tenant.NewJob{ID: fmt.Sprintf("job%08d", r.seq.Add(1)), Kind: tenant.JobRebuild,
		Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare, Scratch: scratch, FromStorage: from}, Created: time.Now()}, r.audit(), nil)
	if err != nil {
		r.t.Fatal(err)
	}
	s := r.start(cfg)
	j = r.waitEnded(s, j.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		r.t.Fatal(err)
	}
	return j, r.tallies(j.ID)
}

// TestCompareFromAReplicaReadsItAlone guards a compare that names a replica
// to read from: it reads the replica alone, for its primary, so a replica
// that holds nothing yet makes the compare differ by every scan, and once
// it holds its primary's files the compare finds the catalog equal, every
// scan placed on the primary as the catalog places it.
func TestCompareFromAReplicaReadsItAlone(t *testing.T) {
	drivers(t, func(t *testing.T, postgres bool) {
		r := newRig(t, postgres)
		r.archive(studyFiles...)
		if _, err := r.p.AddStorage(r.ctx, tenant.Storage{Name: "a-copy", Type: tenant.StorageDirectory,
			Directory: tenant.DirectoryStorage{Root: "replicas"}, ReplicaOf: "main"}, r.clock.Now(), r.audit(), nil); err != nil {
			t.Fatal(err)
		}
		replica, err := dir.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = replica.Close() })
		totals, err := r.p.CatalogTotals(r.ctx)
		if err != nil {
			t.Fatal(err)
		}
		var writes atomic.Int64
		cfg := r.config(&writes)
		cfg.OpenStorages = func(string, tenant.Pipeline) Storages {
			return byName{"main": readOnly{Store: r.storage, writes: &writes}, "a-copy": readOnly{Store: replica, writes: &writes}}
		}

		j, tallies := r.compareFrom(cfg, "a-copy")
		if j.Counts.Scans != 0 || !differs(tallies) {
			t.Errorf("the compare from an empty replica read %d scans and tallied %v; want none read and the catalog differing",
				j.Counts.Scans, tallies)
		}

		page, err := r.storage.List(r.ctx, blob.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range page.Keys {
			rc, err := r.storage.Read(r.ctx, k, 0, -1)
			if err != nil {
				t.Fatal(err)
			}
			_, err = replica.Put(r.ctx, k, rc)
			_ = rc.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		j, tallies = r.compareFrom(cfg, "a-copy")
		if j.State != tenant.JobDone || j.Counts.Scans != totals.Scans || j.Counts.InstancesSent != totals.Instances || differs(tallies) {
			t.Errorf("the compare from a full replica ended %+v with %v; want the catalog's %d scans read and nothing differing",
				j, tallies, totals.Scans)
		}
		if writes.Load() != 0 {
			t.Errorf("the compares wrote %d times to a storage", writes.Load())
		}
	})
}

// TestCompareFromAReplicaOfOnePrimary guards a compare from a replica on a
// tenant with two primaries: it is limited to the catalog of the replica's
// primary, so an exact replica of it compares equal, and the scans of the
// other primary are not answered as removed.
func TestCompareFromAReplicaOfOnePrimary(t *testing.T) {
	drivers(t, func(t *testing.T, postgres bool) {
		r := newRig(t, postgres)
		r.archive(studyFiles[:7]...)
		mainScans := len(r.scanPaths())
		if _, err := r.p.AddStorage(r.ctx, tenant.Storage{Name: "second", Type: tenant.StorageDirectory,
			Directory: tenant.DirectoryStorage{Root: "second"}}, r.clock.Now(), r.audit(), nil); err != nil {
			t.Fatal(err)
		}
		if _, err := r.p.SetReceiver(r.ctx, tenant.Receiver{Name: "dimse", Storage: "second"}, []tenant.ReceiverField{tenant.FieldStorage},
			r.clock.Now(), r.audit(), nil); err != nil {
			t.Fatal(err)
		}
		second, err := dir.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = second.Close() })
		r.archiveOn(second, "MR_small.dcm", "liver_1frame.dcm")
		if all := len(r.scanPaths()); all <= mainScans {
			t.Fatalf("the second primary holds no scan: %d scans in all, %d on main", all, mainScans)
		}
		if _, err := r.p.AddStorage(r.ctx, tenant.Storage{Name: "a-copy", Type: tenant.StorageDirectory,
			Directory: tenant.DirectoryStorage{Root: "replicas"}, ReplicaOf: "main"}, r.clock.Now(), r.audit(), nil); err != nil {
			t.Fatal(err)
		}
		replica, err := dir.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = replica.Close() })
		page, err := r.storage.List(r.ctx, blob.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range page.Keys {
			rc, err := r.storage.Read(r.ctx, k, 0, -1)
			if err != nil {
				t.Fatal(err)
			}
			_, err = replica.Put(r.ctx, k, rc)
			_ = rc.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		var writes atomic.Int64
		cfg := r.config(&writes)
		cfg.OpenStorages = func(string, tenant.Pipeline) Storages {
			return byName{"main": readOnly{Store: r.storage, writes: &writes}, "second": readOnly{Store: second, writes: &writes},
				"a-copy": readOnly{Store: replica, writes: &writes}}
		}
		j, tallies := r.compareFrom(cfg, "a-copy")
		if j.State != tenant.JobDone || j.Counts.Scans != int64(mainScans) || differs(tallies) {
			t.Errorf("the compare from an exact replica of one primary ended %s with %d scans and %v; want done, %d scans, nothing differing",
				j.State, j.Counts.Scans, tallies, mainScans)
		}
	})
}

// differs reports whether a compare's tallies hold a difference.
func differs(tallies map[string]int64) bool {
	for k := range tallies {
		if strings.HasPrefix(k, "differs|") {
			return true
		}
	}
	return false
}
