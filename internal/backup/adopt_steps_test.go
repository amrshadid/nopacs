package backup

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// engineAt returns a corrections engine over the rig whose clock is now,
// with a grace period of two days.
func (r *rig) engineAt(now func() time.Time) *correct.Engine {
	r.t.Helper()
	e, err := correct.New(correct.Config{Tenant: "lab", TenantID: tenantID, Keys: r.keys, Store: r.p, Now: now,
		OpenStorage: func(context.Context, string) (blob.Store, error) { return r.storage, nil },
		Grace:       func(context.Context) (time.Duration, error) { return 48 * time.Hour, nil }})
	if err != nil {
		r.t.Fatal(err)
	}
	return e
}

// gone returns the keys of before that are no longer on the rig's storage.
func (r *rig) gone(before []string) []string {
	r.t.Helper()
	after := r.storedKeys()
	var out []string
	for _, k := range before {
		if !slices.Contains(after, k) {
			out = append(out, k)
		}
	}
	return out
}

// TestRestoreUndoesADeleteUndoneAfterItsBackup guards a delete committed in
// the backup and undone after it: the undo's record, written only once the
// database's undo committed, proves it, so the restore undoes the row as an
// undo does (undone, not unresolved, naming nothing an arrival is held
// for), the subject is in the rebuilt catalog, and the purge pass after the
// grace period removes nothing, on each database type. Kept committed, the
// purge would remove data the tenant took the delete of back.
func TestRestoreUndoesADeleteUndoneAfterItsBackup(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		r.archive(studyFiles...)
		all := r.subjects(r.p)
		e := r.engine()
		c := r.deleteSubject(e, all[0])
		path, _ := r.backup()
		if _, err := e.Undo(r.ctx, c.ID, "deleted by mistake", r.audit(), nil); err != nil {
			t.Fatal(err)
		}
		before := r.storedKeys()
		rep, err := r.restoreFresh(path)
		if err != nil {
			t.Fatalf("the restore failed: %v", err)
		}
		got, err := r.p.Correction(r.ctx, c.ID)
		if err != nil || got.State != record.StateUndone || got.Unresolved || got.RecordsState != record.StateUndone {
			t.Fatalf("after the restore the delete is %s, unresolved %v, records %s (%v); want undone", got.State, got.Unresolved,
				got.RecordsState, err)
		}
		if res := rep.Tenants[0]; res.Moved != 1 || res.Disagreed != 0 {
			t.Errorf("the tenant's reconcile is %+v", res)
		}
		if subs := r.subjects(r.p); !slices.Equal(subs, all) {
			t.Fatalf("the restored catalog holds %v, want every subject %v", subs, all)
		}
		if match, err := r.p.DeletedMatches(r.ctx, tenant.DeletedProbe{Subject: all[0], StudyUID: "1.2.3"}); err != nil || match.Subject != "" {
			t.Errorf("the undone delete still names its subject: %+v (%v)", match, err)
		}
		later := r.engineAt(func() time.Time { return time.Now().Add(72 * time.Hour) })
		if _, err := later.PurgeDue(r.ctx); err != nil {
			t.Fatal(err)
		}
		if lost := r.gone(before); len(lost) > 0 {
			t.Fatalf("the purge pass after the grace removed %d objects of a delete the tenant undid: %v", len(lost), lost)
		}
	})
}

