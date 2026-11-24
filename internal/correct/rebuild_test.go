package correct

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/rebuild"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// oneTenant is the installation of the rig's one tenant.
type oneTenant struct{ p tenant.Pipeline }

// Names answers the one tenant.
func (o oneTenant) Names(context.Context) ([]string, error) { return []string{"lab"}, nil }

// Store answers its database.
func (o oneTenant) Store(string) (tenant.Store, error) { return o.p, nil }

// storages answers every storage name with the rig's one store.
type storages struct{ s blob.Store }

// Get answers the storage.
func (s storages) Get(context.Context, string) (blob.Store, error) { return s.s, nil }

// Close keeps it open: the rig closes it.
func (storages) Close() {}

// compare runs a rebuild of compare mode, from the storage alone, and
// answers its tallies; it fails the test when the shadow differs from the
// catalog in use in any table.
func (r *rig) compare(t *testing.T) map[string]int64 {
	t.Helper()
	j, err := r.p.CreateJob(r.ctx, tenant.NewJob{ID: fmt.Sprintf("job%08d", r.seq.Add(1)), Kind: tenant.JobRebuild,
		Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare}, Created: time.Now()}, r.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	sup, err := rebuild.New(rebuild.Config{Tenants: oneTenant{p: r.p}, OpenStorages: func(string, tenant.Pipeline) rebuild.Storages {
		return storages{s: r.storage}
	}, PollInterval: 20 * time.Millisecond, Interval: time.Second, Keys: r.keys,
		TenantID: func(context.Context, string) (string, error) { return rigTenantID, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(r.ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = sup.Stop(ctx)
	}()
	deadline := time.Now().Add(90 * time.Second)
	for {
		got, err := r.p.Job(r.ctx, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State.Ended() {
			// What it leaves out makes it partial; it never fails.
			if got.State != tenant.JobDone && got.State != tenant.JobPartial {
				t.Fatalf("the rebuild ended %s: %s", got.State, got.Reason)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the rebuild did not end: %+v", got)
		}
		sup.Poke("lab")
		time.Sleep(20 * time.Millisecond)
	}
	tallies, err := r.p.JobTallies(r.ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]int64{}
	for _, x := range tallies {
		by[x.Key] = x.Count
		if strings.HasPrefix(x.Key, "differs|") {
			t.Errorf("the catalog made from the storage differs from the one in use: %s %d", x.Key, x.Count)
		}
	}
	// Every record the engine wrote, at each step, verifies.
	if n := by[tenant.TallyUnauthenticatedCorrection]; n != 0 {
		t.Errorf("the rebuild found %d records the engine wrote that do not verify", n)
	}
	return by
}

// TestRebuildAgreesAfterCorrections guards that the catalog a rebuild
// makes from the storage alone (its correction records read first) is the
// catalog in use after each correction: a scan's delete in its grace, its
// bytes all still there, left out by its record and counted unresolved (no
// row confirms it to a rebuild); an instance's delete in its grace, and
// after its purge rewrote the scan's manifest; a replace whose new line was
// made after the instance was first held, in its grace and after its purge,
// the instance's held time compared with the rest; a relabel in its grace,
// both label trees on the storage, and after its purge.
func TestRebuildAgreesAfterCorrections(t *testing.T) {
	r := newRig(t, false)
	r.archive(crashFiles...)
	e := r.engine()
	scans := r.scans()
	var two []tenant.CatalogScan
	for _, s := range scans {
		if s.Instances >= 2 {
			two = append(two, s)
		}
	}
	if len(two) < 2 {
		t.Fatalf("the corpus gives %d scans of two instances or more, want two", len(two))
	}
	s := two[0]
	if _, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label}); err != nil {
		t.Fatal(err)
	}
	if by := r.compare(t); by[tenant.TallyUnresolvedCorrection] != 1 || by[tenant.TallyLeftOutDeleted] != 1 {
		t.Fatalf("after a scan's delete the rebuild's tallies are %v, want it unresolved and its scan left out", by)
	}
	o := two[1]
	in := r.held(pathOf(o))[0]
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: o.SubjectLabel, Session: o.SessionLabel, Scan: o.Label,
		Instance: in.SOPInstanceUID})
	if err != nil {
		t.Fatal(err)
	}
	if by := r.compare(t); by[tenant.TallyUnresolvedCorrection] != 2 {
		t.Fatalf("after an instance's delete the rebuild's tallies are %v", by)
	}
	if _, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	r.compare(t)
	// A replace, its line made later than the instance was first held, as
	// the pipeline makes it: in its grace, and after its purge removed the
	// revision it superseded and that revision's line with it.
	rin := r.held(pathOf(o))[0]
	old, err := readAll(r.ctx, r.storage, rin.Key, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	corrected := append(append([]byte(nil), old...), 0)
	q := r.holdDifferent(o, rin, corrected)
	line := r.lineFor(pathOf(o), rin.SOPInstanceUID)
	line.HeldAt = time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	rp, err := e.Replace(r.ctx, ReplaceRequest{Quarantine: q, Storage: "main", Path: pathOf(o), UID: rin.SOPInstanceUID,
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(corrected)), nil }, Line: line,
		Reason: "corrected re-send", Audit: r.audit()})
	if err != nil {
		t.Fatal(err)
	}
	r.compare(t)
	if _, err := e.PurgeNow(r.ctx, rp.ID, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	r.compare(t)
	var other tenant.CatalogScan
	for _, x := range r.scans() {
		if x.SubjectLabel != o.SubjectLabel {
			other = x
		}
	}
	if other.SubjectLabel == "" {
		t.Skip("the corpus gives one subject left")
	}
	rc, err := e.Relabel(r.ctx, RelabelRequest{Target: tenant.CorrectionTarget{Subject: other.SubjectLabel}, ToLabel: "renamed-2",
		Reason: "wrong subject id", Audit: r.audit()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ContinueRelabel(r.ctx, rc.ID); err != nil {
		t.Fatal(err)
	}
	r.compare(t)
	if _, err := e.PurgeNow(r.ctx, rc.ID, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	r.compare(t)
}
