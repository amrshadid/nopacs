package tenant

import (
	"errors"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
)

// TestRetentionPeriodAfter guards a period's end: years then days in UTC,
// and a year added to February 29 ending on March 1, never early.
func TestRetentionPeriodAfter(t *testing.T) {
	leap := time.Date(2024, 2, 29, 10, 0, 0, 0, time.UTC)
	if got := (RetentionPeriod{Years: 1}).After(leap); !got.Equal(time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("a year after February 29 ends %v, want March 1", got)
	}
	if got := (RetentionPeriod{Years: 1, Days: 2}).After(leap); !got.Equal(time.Date(2025, 3, 3, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("a year and two days after February 29 ends %v", got)
	}
	for _, bad := range []RetentionPeriod{{}, {Years: 101}, {Days: 366}, {Years: 100, Days: 1}, {Years: -1, Days: 2}} {
		if bad.Validate() == nil {
			t.Errorf("the period %+v is taken", bad)
		}
	}
	if (RetentionPeriod{Years: 1}).Compare(RetentionPeriod{Days: 365}) != 1 {
		t.Error("a year compares no longer than 365 days")
	}
}

// TestRetentionTimesOf guards what rules give a session: a keep matches
// when any scan came through its selector, a dispose only when all did;
// the earliest dispose names the rule (by name on a tie), its action is
// delete only when every matching dispose says delete, and the session is
// due no sooner than it is kept nor than its floor.
func TestRetentionTimesOf(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a := ScanArrival{Receiver: "r1", Route: "ct", Storage: "s1"}
	b := ScanArrival{Receiver: "r2", Storage: "s1"}
	rules := []RetentionRule{
		{Name: "z", DisposeAfter: RetentionPeriod{Days: 10}, Action: RetentionDelete},
		{Name: "y", Selector: RetentionSelector{Receiver: "r1", Route: "ct"}, DisposeAfter: RetentionPeriod{Days: 10}, Action: RetentionDelete},
		{Name: "k", Selector: RetentionSelector{Receiver: "r2"}, KeepAtLeast: RetentionPeriod{Days: 20}, Action: RetentionReview},
		{Name: "s", Selector: RetentionSelector{Storage: "s1"}, DisposeAfter: RetentionPeriod{Days: 30}, Action: RetentionReview},
	}
	got := RetentionTimesOf(rules, start, time.Time{}, []ScanArrival{a})
	if got.Rule != "y" || !got.Due.Equal(start.AddDate(0, 0, 10)) || got.Action != RetentionReview || !got.KeepUntil.IsZero() {
		t.Errorf("a session through r1 gets %+v, want due under y (a tie by name) for review (s says review)", got)
	}
	got = RetentionTimesOf(rules, start, time.Time{}, []ScanArrival{a, b})
	if got.Rule != "z" || !got.KeepUntil.Equal(start.AddDate(0, 0, 20)) || !got.Due.Equal(got.KeepUntil) {
		t.Errorf("a session through r1 and r2 gets %+v, want kept 20 days by k and due then under z", got)
	}
	floor := start.AddDate(1, 0, 0)
	if got := RetentionTimesOf(rules, start, floor, []ScanArrival{a}); !got.Due.Equal(floor) {
		t.Errorf("a session with a floor is due %v, want the floor %v", got.Due, floor)
	}
	if got := RetentionTimesOf(rules, start, time.Time{}, nil); got != (RetentionTimes{}) {
		t.Errorf("a session with no scan gets %+v", got)
	}
}

// TestClampRetention guards the notice: sooner is clamped to the change
// plus the notice and never later than before; later and longer apply at
// once; a session's first times are not clamped.
func TestClampRetention(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	notice := 30 * 24 * time.Hour
	old := RetentionTimes{Due: now.AddDate(1, 0, 0), KeepUntil: now.AddDate(0, 6, 0), Rule: "r"}
	raw := RetentionTimes{Due: now.Add(-time.Hour), Rule: "r"}
	got, clamped := ClampRetention(old, raw, true, now, notice)
	if !clamped || !got.KeepUntil.Equal(now.Add(notice)) || !got.Due.Equal(now.Add(notice)) {
		t.Errorf("a shortened rule stores %+v (clamped %v), want due and kept until %v", got, clamped, now.Add(notice))
	}
	near := RetentionTimes{Due: now.Add(time.Hour), Rule: "r"}
	if got, _ := ClampRetention(near, raw, true, now, notice); !got.Due.Equal(near.Due) {
		t.Errorf("a clamp moved a due time later than before: %v", got.Due)
	}
	later := RetentionTimes{Due: now.AddDate(2, 0, 0), KeepUntil: now.AddDate(1, 0, 0), Rule: "r"}
	if got, clamped := ClampRetention(old, later, true, now, notice); clamped || !got.Due.Equal(later.Due) {
		t.Errorf("a lengthened rule stores %+v (clamped %v)", got, clamped)
	}
	if got, clamped := ClampRetention(RetentionTimes{}, raw, false, now, notice); clamped || got != raw {
		t.Errorf("a session's first times are clamped to %+v", got)
	}
	if got, clamped := ClampRetention(RetentionTimes{}, raw, true, now, notice); !clamped || !got.Due.Equal(now.Add(notice)) {
		t.Errorf("a rule added that makes a session due at once stores %+v", got)
	}
}

// TestHoldTargetCovers guards what a hold covers: the whole tenant
// everything; a subject, session or scan every path that agrees with it at
// every level both name, compared case folded.
func TestHoldTargetCovers(t *testing.T) {
	scan := HoldTarget{Subject: "sub-01", Session: "ses-1", Scan: "t1"}
	for _, c := range []struct {
		p    label.Path
		want bool
	}{
		{label.Path{Subject: "SUB-01"}, true},
		{label.Path{Subject: "sub-01", Session: "ses-1"}, true},
		{label.Path{Subject: "sub-01", Session: "ses-1", Scan: "t1"}, true},
		{label.Path{Subject: "sub-01", Session: "ses-1", Scan: "t2"}, false},
		{label.Path{Subject: "sub-01", Session: "ses-2"}, false},
		{label.Path{Subject: "sub-02"}, false},
	} {
		if got := scan.Covers(c.p); got != c.want {
			t.Errorf("a scan's hold covers %+v: %v, want %v", c.p, got, c.want)
		}
	}
	if !(HoldTarget{WholeTenant: true}).Covers(label.Path{Subject: "x"}) {
		t.Error("a whole tenant's hold covers nothing")
	}
}

// TestCheckRetentionDelete guards retention's decision on a delete: a
// person's is refused by the later of the stored keep and the keep the
// rules give now, unless it overrides; a disposal's is refused while
// paused, unless due both as stored and as the rules give now, and, for
// the daily pass, when a person's undo exempts the session.
func TestCheckRetentionDelete(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	stored, rules := now.Add(time.Hour), now.Add(2*time.Hour)
	person := RetentionDeleteCheck{Now: now, Sessions: []SessionKept{{Found: true, Stored: RetentionTimes{KeepUntil: stored},
		Rules: RetentionTimes{KeepUntil: rules}}}}
	var kept *RetentionMinimum
	if err := CheckRetentionDelete(person); !errors.As(err, &kept) || !kept.KeepUntil.Equal(rules) {
		t.Errorf("a person's delete of a kept session answers %v, want RETENTION_MINIMUM until %v", err, rules)
	}
	person.Override = true
	if err := CheckRetentionDelete(person); err != nil {
		t.Errorf("an overriding delete answers %v", err)
	}
	due := RetentionTimes{Due: now.Add(-time.Hour), Rule: "d"}
	o := &RetentionOrigin{Rule: "d", Scheduled: true}
	disposal := RetentionDeleteCheck{Origin: o, Now: now, Effective: now, Sessions: []SessionKept{{Found: true, Stored: due, Rules: due}}}
	if err := CheckRetentionDelete(disposal); err != nil {
		t.Errorf("a due session's disposal answers %v", err)
	}
	for _, c := range []struct {
		what string
		edit func(*RetentionDeleteCheck)
		want error
	}{
		{"paused", func(c *RetentionDeleteCheck) { c.Paused = true }, ErrRetentionPaused},
		{"no row", func(c *RetentionDeleteCheck) { c.Sessions[0].Found = false }, ErrNotDue},
		{"due later as stored", func(c *RetentionDeleteCheck) { c.Sessions[0].Stored.Due = now.Add(time.Hour) }, ErrNotDue},
		{"kept by the rules now", func(c *RetentionDeleteCheck) { c.Sessions[0].Rules.KeepUntil = now.Add(time.Hour) }, ErrNotDue},
		{"under another rule now", func(c *RetentionDeleteCheck) { c.Sessions[0].Rules.Rule = "e" }, ErrNotDue},
		{"exempt from the pass", func(c *RetentionDeleteCheck) { c.Sessions[0].Exempt = "d" }, ErrNotDue},
	} {
		d := disposal
		d.Sessions = []SessionKept{disposal.Sessions[0]}
		c.edit(&d)
		if err := CheckRetentionDelete(d); !errors.Is(err, c.want) {
			t.Errorf("a disposal %s answers %v, want %v", c.what, err, c.want)
		}
	}
}
