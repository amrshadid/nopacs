package backup

import (
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// TestRestoreKeepsRetentionAndHolds guards that what the tenant's database
// keeps of retention comes through a backup and its restore, on each
// database type: the rules as stored, each hold as it stands with the
// storages of its record, each session's retention row with its start
// (never a DICOM date, and never the restore's time); and that the
// restore's rebuild, which builds no retention state, leaves the hold
// active, so a delete of what it covers is still refused.
func TestRestoreKeepsRetentionAndHolds(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		ctx := r.ctx
		r.archive(studyFiles...)
		sessions, err := r.p.ListSessions(ctx, tenant.SessionFilter{Order: tenant.OrderNewest}, master.Page{Limit: 1000})
		if err != nil || len(sessions.Items) == 0 {
			t.Fatalf("the sessions archived are %+v (%v)", sessions.Items, err)
		}
		s := sessions.Items[0]
		now := r.clock.Now().UTC().Truncate(time.Microsecond)
		if _, err := r.p.AddRetentionRule(ctx, tenant.RetentionRule{Name: "trial7", KeepAtLeast: tenant.RetentionPeriod{Years: 10},
			Action: tenant.RetentionReview}, tenant.RetentionChange{Now: now, Notice: tenant.DefaultShortenNotice}, r.audit(), nil); err != nil {
			t.Fatal(err)
		}
		holdID, err := tenant.NewCorrectionID(now)
		if err != nil {
			t.Fatal(err)
		}
		hold, _, err := r.p.PlaceHold(ctx, tenant.HoldPlace{ID: holdID, Target: tenant.HoldTarget{Subject: s.SubjectLabel}, Reason: "litigation",
			Written: []string{s.Storage}, Now: now}, r.audit(), nil)
		if err != nil {
			t.Fatal(err)
		}
		rules, _, err := r.p.ListRetentionRules(ctx, now, master.Page{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		times, err := r.p.SessionRetention(ctx, s.StudyUID)
		if err != nil {
			t.Fatal(err)
		}
		path, _ := r.backup()
		_ = r.st.Close()
		_ = r.m.Close()
		target := filepath.Join(filepath.Dir(r.dataDir), "restored")
		cfg := RestoreConfig{File: path, Keys: r.keys, KeyFile: r.keyPath, DataDir: target,
			MasterAddress: driver.SQLiteScheme + filepath.Join(target, "master.db")}
		if pg {
			r.emptyPostgres()
			cfg.MasterAddress = r.masterAddr
			cfg.Scratch, _ = testkit.PostgresScratch(t)
		}
		rep, err := Restore(ctx, cfg)
		if err != nil {
			t.Fatalf("the restore failed: %v", err)
		}
		if len(rep.Tenants) != 1 || !rep.Tenants[0].Rebuild.Rebuild.Activated {
			t.Fatalf("the restore's report is %+v", rep)
		}
		r.dataDir, r.masterAddr = target, cfg.MasterAddress
		r.openMaster()
		r.openTenant()
		gotRules, _, err := r.p.ListRetentionRules(ctx, now, master.Page{Limit: 10})
		if err != nil || !reflect.DeepEqual(gotRules, rules) {
			t.Fatalf("the restored rules are %+v (%v), want %+v", gotRules, err, rules)
		}
		gotHold, err := r.p.Hold(ctx, hold.ID)
		if err != nil || !reflect.DeepEqual(gotHold, hold) {
			t.Fatalf("the restored hold is %+v (%v), want %+v", gotHold, err, hold)
		}
		gotTimes, err := r.p.SessionRetention(ctx, s.StudyUID)
		if err != nil || gotTimes != times {
			t.Fatalf("the restored session's retention is %+v (%v), want %+v", gotTimes, err, times)
		}
		p, err := r.p.PreviewDelete(ctx, tenant.CorrectionTarget{Subject: s.SubjectLabel}, r.clock.Now().UTC())
		if err != nil || !slices.Equal(p.Holds, []string{hold.ID}) {
			t.Fatalf("after the restore a delete's preview names the holds %v (%v), want %s", p.Holds, err, hold.ID)
		}
	})
}
