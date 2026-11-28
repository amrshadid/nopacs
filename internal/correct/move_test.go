package correct

import (
	"errors"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestMoveCopiesKeepsAndUndoes guards a session's move on both databases:
// every object of the session is copied to its key under the destination,
// read back equal; the catalog names the new keys at once; the old tree is
// kept whole; the record says what moved where; the undo files the session
// back on the old tree and removes the copies.
func TestMoveCopiesKeepsAndUndoes(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		m := r.archiveForMove()
		e := r.engine()
		before := r.objects()
		s := r.scanOf(secondSeriesUID)
		from := pathOf(s)
		to := tenant.MovedPath(from, m.to)
		c, err := r.moveNow(e, tenant.CorrectionTarget{Subject: m.from, Session: m.session}, m.to, false)
		if err != nil || c.State != record.StateCommitted {
			t.Fatalf("the move = %+v, %v", c, err)
		}
		after := r.objects()
		r.wantUnchanged(before, after)
		oldKeys, newKeys := scanKeys(t, after, from), scanKeys(t, after, to)
		if len(oldKeys) != int(s.Instances)+2 || len(newKeys) != len(oldKeys) {
			t.Fatalf("after the move %d objects are under the old labels and %d under the new, want %d each", len(oldKeys), len(newKeys),
				s.Instances+2)
		}
		moved := r.scanOf(secondSeriesUID)
		if pathOf(moved) != to {
			t.Fatalf("after the move the scan is at %v, want %v", pathOf(moved), to)
		}
		for _, in := range r.held(to) {
			old := keyUnder(t, in.Key, from)
			if sum, ok := after[in.Key]; !ok || sum != before[old] {
				t.Fatalf("the catalog names %s, which is not the old object %s copied", in.Key, old)
			}
		}
		rec := r.correctionRecord(c.ID)
		if rec.Kind != record.KindMove || rec.Move == nil || rec.Move.Level != record.LevelSession || rec.Move.From != m.from ||
			rec.Move.To != m.to || rec.State != record.StateCommitted || len(rec.Scans) != 1 {
			t.Fatalf("the move's record is %+v", rec)
		}
		u, err := e.Undo(r.ctx, c.ID, "the right patient after all", r.audit(), nil)
		if err != nil || u.State != record.StateUndone || !u.Swept {
			t.Fatalf("the move's undo = %+v, %v", u, err)
		}
		if got := pathOf(r.scanOf(secondSeriesUID)); got != from {
			t.Fatalf("after the undo the scan is at %v, want %v", got, from)
		}
		undone := r.objects()
		r.wantUnchanged(before, undone)
		if left := scanKeys(t, undone, to); len(left) != 0 {
			t.Fatalf("after the undo the copies %v are left", left)
		}
		if got := r.correctionRecord(c.ID).State; got != record.StateUndone {
			t.Fatalf("after the undo the record says %s", got)
		}
	})
}

// TestMovePurgeRemovesTheOldTree guards a move's purge: the old tree goes,
// the scan's objects under the destination stay and are those the catalog
// names, and the catalog a rebuild makes from the storage agrees with the
// one in use in the grace and after the purge.
func TestMovePurgeRemovesTheOldTree(t *testing.T) {
	r := newRig(t, false)
	m := r.archiveForMove()
	e := r.engine()
	from := pathOf(r.scanOf(secondSeriesUID))
	to := tenant.MovedPath(from, m.to)
	c, err := r.moveNow(e, tenant.CorrectionTarget{Subject: m.from, Session: m.session}, m.to, false)
	if err != nil {
		t.Fatal(err)
	}
	wantBothTreesRead(t, r.compare(t))
	p, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil)
	if err != nil || p.State != record.StatePurged {
		t.Fatalf("the move's purge = %+v, %v", p, err)
	}
	objs := r.objects()
	if left := scanKeys(t, objs, from); len(left) != 0 {
		t.Fatalf("after the purge the old tree keeps %v", left)
	}
	for _, in := range r.held(to) {
		if _, ok := objs[in.Key]; !ok {
			t.Fatalf("after the purge the catalog names %s, which is not stored", in.Key)
		}
	}
	r.compare(t)
}