// TestRestoreMovesAReplaceWithWhatItsPublishWrites guards a replace the
// backup holds prepared and the restore moves to committed: it gets what
// its publish writes, so its quarantined file is released, the revision it
// superseded resolves as superseded, and an admin's purge removes that
// revision's object from the storage, on each database type.
func TestRestoreMovesAReplaceWithWhatItsPublishWrites(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		r.archive(studyFiles...)
		req, _ := r.replaceRequest()
		oldKey, err := layout.InstanceKey(req.Path, req.UID)
		if err != nil {
			t.Fatal(err)
		}
		var path string
		e := r.engineOver(beforeCommit{Corrections: r.p, step: func() error {
			path, _ = r.backup()
			return nil
		}})
		c, err := e.Replace(r.ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.restoreFresh(path); err != nil {
			t.Fatalf("the restore failed: %v", err)
		}
		if err := r.engine().Recover(r.ctx); err != nil {
			t.Fatal(err)
		}
		if q, err := r.p.QuarantinedFile(r.ctx, req.Quarantine); err != nil || q.State != tenant.QuarantineReleased {
			t.Errorf("the replace's quarantined file is %s (%v), want released", q.State, err)
		}
		res, err := r.p.ResolveInstances(r.ctx, []tenant.InstanceRef{{UID: req.UID, Revision: 1}})
		if err != nil || len(res) != 1 || res[0].Disposition != tenant.ResolvedSuperseded {
			t.Errorf("revision 1 resolves %+v (%v), want superseded", res, err)
		}
		got, err := r.engine().PurgeNow(r.ctx, c.ID, r.audit(), nil)
		if err != nil || got.State != record.StatePurged {
			t.Fatalf("the purge is %s (%v), want purged", got.State, err)
		}
		if slices.Contains(r.storedKeys(), oldKey) {
			t.Errorf("the purged replace left the revision it superseded, %s, on the storage", oldKey)
		}
	})
}

// TestRestoreMovesADeleteAnUndoShowsAgain guards a delete the backup holds
// prepared and the restore moves to committed: it hides what the backup's
// catalog holds of its subject as its publish does, so the rebuilt catalog
// leaves the subject out and an undo after the restore puts it back, on
// each database type.
func TestRestoreMovesADeleteAnUndoShowsAgain(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		r.archive(studyFiles...)
		all := r.subjects(r.p)
		var path string
		e := r.engineOver(beforeCommit{Corrections: r.p, step: func() error {
			path, _ = r.backup()
			return nil
		}})
		del := r.deleteSubject(e, all[0])
		if _, err := r.restoreFresh(path); err != nil {
			t.Fatalf("the restore failed: %v", err)
		}
		if d, err := r.p.Correction(r.ctx, del.ID); err != nil || d.State != record.StateCommitted || !d.Unresolved {
			t.Fatalf("after the restore the delete is %s, unresolved %v (%v); want committed, unresolved", d.State, d.Unresolved, err)
		}
		if subs := r.subjects(r.p); slices.Contains(subs, all[0]) {
			t.Fatalf("after the restore the catalog holds the deleted subject: %v", subs)
		}
		if err := r.engine().Recover(r.ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := r.engine().Undo(r.ctx, del.ID, "deleted by mistake", r.audit(), nil); err != nil {
			t.Fatalf("the moved delete cannot be undone: %v", err)
		}
		if subs := r.subjects(r.p); !slices.Equal(subs, all) {
			t.Fatalf("after the undo the catalog holds %v, want every subject %v", subs, all)
		}
	})
}

// TestRestoreAdoptsADeleteAnUndoShowsAgain guards a delete made after the
// backup, which the restore adopts from its record alone: it hides what the
// backup's catalog holds of its subject as its publish does, so an undo
// after the restore puts the subject back, on each database type.
func TestRestoreAdoptsADeleteAnUndoShowsAgain(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		r.archive(studyFiles...)
		all := r.subjects(r.p)
		path, _ := r.backup()
		del := r.deleteSubject(r.engine(), all[0])
		if _, err := r.restoreFresh(path); err != nil {
			t.Fatalf("the restore failed: %v", err)
		}
		if subs := r.subjects(r.p); slices.Contains(subs, all[0]) {
			t.Fatalf("after the restore the catalog holds the deleted subject: %v", subs)
		}
		if _, err := r.engine().Undo(r.ctx, del.ID, "deleted by mistake", r.audit(), nil); err != nil {
			t.Fatalf("the adopted delete cannot be undone: %v", err)
		}
		if subs := r.subjects(r.p); !slices.Equal(subs, all) {
			t.Fatalf("after the undo the catalog holds %v, want every subject %v", subs, all)
		}
	})
}
