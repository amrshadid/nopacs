package servecmd

import (
	"testing"
	"time"
)

// TestAClockRunFastInSmallStepsPausesRetention guards retention's check
// against a clock that runs fast in steps each within
// retention.max-clock-step (two days): the server's clock moves 40 hours
// at each check while the process's monotonic clock moves one. The first
// such check, 39 hours ahead of the time that passed, goes on; the second,
// 78 hours ahead, pauses retention for the clock, though neither step alone
// passed the step allowed.
func TestAClockRunFastInSmallStepsPausesRetention(t *testing.T) {
	r := newWorkRig(t)
	r.w.setEngines(retentionEngines(t, r))
	wall, mono := time.Now(), time.Now()
	r.w.now = func() time.Time { return wall }
	ret := &tenantRetention{w: r.w, name: "lab", st: r.db, base: r.db, log: r.w.log,
		seams: retentionSeams{monotonic: func() time.Time { return mono }}}
	if err := ret.once(r.ctx); err != nil {
		t.Fatalf("the first check: %v", err)
	}
	for i, wantPaused := range []bool{false, true} {
		wall, mono = wall.Add(40*time.Hour), mono.Add(time.Hour)
		if err := ret.once(r.ctx); err != nil {
			t.Fatalf("check %d: %v", i+2, err)
		}
		rc, err := r.db.RetentionClock(r.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if rc.Paused != wantPaused {
			t.Fatalf("after %d steps of 40 hours in %d hours of the process's time retention is %+v, want paused %v", i+1, i+1, rc, wantPaused)
		}
		if wantPaused && rc.Step != 78*time.Hour {
			t.Fatalf("the pause names a step of %v, want the 78 hours the clock ran ahead", rc.Step)
		}
	}
}
