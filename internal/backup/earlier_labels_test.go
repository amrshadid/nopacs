package backup

import (
	"testing"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// relabelSubject relabels the subject from to to and waits for the relabel
// to commit, then purges it when purge is set, so its row is purged.
func (r *rig) relabelSubject(e *correct.Engine, from, to string, purge bool) tenant.Correction {
	r.t.Helper()
	c, err := e.Relabel(r.ctx, correct.RelabelRequest{Target: tenant.CorrectionTarget{Subject: from}, ToLabel: to, Reason: "wrong label",
		Audit: r.audit()})
	if err != nil {
		r.t.Fatal(err)
	}
	if c, err = e.ContinueRelabel(r.ctx, c.ID); err != nil || c.State != record.StateCommitted {
		r.t.Fatalf("the relabel is %s (%v), want committed", c.State, err)
	}
	if purge {
		if c, err = e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil || c.State != record.StatePurged {
			r.t.Fatalf("the relabel's purge is %s (%v), want purged", c.State, err)
		}
	}
	return c
}

// heldUnder answers the delete that holds data arriving under the subject
// label, as a reception asks (tenant.Corrections.DeletedMatches).
func (r *rig) heldUnder(subject string) string {
	r.t.Helper()
	m, err := r.p.DeletedMatches(r.ctx, tenant.DeletedProbe{Subject: subject, StudyUID: "1.2.3.4.5.6.7.8.9"})
	if err != nil {
		r.t.Fatal(err)
	}
	return m.Subject
}

// TestRestoreDoesNotHoldALabelGivenAfterTheDelete guards the other side of
// the labels a subject's delete holds: a delete committed in the backup,
// purged after it, and then another subject relabeled to the deleted
// subject's label (a relabel made after the delete, adopted from its
// record). After the restore the delete still holds its own label but not
// the other subject's earlier one: that subject's data was never withdrawn,
// and holding its arrivals would hide data a scanner sends under its old
// label. On each database type.
func TestRestoreDoesNotHoldALabelGivenAfterTheDelete(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		r.archive(studyFiles...)
		all := r.subjects(r.p)
		e := r.engine()
		d := r.deleteSubject(e, all[0])
		path, _ := r.backup()
		if c, err := e.PurgeNow(r.ctx, d.ID, r.audit(), nil); err != nil || c.State != record.StatePurged {
			t.Fatalf("the delete's purge is %s (%v)", c.State, err)
		}
		r.relabelSubject(e, all[1], all[0], false)
		if got := r.heldUnder(all[1]); got != "" {
			t.Fatalf("before the restore the relabeled subject's earlier label is held by %q", got)
		}
		if _, err := r.restoreFresh(path); err != nil {
			t.Fatalf("the restore failed: %v", err)
		}
		if got := r.heldUnder(all[0]); got != d.ID {
			t.Fatalf("after the restore the delete holds its own label as %q, want %q", got, d.ID)
		}
		if got := r.heldUnder(all[1]); got != "" {
			t.Fatalf("after the restore the label %q, given up by a relabel made after the delete, is held by %q", all[1], got)
		}
	})
}

// TestRestoreHoldsASubjectsEarlierLabels guards a subject's delete made
// after the copy a restore reads (a backup, or the site's own copy for a
// reconcile in place) and made again from its record: like the delete
// itself, it holds data arriving under every label the subject had before
// a relabel, not only the one its record names, so a scanner that still
// knows the patient by the old label cannot bring the withdrawn data back.
// The subject is relabeled (and that relabel purged, as a delete of its
// new label needs) before the copy, or after it, so the relabel too is
// made from its record, on each database type.
func TestRestoreHoldsASubjectsEarlierLabels(t *testing.T) {
	for _, way := range []string{"restore", "reconcile", "relabel-after-copy"} {
		t.Run(way, func(t *testing.T) {
			onBoth(t, func(t *testing.T, pg bool) {
				r := newRig(t, pg)
				r.archive(studyFiles...)
				first := r.subjects(r.p)[0]
				e := r.engine()
				var path string
				var cp siteCopy
				switch way {
				case "restore":
					r.relabelSubject(e, first, "renamed-1", true)
					path, _ = r.backup()
				case "reconcile":
					r.relabelSubject(e, first, "renamed-1", true)
					cp = r.takeCopy()
				default:
					path, _ = r.backup()
					r.relabelSubject(e, first, "renamed-1", true)
				}
				d := r.deleteSubject(r.engine(), "renamed-1")
				if got := r.heldUnder(first); got != d.ID {
					t.Fatalf("before the restore the delete holds the earlier label %q as %q, want %q", first, got, d.ID)
				}
				if way == "reconcile" {
					r.putBack(cp)
					if _, err := ReconcileOnly(r.ctx, r.reconcileConfig()); err != nil {
						t.Fatalf("the reconcile failed: %v", err)
					}
					r.openMaster()
					r.openTenant()
				} else if _, err := r.restoreFresh(path); err != nil {
					t.Fatalf("the restore failed: %v", err)
				}
				if got := r.heldUnder("renamed-1"); got != d.ID {
					t.Fatalf("after it the delete holds its own label as %q, want %q", got, d.ID)
				}
				if got := r.heldUnder(first); got != d.ID {
					t.Fatalf("after it the delete holds the earlier label %q as %q, want %q: data arriving under it would be archived again",
						first, got, d.ID)
				}
			})
		})
	}
}