// TestMergeAndUndo guards a merge: every session of the subject filed under
// the destination, the subject gone; a rebuild in its grace makes the same
// catalog; the undo makes the subject again with its sessions on the old
// tree, and a rebuild agrees again.
func TestMergeAndUndo(t *testing.T) {
	r := newRig(t, false)
	m := r.archiveForMove()
	e := r.engine()
	c, err := r.moveNow(e, tenant.CorrectionTarget{Subject: m.from}, m.to, false)
	if err != nil || c.State != record.StateCommitted || c.Level != record.LevelSubject {
		t.Fatalf("the merge = %+v, %v", c, err)
	}
	for _, s := range r.scans() {
		if s.SubjectLabel != m.to {
			t.Fatalf("after the merge a scan is under %s", s.SubjectLabel)
		}
	}
	if _, err := r.p.CatalogSubject(r.ctx, m.from); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("after the merge the merged subject is found: %v", err)
	}
	wantBothTreesRead(t, r.compare(t))
	if _, err := e.Undo(r.ctx, c.ID, "two patients after all", r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	sub, err := r.p.CatalogSubject(r.ctx, m.from)
	if err != nil || sub.Sessions != 2 {
		t.Fatalf("after the undo the merged subject is %+v, %v", sub, err)
	}
	wantBothTreesRead(t, r.compare(t))
}

// wantBothTreesRead fails unless a rebuild's tallies left out no scan as
// a label conflict or as held elsewhere: in a move's grace the storage
// holds its scans under both subjects, and the move's record decides which
// tree is the archive's, so neither is found again as a second copy.
func wantBothTreesRead(t *testing.T, by map[string]int64) {
	t.Helper()
	if n := by[tenant.TallyLeftOutLabelConflict] + by[tenant.TallyLeftOutHeldElsewhere]; n != 0 {
		t.Fatalf("the rebuild left %d scans out as a second copy: %v", n, by)
	}
}

// TestMoveRefusesAStaleToken guards a move's token: one whose preview no
// longer answers the same, because an instance arrived in the session, is
// refused, nothing recorded.
func TestMoveRefusesAStaleToken(t *testing.T) {
	r := newRig(t, false)
	m := r.archiveForMove()
	e := r.engine()
	target := tenant.CorrectionTarget{Subject: m.from, Session: m.session}
	p, err := e.PreviewMove(r.ctx, target, m.to)
	if err != nil {
		t.Fatal(err)
	}
	// Another instance of the session's series arrives.
	extra := secondStudy(t)
	b, err := rewriteUIDs(extra, map[uint32]string{0x00080018: "2.25.100100100100100199"})
	if err != nil {
		t.Fatal(err)
	}
	pl := r.pipeline()
	r.acceptBytes(pl, b)
	r.waitEnded()
	if err := pl.Stop(r.ctx); err != nil {
		t.Fatal(err)
	}
	_, err = e.Move(r.ctx, MoveRequest{Target: target, To: m.to, Reason: "filed under the wrong patient", Confirm: p.Confirm, Audit: r.audit()})
	if fault.KindOf(err) != fault.Precondition {
		t.Fatalf("a move with a stale token = %v, want a precondition failure", err)
	}
	if list, _, err := r.p.ListCorrections(r.ctx, tenant.CorrectionFilter{}, pageAll); err != nil || len(list) != 0 {
		t.Fatalf("a refused move recorded %+v, %v", list, err)
	}
}

