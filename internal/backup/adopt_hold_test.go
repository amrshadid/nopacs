package backup

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// instancesHeld returns how many instances the catalog's scans hold.
func (r *rig) instancesHeld() int64 {
	r.t.Helper()
	page, err := r.p.ListScans(r.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 1000})
	if err != nil {
		r.t.Fatal(err)
	}
	n := int64(0)
	for _, s := range page.Items {
		n += s.Instances
	}
	return n
}

// levelTarget returns a delete's target at level: a scan of a session of
// two or more scans, or an instance of a scan of two or more.
func (r *rig) levelTarget(level string) tenant.CorrectionTarget {
	r.t.Helper()
	page, err := r.p.ListScans(r.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 1000})
	if err != nil {
		r.t.Fatal(err)
	}
	per := map[[2]string]int{}
	for _, s := range page.Items {
		per[[2]string{s.SubjectLabel, s.SessionLabel}]++
	}
	for _, s := range page.Items {
		switch {
		case level == record.LevelScan && per[[2]string{s.SubjectLabel, s.SessionLabel}] >= 2:
			return tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label}
		case level == record.LevelInstance && s.Instances >= 2:
			p := label.Path{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label}
			held, _, err := r.p.ListHeldInstances(r.ctx, p, master.Page{Limit: 10})
			if err != nil {
				r.t.Fatal(err)
			}
			return tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label, Instance: held[0].SOPInstanceUID}
		}
	}
	r.t.Fatalf("the corpus gives no %s target", level)
	return tenant.CorrectionTarget{}
}

// keysUnder returns the keys on the rig's storage that name a subject.
func (r *rig) keysUnder(subject string) []string {
	r.t.Helper()
	var out []string
	for _, k := range r.storedKeys() {
		if strings.Contains(k, subject) {
			out = append(out, k)
		}
	}
	return out
}

// TestRestoreAdoptedDeleteAtEachLevelIsUndoneWhole guards a scan delete and
// an instance delete made after the backup and adopted from their records:
// the rebuilt catalog leaves their data out, and an undo after the restore
// puts back every instance, on each database type.
func TestRestoreAdoptedDeleteAtEachLevelIsUndoneWhole(t *testing.T) {
	for _, level := range []string{record.LevelScan, record.LevelInstance} {
		t.Run(level, func(t *testing.T) {
			onBoth(t, func(t *testing.T, pg bool) {
				r := newRig(t, pg)
				r.archive(studyFiles...)
				want := r.instancesHeld()
				target := r.levelTarget(level)
				path, _ := r.backup()
				c := r.deleteNow(r.engine(), target)
				hidden := r.instancesHeld()
				if _, err := r.restoreFresh(path); err != nil {
					t.Fatalf("the restore failed: %v", err)
				}
				if n := r.instancesHeld(); n != hidden {
					t.Fatalf("after the restore the catalog holds %d instances, want %d", n, hidden)
				}
				if _, err := r.engine().Undo(r.ctx, c.ID, "deleted by mistake", r.audit(), nil); err != nil {
					t.Fatalf("the adopted delete cannot be undone: %v", err)
				}
				if n := r.instancesHeld(); n != want {
					t.Fatalf("after the undo the catalog holds %d instances, want %d", n, want)
				}
			})
		})
	}
}

