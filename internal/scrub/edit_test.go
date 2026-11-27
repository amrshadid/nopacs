package scrub

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// passEdits passes every new revision an edit writes: these tests are of
// what a scrub expects of an edit, not of its judging.
type passEdits struct{}

// JudgeEdit passes the file.
func (passEdits) JudgeEdit(context.Context, correct.EditedFile) error { return nil }

// editsStopAt is stopAt over a database that keeps edits.
type editsStopAt struct {
	stopAt
	tenant.Edits
}

// Correction fails when the publish is set to stop, as stopAt's does.
func (s editsStopAt) Correction(ctx context.Context, id string) (tenant.Correction, error) {
	return s.stopAt.Correction(ctx, id)
}

// noDelete is a storage whose removals fail: an undone edit whose sweep
// has not run.
type noDelete struct{ blob.Store }

// Delete fails.
func (noDelete) Delete(context.Context, string) error { return errStop }

// editEngine returns a corrections engine that makes edits, over the rig's
// database as db answers it and its storage as store answers it.
func (r *corrRig) editEngine(db tenant.Corrections, store blob.Store) *correct.Engine {
	r.t.Helper()
	e, err := correct.New(correct.Config{Tenant: "lab", TenantID: fixtureTenantID, Keys: r.installationKeys, Store: db, Now: r.clock.Now,
		Judge:       passEdits{},
		OpenStorage: func(context.Context, string) (blob.Store, error) { return store, nil },
		Grace:       func(context.Context) (time.Duration, error) { return 24 * time.Hour, nil }})
	if err != nil {
		r.t.Fatal(err)
	}
	return e
}

// edit previews and makes an edit of the scan s's series description with
// e.
func (r *corrRig) edit(e *correct.Engine, s tenant.CatalogScan) (tenant.Correction, error) {
	r.t.Helper()
	target := tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label}
	changes := []correct.EditChange{{Tag: 0x0008103E, Value: "Scout view"}}
	p, err := e.PreviewEdit(r.ctx, correct.EditPreviewRequest{Target: target, Changes: changes})
	if err != nil {
		r.t.Fatal(err)
	}
	return e.Edit(r.ctx, correct.EditRequest{Target: target, Changes: changes, Reason: "series named wrongly", Confirm: p.Confirm,
		Audit: r.audit()})
}

// TestScrubDuringAnEditFindsNothing guards that a scrub knows what an edit
// keeps, as it knows a replace's: with an edit stopped at its publish (its
// new revisions put, the manifest and metadata naming them), after an
// edit's publish (the superseded revisions kept, not orphans, the new ones
// read good), and with an undone edit whose sweep has not yet removed its
// new revisions, it calls nothing a difference; after the purge the
// superseded revisions are gone and it still finds nothing.
func TestScrubDuringAnEditFindsNothing(t *testing.T) {
	r := newCorrRig(t)
	var scans []tenant.CatalogScan
	for _, s := range r.scans() {
		if s.Instances >= 2 {
			scans = append(scans, s)
		}
	}
	if len(scans) < 2 {
		t.Fatalf("the corpus gave fewer than two scans of two instances: %+v", r.scans())
	}
	edits := r.p.(tenant.Edits)

	// An edit stopped at its publish: nothing is rolled back.
	first := pathOf(scans[0])
	stopped := r.editEngine(editsStopAt{stopAt: stopAt{Corrections: r.p, commit: true}, Edits: edits}, r.store)
	if _, err := r.edit(stopped, scans[0]); !errors.Is(err, errStop) {
		t.Fatalf("the edit did not stop at its publish: %v", err)
	}
	in := r.held(first)[0]
	key, _ := layout.RevisionKey(first, in.SOPInstanceUID, 2)
	if got := r.lineOf(first, in.SOPInstanceUID); got.Key != key {
		t.Fatalf("the stopped edit's metadata line is %+v, want %s", got, key)
	}
	r.scrubClean("with an edit stopped before its publish")

	second := pathOf(scans[1])
	before := r.held(second)
	e := r.editEngine(r.p, r.store)
	c, err := r.edit(e, scans[1])
	if err != nil || c.State != record.StateCommitted {
		t.Fatalf("Edit = %+v, %v", c, err)
	}
	r.readKeys()
	r.scrubClean("after an edit's publish")
	read := r.readKeys()
	for _, in := range before {
		key, _ := layout.RevisionKey(second, in.SOPInstanceUID, 2)
		if read[key] == 0 {
			t.Errorf("the scrub did not read the revision key %s", key)
		}
		if read[in.Key] != 0 {
			t.Errorf("the scrub read the superseded revision %s as an instance", in.Key)
		}
	}

	if c, err = e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil || c.State != record.StatePurged {
		t.Fatalf("PurgeNow = %+v, %v", c, err)
	}
	for _, in := range before {
		if _, err := r.store.Read(r.ctx, in.Key, 0, -1); err == nil {
			t.Fatalf("the purge left the superseded revision %s", in.Key)
		}
	}
	r.scrubClean("after the edit's purge")

	// An edit undone whose sweep could not remove its new revisions.
	u, err := r.edit(e, scans[1])
	if err != nil || u.State != record.StateCommitted {
		t.Fatalf("Edit = %+v, %v", u, err)
	}
	unswept := r.editEngine(r.p, noDelete{r.store})
	if got, err := unswept.Undo(r.ctx, u.ID, "the description was right", r.audit(), nil); err == nil || got.State != record.StateUndone || got.Swept {
		t.Fatalf("the undo whose sweep fails is %s swept %v (%v)", got.State, got.Swept, err)
	}
	r.scrubClean("with an undone edit not yet swept")
}
