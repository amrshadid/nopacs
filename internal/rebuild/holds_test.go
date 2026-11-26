package rebuild

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/holds"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// keptOpen is the rig's storage as a hold writer opens it: the writer
// closes what it opens, and the rig closes the storage at the test's end.
type keptOpen struct{ blob.Store }

// Close keeps the storage open.
func (keptOpen) Close() error { return nil }

// TestHoldsAreReadThroughTheStorageReadFrom guards where a rebuild reads
// hold records: a storage read from another (its replica) has its records
// read there, under that storage's name, never from the storage it stands
// for; and a storage that cannot be opened is the error while every other
// storage's records are still read.
func TestHoldsAreReadThroughTheStorageReadFrom(t *testing.T) {
	ctx := context.Background()
	keys := testkit.InstallationKeys(t)
	stores := byName{}
	for _, name := range []string{"main", "a-copy", "other"} {
		s, err := dir.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		stores[name] = s
	}
	put := func(storage, id string) {
		b, err := record.EncodeHold(record.Hold{Format: record.Format, Kind: record.HoldKind, ID: id, Level: record.LevelSubject,
			Subject: "sub-1", State: record.HoldActive, ReasonSHA256: record.ReasonDigest("x"), Placed: time.Now().UTC(), Seq: 1}, rigTenantID, keys)
		if err != nil {
			t.Fatal(err)
		}
		key, _ := layout.HoldKey(id)
		if _, err := stores[storage].Put(ctx, key, bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	const onCopy, onMain, onOther = "01jd5x7k2m3n4p5q6r7s8t9v0a", "01jd5x7k2m3n4p5q6r7s8t9v0b", "01jd5x7k2m3n4p5q6r7s8t9v0c"
	put("a-copy", onCopy)
	put("main", onMain)
	put("other", onOther)
	get := func(ctx context.Context, name string) (Storage, error) { return stores.Get(ctx, name) }
	found, err := readHolds(ctx, []string{"gone", "main", "other"}, map[string]string{"main": "a-copy"}, get, rigTenantID, keys)
	if err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("a storage that cannot be opened answers %v, want its error", err)
	}
	var got []string
	for _, f := range found {
		for _, r := range f.Records {
			got = append(got, f.Storage+":"+r.ID)
		}
	}
	want := []string{"a-copy:" + onCopy, "other:" + onOther}
	if !slices.Equal(got, want) {
		t.Fatalf("the records read are %v, want %v", got, want)
	}
}

// TestAReplaceAfterALostDatabaseHoldsAsBefore guards that a hold outlives
// its database through a rebuild from the storage alone: a hold placed
// through the hold writer leaves its record on the storage; the catalog and
// every hold row are then lost, and a record that does not verify is put
// beside it; the replace makes the catalog again and makes the hold active
// again from its record, found on the storage, so a delete of what it
// covers is refused as held; the record that does not verify makes no hold
// and is tallied.
func TestAReplaceAfterALostDatabaseHoldsAsBefore(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	subject := r.scanPaths()[0].Subject
	st, ok := r.p.(holds.Store)
	if !ok {
		t.Fatal("the tenant's database keeps no holds")
	}
	w, err := holds.New(holds.Config{Tenant: "lab", TenantID: rigTenantID, Keys: r.keys, Store: st,
		Open: func(context.Context, string) (blob.Store, error) { return keptOpen{r.storage}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	h, made, err := w.Place(r.ctx, holds.Placing{Target: tenant.HoldTarget{Subject: subject}, Reason: "litigation", Audit: r.audit()})
	if err != nil || !made {
		t.Fatalf("Place = %+v, %v, %v", h, made, err)
	}
	forged, _ := layout.HoldKey("01jd5x7k2m3n4p5q6r7s8t9v0w")
	if _, err := r.storage.Put(r.ctx, forged, bytes.NewReader([]byte(`{"format":1,"kind":"hold"}`))); err != nil {
		t.Fatal(err)
	}
	r.forgetCatalog()
	sqliteExec(t, r.dbPath, "DELETE FROM hold_records", "DELETE FROM holds")
	if _, err := st.Hold(r.ctx, h.ID); err == nil {
		t.Fatal("the hold's row is still there")
	}
	var writes atomic.Int64
	j, tallies := r.run(r.config(&writes), tenant.RebuildReplace)
	if j.State != tenant.JobDone || j.Rebuild == nil || !j.Rebuild.Activated {
		t.Fatalf("the replace ended %+v", j)
	}
	got, err := st.Hold(r.ctx, h.ID)
	if err != nil || got.State != tenant.HoldActive || !got.FoundOnStorage || got.Target != h.Target {
		t.Fatalf("after the replace the hold is %+v (%v), want it active again from its record", got, err)
	}
	if n := tallies[tenant.TallyUnauthenticatedHold]; n != 1 {
		t.Fatalf("the record that does not verify is tallied %d, want 1 (tallies %v)", n, tallies)
	}
	if hs, _, err := st.ListHolds(r.ctx, tenant.HoldFilter{}, pageOf(10)); err != nil || len(hs) != 1 {
		t.Fatalf("the holds after the replace: %+v, %v", hs, err)
	}
	preview, err := r.p.PreviewDelete(r.ctx, tenant.CorrectionTarget{Subject: subject}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := tenant.NewCorrectionID(time.Now())
	_, _, err = r.p.PrepareCorrection(r.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindDelete, Target: tenant.CorrectionTarget{Subject: subject},
		Reason: "consent withdrawn", Confirm: preview.Confirm, Now: time.Now()}, r.audit(), nil)
	var held *tenant.HeldRefusal
	if !errors.As(err, &held) || held.Hold != h.ID {
		t.Fatalf("a delete of the held subject after the replace = %v, want HELD by %s", err, h.ID)
	}
}
