package rebuild

import (
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestAReplaceKeepsTheRetentionOfASessionLeftOut guards the retention row
// of a session a replace leaves out: its files gone from the storage, the
// replace activates a catalog without it, and its row stays as it was (its
// start, its clamped times, the exemption a person's undo gave); once its
// files are back, the next replace shows it again with that same row, not
// a row started anew, unclamped and no longer exempt.
func TestAReplaceKeepsTheRetentionOfASessionLeftOut(t *testing.T) {
	drivers(t, func(t *testing.T, postgres bool) {
		r := newRig(t, postgres)
		r.archive(studyFiles...)
		page, err := r.p.ListSessions(r.ctx, tenant.SessionFilter{Order: tenant.OrderNewest}, master.Page{Limit: 1000})
		if err != nil || len(page.Items) < 2 {
			t.Fatalf("the sessions archived are %+v (%v), want two or more", page.Items, err)
		}
		// The server's clock, which the activation reads too.
		now := time.Now().UTC().Truncate(time.Microsecond)
		// A rule added after the sessions were archived makes them due no
		// sooner than the notice's end: their times are clamped.
		if _, err := r.p.AddRetentionRule(r.ctx, tenant.RetentionRule{Name: "short", DisposeAfter: tenant.RetentionPeriod{Days: 1},
			Action: tenant.RetentionDelete}, tenant.RetentionChange{Now: now, Notice: tenant.DefaultShortenNotice}, r.audit(), nil); err != nil {
			t.Fatal(err)
		}
		left := page.Items[0]
		r.catalogExec("UPDATE retention_sessions SET exempt_rule = 'short' WHERE study_uid = '" + left.StudyUID + "'")
		before, err := r.p.SessionRetention(r.ctx, left.StudyUID)
		if err != nil {
			t.Fatal(err)
		}
		if before.ExemptRule != "short" || !before.Due.Equal(now.Add(tenant.DefaultShortenNotice)) {
			t.Fatalf("the session's row before the replace is %+v, want exempt and clamped to the notice's end", before)
		}
		aside := t.TempDir()
		move := func(from, to string) {
			t.Helper()
			for _, root := range []string{layout.DataRoot, layout.MetaRoot} {
				src := filepath.Join(from, root, left.SubjectLabel, left.Label)
				dst := filepath.Join(to, root, left.SubjectLabel, left.Label)
				if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(src, dst); err != nil {
					t.Fatal(err)
				}
			}
		}
		storage := filepath.Join(r.dataDir, "storage")
		move(storage, aside)
		var writes atomic.Int64
		if _, tallies := r.run(r.config(&writes), tenant.RebuildReplace); tallies[tenant.TallyActivated] != 1 {
			t.Fatalf("the replace without the session's files ended with %v", tallies)
		}
		if _, err := r.p.CatalogSession(r.ctx, left.SubjectLabel, left.Label); err == nil {
			t.Fatal("the replace shows the session whose files are gone")
		}
		if got, err := r.p.SessionRetention(r.ctx, left.StudyUID); err != nil || got != before {
			t.Fatalf("after a replace that left the session out its row is %+v (%v), want it kept as %+v", got, err, before)
		}
		move(aside, storage)
		if _, tallies := r.run(r.config(&writes), tenant.RebuildReplace); tallies[tenant.TallyActivated] != 1 {
			t.Fatalf("the replace with the session's files back ended with %v", tallies)
		}
		if got, err := r.p.SessionRetention(r.ctx, left.StudyUID); err != nil || got != before {
			t.Fatalf("after the session came back its row is %+v (%v), want %+v", got, err, before)
		}
	})
}

// TestRebuildLeavesRetention guards that a rebuild neither builds nor
// compares retention state: a compare over a catalog with a rule, a hold and
// each session's retention row tallies nothing; a replace that makes the
// catalog again from the storage alone (its every row, ids included,
// forgotten first) leaves the rules and the hold as they were, and each
// session's retention row, keyed by its Study Instance UID, with the start
// it had, never the rebuild's time.
func TestRebuildLeavesRetention(t *testing.T) {
	drivers(t, func(t *testing.T, postgres bool) {
		r := newRig(t, postgres)
		r.archive(studyFiles...)
		page, err := r.p.ListSessions(r.ctx, tenant.SessionFilter{Order: tenant.OrderNewest}, master.Page{Limit: 1000})
		if err != nil || len(page.Items) == 0 {
			t.Fatalf("the sessions archived are %+v (%v)", page.Items, err)
		}
		now := r.clock.Now().UTC().Truncate(time.Microsecond)
		if _, err := r.p.AddRetentionRule(r.ctx, tenant.RetentionRule{Name: "trial7", KeepAtLeast: tenant.RetentionPeriod{Years: 10},
			DisposeAfter: tenant.RetentionPeriod{Years: 15}, Action: tenant.RetentionReview},
			tenant.RetentionChange{Now: now, Notice: tenant.DefaultShortenNotice}, r.audit(), nil); err != nil {
			t.Fatal(err)
		}
		id, err := tenant.NewCorrectionID(now)
		if err != nil {
			t.Fatal(err)
		}
		s := page.Items[0]
		hold, _, err := r.p.PlaceHold(r.ctx, tenant.HoldPlace{ID: id, Target: tenant.HoldTarget{Subject: s.SubjectLabel}, Reason: "audit",
			Written: []string{s.Storage}, Now: now}, r.audit(), nil)
		if err != nil {
			t.Fatal(err)
		}
		before := map[string]tenant.SessionRetention{}
		for _, x := range page.Items {
			if before[x.StudyUID], err = r.p.SessionRetention(r.ctx, x.StudyUID); err != nil {
				t.Fatal(err)
			}
		}
		var writes atomic.Int64
		j, tallies := r.run(r.config(&writes), tenant.RebuildCompare)
		if j.State != tenant.JobDone || len(tallies) != 0 {
			t.Fatalf("a compare beside retention state ended %+v with %v, want done with nothing", j, tallies)
		}
		r.forgetCatalog()
		j, tallies = r.run(r.config(&writes), tenant.RebuildReplace)
		if tallies[tenant.TallyActivated] != 1 {
			t.Fatalf("the replace ended %+v with %v", j, tallies)
		}
		got, err := r.p.Hold(r.ctx, hold.ID)
		if err != nil || !reflect.DeepEqual(got, hold) {
			t.Fatalf("after the replace the hold is %+v (%v), want %+v", got, err, hold)
		}
		if rule, err := r.p.RetentionRule(r.ctx, "trial7", now); err != nil || rule.KeepAtLeast.Years != 10 {
			t.Fatalf("after the replace the rule is %+v (%v)", rule, err)
		}
		for study, want := range before {
			if got, err := r.p.SessionRetention(r.ctx, study); err != nil || got != want {
				t.Errorf("after the replace the session %s's retention is %+v (%v), want %+v", study, got, err, want)
			}
		}
	})
}