// TestRestoreUndoesADeleteAtEachLevelUndoneAfterItsBackup guards a scan
// delete and an instance delete committed in the backup and undone after
// it: the rebuilt catalog holds every instance, and neither the start nor
// the purge pass after the grace removes anything, on each database type.
func TestRestoreUndoesADeleteAtEachLevelUndoneAfterItsBackup(t *testing.T) {
	for _, level := range []string{record.LevelScan, record.LevelInstance} {
		t.Run(level, func(t *testing.T) {
			onBoth(t, func(t *testing.T, pg bool) {
				r := newRig(t, pg)
				r.archive(studyFiles...)
				want := r.instancesHeld()
				e := r.engine()
				c := r.deleteNow(e, r.levelTarget(level))
				path, _ := r.backup()
				if _, err := e.Undo(r.ctx, c.ID, "deleted by mistake", r.audit(), nil); err != nil {
					t.Fatal(err)
				}
				before := r.storedKeys()
				if _, err := r.restoreFresh(path); err != nil {
					t.Fatalf("the restore failed: %v", err)
				}
				if n := r.instancesHeld(); n != want {
					t.Fatalf("after the restore the catalog holds %d instances, want %d", n, want)
				}
				if err := r.engine().Recover(r.ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := r.engineAt(func() time.Time { return time.Now().Add(72 * time.Hour) }).PurgeDue(r.ctx); err != nil {
					t.Fatal(err)
				}
				if lost := r.gone(before); len(lost) > 0 {
					t.Fatalf("objects of a delete the tenant undid were removed: %v", lost)
				}
			})
		})
	}
}

// TestRestorePurgesAnAdoptedOrMovedDeleteWhole guards what the purge of a
// delete a restore made committed from its record removes: a delete made
// after the backup (adopted) and one the backup holds prepared (moved),
// each purged by an admin after the restore, leave nothing of their
// subject on the storage, its scans' manifest and metadata included, as a
// purge of a delete never restored leaves, on each database type.
func TestRestorePurgesAnAdoptedOrMovedDeleteWhole(t *testing.T) {
	for _, way := range []string{"adopted", "moved"} {
		t.Run(way, func(t *testing.T) {
			onBoth(t, func(t *testing.T, pg bool) {
				r := newRig(t, pg)
				r.archive(studyFiles...)
				all := r.subjects(r.p)
				var path string
				var c tenant.Correction
				if way == "adopted" {
					path, _ = r.backup()
					c = r.deleteSubject(r.engine(), all[0])
				} else {
					e := r.engineOver(beforeCommit{Corrections: r.p, step: func() error {
						path, _ = r.backup()
						return nil
					}})
					c = r.deleteSubject(e, all[0])
				}
				if _, err := r.restoreFresh(path); err != nil {
					t.Fatalf("the restore failed: %v", err)
				}
				if err := r.engine().Recover(r.ctx); err != nil {
					t.Fatal(err)
				}
				got, err := r.engine().PurgeNow(r.ctx, c.ID, r.audit(), nil)
				if err != nil || got.State != record.StatePurged {
					t.Fatalf("the purge is %s (%v), want purged", got.State, err)
				}
				if left := r.keysUnder(all[0]); len(left) > 0 {
					t.Fatalf("the purge left under the subject: %v", left)
				}
			})
		})
	}
}

// TestRestoreTakesADeleteWhoseRecordStayedPreparedAsCommitted guards a
// delete the backup holds prepared, published after it and answered, whose
// record's rewrite to committed failed, so the record still says prepared:
// the restore takes it as committed and unresolved, as a record with no row
// is read, and after the start its subject stays hidden, on each database
// type.
func TestRestoreTakesADeleteWhoseRecordStayedPreparedAsCommitted(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		r.archive(studyFiles...)
		all := r.subjects(r.p)
		var path string
		e := r.engineOver(beforeCommit{Corrections: r.p, step: func() error {
			path, _ = r.backup()
			return nil
		}})
		c := r.deleteSubject(e, all[0])
		r.forgeRecord(c.ID, func(rc *record.Correction) {
			rc.State, rc.CommittedAt, rc.PurgeAfter = record.StatePrepared, time.Time{}, time.Time{}
		})
		if _, err := r.restoreFresh(path); err != nil {
			t.Fatalf("the restore failed: %v", err)
		}
		if err := r.engine().Recover(r.ctx); err != nil {
			t.Fatal(err)
		}
		got, err := r.p.Correction(r.ctx, c.ID)
		if err != nil || got.State != record.StateCommitted || !got.Unresolved {
			t.Fatalf("after the restore and the start the delete is %s, unresolved %v (%v); want committed, unresolved", got.State,
				got.Unresolved, err)
		}
		if subs := r.subjects(r.p); slices.Contains(subs, all[0]) {
			t.Fatalf("the subject a delete withdrew is shown after the restore: %v", subs)
		}
	})
}

