package backup

import (
	"slices"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestRestoreAdoptsAMerge guards a merge made after the backup, which the
// restore adopts from its record alone: the rebuilt catalog files every
// session of the merged subject under the destination and holds no row of
// the merged subject; the merge is committed and unresolved; a lookup by
// an old label answers where the data is now; and an admin's purge removes
// the old tree and keeps the objects the catalog names, on each database
// type.
func TestRestoreAdoptsAMerge(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		r.archive(studyFiles...)
		page, err := r.p.ListScans(r.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		var subjects []string
		var from label.Path
		for _, s := range page.Items {
			if !slices.Contains(subjects, s.SubjectLabel) {
				subjects = append(subjects, s.SubjectLabel)
			}
		}
		if len(subjects) < 2 {
			t.Fatalf("the corpus gives %d subjects, want two", len(subjects))
		}
		for _, s := range page.Items {
			if s.SubjectLabel == subjects[0] {
				from = label.Path{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label}
			}
		}
		path, _ := r.backup()
		e := r.engine()
		target := tenant.CorrectionTarget{Subject: subjects[0]}
		pv, err := e.PreviewMove(r.ctx, target, subjects[1])
		if err != nil {
			t.Fatal(err)
		}
		c, err := e.Move(r.ctx, correct.MoveRequest{Target: target, To: subjects[1], Reason: "one patient under two ids", Confirm: pv.Confirm,
			Audit: r.audit()})
		if err != nil {
			t.Fatal(err)
		}
		if c, err = e.ContinueRelabel(r.ctx, c.ID); err != nil || c.State != record.StateCommitted {
			t.Fatalf("the merge is %s (%v), want committed", c.State, err)
		}
		if _, err := r.restoreFresh(path); err != nil {
			t.Fatalf("the restore failed: %v", err)
		}
		if err := r.engine().Recover(r.ctx); err != nil {
			t.Fatal(err)
		}
		got, err := r.p.Correction(r.ctx, c.ID)
		if err != nil || got.Kind != record.KindMove || got.State != record.StateCommitted || !got.Unresolved || got.ToLabel != subjects[1] {
			t.Fatalf("after the restore the merge is %+v (%v); want committed, unresolved", got, err)
		}
		if _, err := r.p.CatalogSubject(r.ctx, subjects[0]); err == nil {
			t.Fatal("after the restore the merged subject is in the catalog")
		}
		to := tenant.MovedPath(from, subjects[1])
		if _, err := r.p.CatalogScan(r.ctx, to); err != nil {
			t.Fatalf("after the restore the merged scan is not under the destination: %v", err)
		}
		if p, found, err := r.p.RelabeledTo(r.ctx, from); err != nil || !found || p.To != to {
			t.Fatalf("after the restore a lookup by the old labels answers %+v, %v, %v", p, found, err)
		}
		held, _, err := r.p.ListHeldInstances(r.ctx, to, master.Page{Limit: 1000})
		if err != nil || len(held) == 0 {
			t.Fatalf("the merged scan holds %d instances (%v)", len(held), err)
		}
		if p, err := r.engine().PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil || p.State != record.StatePurged {
			t.Fatalf("the merge's purge is %s (%v), want purged", p.State, err)
		}
		keys := r.storedKeys()
		for _, in := range held {
			old, _ := layout.InstanceKey(from, in.SOPInstanceUID)
			if slices.Contains(keys, old) {
				t.Errorf("the purge left the old object %s", old)
			}
			if !slices.Contains(keys, in.Key) {
				t.Errorf("the purge removed %s, which the catalog names", in.Key)
			}
		}
	})
}
