package backup

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// passJudge passes every new revision an edit writes: these tests are of
// what a restore takes from an edit's records, not of an edit's judging.
type passJudge struct{}

// JudgeEdit passes the file.
func (passJudge) JudgeEdit(context.Context, correct.EditedFile) error { return nil }

// editEngineOver returns a corrections engine over the rig's storage whose
// database is store, which makes edits, with a grace period of two days.
func (r *rig) editEngineOver(store tenant.Corrections) *correct.Engine {
	r.t.Helper()
	e, err := correct.New(correct.Config{Tenant: "lab", TenantID: tenantID, Keys: r.keys, Store: store, Judge: passJudge{},
		OpenStorage: func(context.Context, string) (blob.Store, error) { return r.storage, nil },
		Grace:       func(context.Context) (time.Duration, error) { return 48 * time.Hour, nil }})
	if err != nil {
		r.t.Fatal(err)
	}
	return e
}

// editsBeforeCommit is beforeCommit over a database that keeps edits.
type editsBeforeCommit struct {
	beforeCommit
	tenant.Edits
}

// editScan returns the labels of a scan of at least two instances and
// those instances.
func (r *rig) editScan() (label.Path, []tenant.ScanInstance) {
	r.t.Helper()
	page, err := r.p.ListScans(r.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 1000})
	if err != nil {
		r.t.Fatal(err)
	}
	for _, s := range page.Items {
		if s.Instances >= 2 {
			p := label.Path{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label}
			ins, _, err := r.p.ListHeldInstances(r.ctx, p, master.Page{Limit: 1000})
			if err != nil {
				r.t.Fatal(err)
			}
			return p, ins
		}
	}
	r.t.Fatal("no scan holds two instances")
	return label.Path{}, nil
}

// editNow previews an edit of the scan at p's series description and
// makes it with the preview's token.
func (r *rig) editNow(e *correct.Engine, p label.Path) tenant.Correction {
	r.t.Helper()
	target := tenant.CorrectionTarget{Subject: p.Subject, Session: p.Session, Scan: p.Scan}
	changes := []correct.EditChange{{Tag: 0x0008103E, Value: "Scout view"}}
	pv, err := e.PreviewEdit(r.ctx, correct.EditPreviewRequest{Target: target, Changes: changes})
	if err != nil {
		r.t.Fatal(err)
	}
	c, err := e.Edit(r.ctx, correct.EditRequest{Target: target, Changes: changes, Reason: "series named wrongly", Confirm: pv.Confirm,
		Audit: r.audit()})
	if err != nil || c.State != record.StateCommitted {
		r.t.Fatalf("the edit is %s (%q), %v", c.State, c.StateReason, err)
	}
	return c
}

// wantEditOwned fails unless, after a restore, the edit is committed and
// unresolved, each revision it superseded resolves as superseded, an
// admin's purge removes those revisions' objects, and the new revisions
// stay.
func (r *rig) wantEditOwned(id string, p label.Path, before []tenant.ScanInstance) {
	r.t.Helper()
	if err := r.engine().Recover(r.ctx); err != nil {
		r.t.Fatal(err)
	}
	if got, err := r.p.Correction(r.ctx, id); err != nil || got.State != record.StateCommitted || !got.Unresolved {
		r.t.Fatalf("after the restore the edit is %s, unresolved %v (%v); want committed, unresolved", got.State, got.Unresolved, err)
	}
	for _, in := range before {
		res, err := r.p.ResolveInstances(r.ctx, []tenant.InstanceRef{{UID: in.SOPInstanceUID, Revision: 1}})
		if err != nil || len(res) != 1 || res[0].Disposition != tenant.ResolvedSuperseded {
			r.t.Fatalf("revision 1 of an edited instance resolves %+v (%v), want superseded", res, err)
		}
	}
	got, err := r.editEngineOver(r.p).PurgeNow(r.ctx, id, r.audit(), nil)
	if err != nil || got.State != record.StatePurged {
		r.t.Fatalf("the purge is %s (%v), want purged", got.State, err)
	}
	keys := r.storedKeys()
	for _, in := range before {
		if slices.Contains(keys, in.Key) {
			r.t.Errorf("the purged edit left the revision it superseded, %s, on the storage", in.Key)
		}
		if k, _ := layout.RevisionKey(p, in.SOPInstanceUID, 2); !slices.Contains(keys, k) {
			r.t.Errorf("the purge removed the edit's new revision %s", k)
		}
	}
}

// TestRestoreAdoptsAnEditWithWhatItsPublishWrites guards an edit made
// after the backup, which the restore adopts from its record alone, as a
// replace is adopted: it gets what its publish writes, so each revision it
// superseded resolves as superseded and an admin's purge removes those
// revisions' objects, on each database type. Without it the edit would own
// nothing and its purge would leave every superseded revision behind.
func TestRestoreAdoptsAnEditWithWhatItsPublishWrites(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		r.archive(studyFiles...)
		p, before := r.editScan()
		path, _ := r.backup()
		c := r.editNow(r.editEngineOver(r.p), p)
		if _, err := r.restoreFresh(path); err != nil {
			t.Fatalf("the restore failed: %v", err)
		}
		r.wantEditOwned(c.ID, p, before)
	})
}

// TestRestoreMovesAnEditWithWhatItsPublishWrites guards an edit the backup
// holds prepared and the restore moves to committed, as a replace is
// moved: it gets what its publish writes from what its prepare recorded,
// so each revision it superseded resolves as superseded and an admin's
// purge removes those revisions' objects, on each database type.
func TestRestoreMovesAnEditWithWhatItsPublishWrites(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		r.archive(studyFiles...)
		p, before := r.editScan()
		var path string
		e := r.editEngineOver(editsBeforeCommit{beforeCommit: beforeCommit{Corrections: r.p, step: func() error {
			path, _ = r.backup()
			return nil
		}}, Edits: r.p.(tenant.Edits)})
		c := r.editNow(e, p)
		if _, err := r.restoreFresh(path); err != nil {
			t.Fatalf("the restore failed: %v", err)
		}
		r.wantEditOwned(c.ID, p, before)
	})
}