// TestRestoreUndoesAnAdoptedDeleteUndoneAfterASecondBackup guards a delete
// a first restore adopted from its record (unresolved), backed up again,
// then undone: a second restore from that backup undoes it as its record
// says, and every subject is shown, on each database type.
func TestRestoreUndoesAnAdoptedDeleteUndoneAfterASecondBackup(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		r.archive(studyFiles...)
		all := r.subjects(r.p)
		path, _ := r.backup()
		c := r.deleteSubject(r.engine(), all[0])
		if _, err := r.restoreFresh(path); err != nil {
			t.Fatalf("the first restore failed: %v", err)
		}
		if err := r.engine().Recover(r.ctx); err != nil {
			t.Fatal(err)
		}
		second, _ := r.backup()
		if _, err := r.engine().Undo(r.ctx, c.ID, "deleted by mistake", r.audit(), nil); err != nil {
			t.Fatal(err)
		}
		rep, err := r.restoreFresh(second)
		if err != nil {
			t.Fatalf("the second restore failed: %v", err)
		}
		got, err := r.p.Correction(r.ctx, c.ID)
		if err != nil || got.State != record.StateUndone || got.Unresolved || rep.Tenants[0].Moved != 1 {
			t.Fatalf("after the second restore the delete is %s, unresolved %v (%v), the reconcile %+v; want undone", got.State,
				got.Unresolved, err, rep.Tenants[0])
		}
		if subs := r.subjects(r.p); !slices.Equal(subs, all) {
			t.Fatalf("after the second restore the catalog holds %v, want every subject %v", subs, all)
		}
	})
}

// TestRestoreHoldsEveryCommittedDeleteItKeeps guards a delete the backup
// holds committed, undone after it with its record's rewrite failed (the
// record still says committed): the restore cannot tell it from one not
// undone, so it keeps it committed and unresolved, its state reason saying
// why, counts and names it in the report, and neither the start nor the
// purge pass after the grace removes anything, on each database type.
func TestRestoreHoldsEveryCommittedDeleteItKeeps(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		r.archive(studyFiles...)
		all := r.subjects(r.p)
		e := r.engine()
		c := r.deleteSubject(e, all[0])
		path, _ := r.backup()
		var committed []byte
		r.rewriteRecord(c.ID, func(rc record.Correction) []byte {
			b, err := record.EncodeCorrection(rc, tenantID, r.keys)
			if err != nil {
				t.Fatal(err)
			}
			committed = b
			return b
		})
		if _, err := e.Undo(r.ctx, c.ID, "deleted by mistake", r.audit(), nil); err != nil {
			t.Fatal(err)
		}
		r.rewriteRecord(c.ID, func(record.Correction) []byte { return committed })
		before := r.storedKeys()
		rep, err := r.restoreFresh(path)
		if err != nil {
			t.Fatalf("the restore failed: %v", err)
		}
		got, err := r.p.Correction(r.ctx, c.ID)
		if err != nil || got.State != record.StateCommitted || !got.Unresolved || got.StateReason != tenant.StateReasonRestoreHeld {
			t.Fatalf("after the restore the delete is %s, unresolved %v, %q (%v); want committed, unresolved, held", got.State,
				got.Unresolved, got.StateReason, err)
		}
		var out strings.Builder
		if err := rep.Print(&out); err != nil || rep.Tenants[0].Held != 1 ||
			!strings.Contains(out.String(), "1 deletes the backup holds committed are kept, each unresolved") ||
			!strings.Contains(out.String(), "nopacs correction purge") {
			t.Errorf("the report (held %d) reads %q (%v)", rep.Tenants[0].Held, out.String(), err)
		}
		if err := r.engine().Recover(r.ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := r.engineAt(func() time.Time { return time.Now().Add(72 * time.Hour) }).PurgeDue(r.ctx); err != nil {
			t.Fatal(err)
		}
		if lost := r.gone(before); len(lost) > 0 {
			t.Fatalf("the purge pass removed %d objects of a delete the restore keeps for an admin", len(lost))
		}
	})
}

// TestRestoreAdoptsAReplaceWithWhatItsPublishWrites guards a replace made
// after the backup, which the restore adopts from its record alone: it gets
// what its publish writes, from the record and the storage, so its
// quarantined file is released, the revision it superseded resolves as
// superseded, and an admin's purge removes that revision's object, on each
// database type.
func TestRestoreAdoptsAReplaceWithWhatItsPublishWrites(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		r.archive(studyFiles...)
		req, _ := r.replaceRequest()
		oldKey, err := layout.InstanceKey(req.Path, req.UID)
		if err != nil {
			t.Fatal(err)
		}
		path, _ := r.backup()
		c, err := r.engine().Replace(r.ctx, req)
		if err != nil || c.State != record.StateCommitted {
			t.Fatalf("the replace is %s (%v)", c.State, err)
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