// TestMoveRefusesAHeldTarget guards that a move into a subject a hold
// covers is refused as HELD, nothing recorded.
func TestMoveRefusesAHeldTarget(t *testing.T) {
	r := newRig(t, false)
	m := r.archiveForMove()
	e := r.engine()
	if _, _, err := r.p.PlaceHold(r.ctx, tenant.HoldPlace{ID: "01jd5x7k2m3n4p5q6r7s8t9v0w", Target: tenant.HoldTarget{Subject: m.to},
		Reason: "litigation", Written: []string{"main"}, Now: time.Now().UTC().Truncate(time.Microsecond)}, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	_, err := r.moveNow(e, tenant.CorrectionTarget{Subject: m.from, Session: m.session}, m.to, false)
	var held *tenant.HeldRefusal
	if fault.KindOf(err) != fault.Precondition || !errors.As(err, &held) {
		t.Fatalf("a move into a held subject = %v, want HELD", err)
	}
}

// TestMoveCrashes guards that a move left at any step goes on at the
// restart, as a relabel does: copied, published, the session under the
// destination with every object the catalog names stored, its record
// committed.
func TestMoveCrashes(t *testing.T) {
	for _, step := range []string{"prepared", "records", "relabel-object", "copied", "committed"} {
		t.Run(step, func(t *testing.T) {
			r := newRig(t, false)
			m := r.archiveForMove()
			c, _ := crashed(t, r, step, func(e *Engine) (tenant.Correction, error) {
				return r.moveNow(e, tenant.CorrectionTarget{Subject: m.from, Session: m.session}, m.to, false)
			})
			if c.ID == "" {
				list, _, err := r.p.ListCorrections(r.ctx, tenant.CorrectionFilter{}, pageAll)
				if err != nil || len(list) != 1 {
					t.Fatalf("the corrections after the restart are %+v, %v", list, err)
				}
				c = list[0]
			}
			if got := r.state(c.ID); got.State != record.StateCommitted || r.correctionRecord(c.ID).State != record.StateCommitted {
				t.Fatalf("after the restart the move is %s", got.State)
			}
			s := r.scanOf(secondSeriesUID)
			if s.SubjectLabel != m.to {
				t.Fatalf("after the restart the session is under %s", s.SubjectLabel)
			}
			objs := r.objects()
			for _, in := range r.held(pathOf(s)) {
				if _, ok := objs[in.Key]; !ok {
					t.Fatalf("the catalog names %s, which is not stored", in.Key)
				}
			}
		})
	}
}

// TestMoveReachesTheReplica guards a merge on a primary with a replica in
// step: the copies are on the replica before the publish, which keeps its
// rows in step; the purge removes the old tree from both; the undo of a
// second merge removes the copies from both.
func TestMoveReachesTheReplica(t *testing.T) {
	r := newReplicaRig(t)
	m := r.archiveForMove()
	r.attach()
	e := r.engine()
	from := pathOf(r.scanOf(secondSeriesUID))
	to := tenant.MovedPath(from, m.to)
	c, err := r.moveNow(e, tenant.CorrectionTarget{Subject: m.from}, m.to, false)
	if err != nil || c.State != record.StateCommitted {
		t.Fatalf("the merge = %+v, %v", c, err)
	}
	if n := len(scanKeys(t, r.copyObjects(), to)); n != len(scanKeys(t, r.objects(), to)) || n == 0 {
		t.Fatalf("the replica holds %d objects under the destination, the primary %d", n, len(scanKeys(t, r.objects(), to)))
	}
	if rec, ok := r.recordOn(r.copy, c.ID); !ok || rec.Move == nil || rec.State != record.StateCommitted {
		t.Fatalf("the replica's record of the merge is %+v, %v", rec, ok)
	}
	row, err := r.p.ReplicaScan(r.ctx, "copy", secondSeriesUID)
	if err != nil || row.State != tenant.ReplicaInStep {
		t.Fatalf("the replica's row of a merged scan is %+v, %v; want in step", row, err)
	}
	if p, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil || p.State != record.StatePurged {
		t.Fatalf("the merge's purge = %+v, %v", p, err)
	}
	if left := append(scanKeys(t, r.objects(), from), scanKeys(t, r.copyObjects(), from)...); len(left) != 0 {
		t.Fatalf("the old tree keeps %v after the purge", left)
	}
}

// pageAll is a page of every correction the tests make.
var pageAll = master.Page{Limit: 1000}

// keyUnder returns an object's key moved under the scan at p: the same
// name and revision in that scan's tree.
func keyUnder(t testing.TB, key string, p label.Path) string {
	t.Helper()
	k, err := newKey(key, p)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
