package tenanttest

import (
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// checkClock checks retention's clock at now with the step allowed,
// failing the test on an error.
func (e *tenv) checkClock(now time.Time, step, drift time.Duration) tenant.RetentionClock {
	e.Helper()
	c, err := e.p.CheckRetentionClock(e.ctx, tenant.RetentionClockCheck{Now: now, MaxStep: step, Notice: notice, Drift: drift})
	e.ok("CheckRetentionClock", err)
	return c
}

// testRetentionClockSetBack guards the times stored while the clock is set
// back: with the clock trusted at t0, a study that arrives while it reads
// two years earlier starts at t0, so once the clock is put right it is not
// due under a one-year rule, and a keep-at-least of a year keeps it a year
// from t0; a rule shortened while the clock is still behind gets its whole
// notice from t0, not a notice that ran out two years ago.
func testRetentionClockSetBack(e *tenv) {
	e.addReceiver("recv", "store", 11112)
	t0 := at(0)
	e.checkClock(t0, tenant.DefaultMaxClockStep, 0)
	yearly := tenant.RetentionRule{Name: "yearly", KeepAtLeast: tenant.RetentionPeriod{Years: 1}, DisposeAfter: tenant.RetentionPeriod{Years: 1},
		Action: tenant.RetentionDelete}
	e.addRule(yearly, changeAt(t0))
	back := t0.AddDate(-2, 0, 0)
	e.archiveScans("recv", 1, []int{100}, 0, back, "ses-1", nil)
	got := e.sessionTimes(1)
	if !got.Start.Equal(t0) || !got.Archived.Equal(t0) {
		e.Fatalf("a session archived while the clock read two years back starts %v (archived %v), want the time trusted %v", got.Start,
			got.Archived, t0)
	}
	if want := t0.AddDate(1, 0, 0); !got.KeepUntil.Equal(want) || !got.Due.Equal(want) {
		e.Fatalf("its one-year keep and dispose end %v and %v, want both %v", got.KeepUntil, got.Due, want)
	}
	right := t0.Add(time.Hour)
	e.checkClock(right, tenant.DefaultMaxClockStep, 0)
	if due := e.due("", right, true); len(due) != 0 {
		e.Fatalf("once the clock is put right the sessions due are %+v, want none", due)
	}
	// Shortened while the clock reads two years back: clamped from the
	// time trusted.
	short := tenant.RetentionRule{Name: "yearly", DisposeAfter: tenant.RetentionPeriod{Days: 1}, Action: tenant.RetentionDelete}
	changed, err := e.p.SetRetentionRule(e.ctx, short, changeAt(back.Add(2*time.Hour)), e.audit(), nil)
	e.ok("SetRetentionRule", err)
	want := right.Add(notice)
	if !changed.Updated.Equal(right) || !changed.NoticeUntil.Equal(want) {
		e.Fatalf("a rule shortened while the clock is behind is %+v, want changed at %v with its notice ending %v", changed, right, want)
	}
	if got := e.sessionTimes(1); !got.KeepUntil.Equal(want) || !got.Due.Equal(want) {
		e.Fatalf("after the rule is shortened while the clock is behind the session is kept until %v and due %v, want both %v",
			got.KeepUntil, got.Due, want)
	}
	if due := e.due("", want.Add(-time.Second), true); len(due) != 0 {
		e.Fatalf("the session is due before the notice from the time trusted ends: %+v", due)
	}
}

// testPurgeClockStep guards the step an automatic purge's clock is
// allowed: the tenant's retention.max-clock-step as the last check of
// retention's clock stored it, not a fixed two days. With the step set to
// an hour, a purge's check 40 hours after the last time trusted is refused,
// and retention is paused by that check at once, without waiting for its
// own next check.
func testPurgeClockStep(e *tenv) {
	t0 := at(0)
	e.checkClock(t0, time.Hour, 0)
	e.ok("CheckPurgeClock", e.p.CheckPurgeClock(e.ctx, t0))
	jump := t0.Add(40 * time.Hour)
	err := e.p.CheckPurgeClock(e.ctx, jump)
	if !errors.Is(err, tenant.ErrClock) {
		e.Fatalf("an automatic purge 40 hours past the time trusted, with a step of an hour, answers %v, want the clock's refusal", err)
	}
	rc, err := e.p.RetentionClock(e.ctx)
	e.ok("RetentionClock", err)
	if !rc.Paused || rc.Reason != tenant.PauseClockJumped || rc.Step != 40*time.Hour || !rc.PausedAt.Equal(jump) {
		e.Fatalf("after the purge's check saw the jump retention is %+v, want paused for a 40-hour jump at %v", rc, jump)
	}
	if again := e.checkClock(jump, time.Hour, 0); !again.Paused || again.JustPaused {
		e.Fatalf("retention's own check after the purge's paused it answers %+v, want paused already", again)
	}
}

// testRetentionClockDrift guards a clock run fast in small steps: a check
// whose step from the time trusted is within the step allowed, but whose
// caller measured the clock ahead of the time that passed by more than
// that step, pauses retention; one whose drift is within it does not.
func testRetentionClockDrift(e *tenv) {
	t0 := at(0)
	e.checkClock(t0, time.Hour, 0)
	if c := e.checkClock(t0.Add(50*time.Minute), time.Hour, 55*time.Minute); c.Paused {
		e.Fatalf("a drift within the step allowed paused retention: %+v", c)
	}
	c := e.checkClock(t0.Add(100*time.Minute), time.Hour, 2*time.Hour)
	if !c.Paused || !c.JustPaused || c.Reason != tenant.PauseClockJumped || c.Step != 2*time.Hour {
		e.Fatalf("a drift of two hours past a step of one answers %+v, want paused for it", c)
	}
}
