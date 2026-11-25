package tenant

import (
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// The rules of retention (Retention): a rule's bounds, which sessions it
// matches, the times it gives them, and the clamp a change is held to. Every
// store applies them; none is written in a driver.

// RetentionPeriod is a length of time counted from a session's start:
// whole calendar years, then whole days, in UTC. Its zero value is no
// period.
type RetentionPeriod struct {
	// Years is 0 to 100; Days 0 to 365.
	Years, Days int
}

// IsZero reports whether p is no period.
func (p RetentionPeriod) IsZero() bool { return p.Years == 0 && p.Days == 0 }

// Validate returns an Invalid error unless p is a period of 1 day to 100
// years: years 0 to 100, days 0 to 365, not both 0, and days 0 with 100
// years.
func (p RetentionPeriod) Validate() error {
	if p.Years < 0 || p.Years > 100 || p.Days < 0 || p.Days > 365 || p.IsZero() || (p.Years == 100 && p.Days != 0) {
		return invalid("a retention period is 1 day to 100 years: years 0 to 100, days 0 to 365")
	}
	return nil
}

// After returns the end of p counted from start: the years added first,
// then the days. A year added to February 29 ends on March 1, the later
// day (time.Time.AddDate normalizes so), so a period never ends early.
func (p RetentionPeriod) After(start time.Time) time.Time {
	return start.UTC().AddDate(p.Years, 0, p.Days)
}

// Compare returns -1, 0 or 1 as p is shorter than, as long as, or longer
// than q: by years, then by days. With days at most 365, the one that
// compares longer ends no earlier from every start.
func (p RetentionPeriod) Compare(q RetentionPeriod) int {
	switch {
	case p.Years != q.Years:
		if p.Years < q.Years {
			return -1
		}
		return 1
	case p.Days < q.Days:
		return -1
	case p.Days > q.Days:
		return 1
	}
	return 0
}

// RetentionAction is what is done with a rule's due sessions.
type RetentionAction string

// The actions.
const (
	// RetentionReview lists the due sessions for a person.
	RetentionReview RetentionAction = "review"
	// RetentionDelete has the daily pass dispose of them.
	RetentionDelete RetentionAction = "delete"
)

// RetentionSelector names the scans a rule matches by what they were
// archived through: all empty for every scan of the tenant, or a receiver
// (and optionally a route of it), or a primary storage.
type RetentionSelector struct {
	Receiver, Route, Storage string
}

// IsZero reports whether s selects the whole tenant.
func (s RetentionSelector) IsZero() bool { return s == RetentionSelector{} }

// Validate returns an Invalid error unless s is well formed.
func (s RetentionSelector) Validate() error {
	if s.Receiver != "" && s.Storage != "" {
		return invalid("a retention selector names a receiver or a storage, not both")
	}
	if s.Route != "" && s.Receiver == "" {
		return invalid("a retention selector's route needs its receiver")
	}
	for _, x := range []struct{ what, name string }{{"receiver", s.Receiver}, {"route", s.Route}, {"storage", s.Storage}} {
		if x.name != "" {
			if err := ValidateName(x.what, x.name); err != nil {
				return err
			}
		}
	}
	return nil
}

// ScanArrival is what one scan of a session was archived through.
type ScanArrival struct {
	// Receiver is the receiver it arrived at; Route the route that chose
	// for its session ("" for none); Storage the primary it was archived
	// to.
	Receiver, Route, Storage string
}

// Matches reports whether s selects a scan that arrived as a.
func (s RetentionSelector) Matches(a ScanArrival) bool {
	switch {
	case s.Storage != "":
		return a.Storage == s.Storage
	case s.Receiver != "":
		return a.Receiver == s.Receiver && (s.Route == "" || a.Route == s.Route)
	}
	return true
}

// RetentionRule is one rule of a tenant.
type RetentionRule struct {
	// Name is unique in the tenant, under the rule of a tenant's name.
	Name string
	// Selector is the scans it matches.
	Selector RetentionSelector
	// KeepAtLeast and DisposeAfter are its periods, zero for none.
	KeepAtLeast, DisposeAfter RetentionPeriod
	// Action is what is done with its due sessions.
	Action RetentionAction
	// Created and Updated are the server's; NoticeUntil is until when the
	// clamp of its last change that made anything due sooner or kept it
	// less holds, zero for none.
	Created, Updated, NoticeUntil time.Time
	// SessionsDue counts the sessions due under it at the time asked,
	// held ones included; answered by reads only.
	SessionsDue int64
}

// Validate returns an Invalid error unless r can be stored.
func (r RetentionRule) Validate() error {
	if err := ValidateName("rule", r.Name); err != nil {
		return err
	}
	if err := r.Selector.Validate(); err != nil {
		return err
	}
	if r.KeepAtLeast.IsZero() && r.DisposeAfter.IsZero() {
		return invalid("a retention rule has a keep-at-least period, a dispose-after period, or both")
	}
	for _, p := range []RetentionPeriod{r.KeepAtLeast, r.DisposeAfter} {
		if !p.IsZero() {
			if err := p.Validate(); err != nil {
				return err
			}
		}
	}
	if !r.KeepAtLeast.IsZero() && !r.DisposeAfter.IsZero() && r.DisposeAfter.Compare(r.KeepAtLeast) < 0 {
		return invalid("a rule's dispose-after is not shorter than its keep-at-least")
	}
	if r.Action != RetentionReview && r.Action != RetentionDelete {
		return invalid("a retention rule's action is review or delete")
	}
	return nil
}

// SameAs reports whether r and o state the same rule: selector, periods and
// action.
func (r RetentionRule) SameAs(o RetentionRule) bool {
	return r.Selector == o.Selector && r.KeepAtLeast == o.KeepAtLeast && r.DisposeAfter == o.DisposeAfter && r.Action == o.Action
}

// SelectorFacts is what CheckRetentionSelector needs of the tenant.
type SelectorFacts struct {
	// ReceiverFound says the tenant has the receiver named; RouteFound the
	// route of it named.
	ReceiverFound, RouteFound bool
	// Storage is the storage named, when StorageFound.
	Storage      Storage
	StorageFound bool
}

// CheckRetentionSelector returns a precondition failure unless the
// selector names what the tenant has: a receiver, a route of it, a storage
// that is a primary. A replica is never a selector's storage, since nothing
// is archived to it first (CheckArchivedTo).
func CheckRetentionSelector(s RetentionSelector, f SelectorFacts) error {
	switch {
	case s.Receiver != "" && !f.ReceiverFound:
		return Precondition("the tenant has no receiver named " + s.Receiver)
	case s.Route != "" && !f.RouteFound:
		return Precondition("the receiver " + s.Receiver + " has no route named " + s.Route)
	case s.Storage != "" && !f.StorageFound:
		return Precondition("the tenant has no storage named " + s.Storage)
	case s.Storage != "" && f.Storage.ReplicaOf != "":
		return Precondition("the storage " + s.Storage + " is a replica; a rule names the primary its scans were archived to")
	}
	return nil
}

// RetentionTimes are the times the rules give one session.
type RetentionTimes struct {
	// KeepUntil is the latest time a rule keeps it until, zero for none.
	KeepUntil time.Time
	// Due is when it is due, zero for never.
	Due time.Time
	// Rule is the rule with the earliest dispose-after that matches it (by
	// name on a tie), "" when Due is zero; Action is delete only when every
	// rule whose dispose-after matches says delete.
	Rule   string
	Action RetentionAction
}

// RetentionTimesOf returns the times rules give a session that started at
// start and whose scans arrived as arrivals: a keep-at-least matches when
// any scan came through its rule's selector, a dispose-after only when all
// did; the latest keep and the earliest dispose apply, and the session is
// due no sooner than its keep. floor, when not zero, is the time it is
// never due before (an import's retention start). A session with no scan
// matches nothing.
func RetentionTimesOf(rules []RetentionRule, start, floor time.Time, arrivals []ScanArrival) RetentionTimes {
	var out RetentionTimes
	if len(arrivals) == 0 {
		return out
	}
	var dispose *RetentionRule
	allDelete := true
	for i := range rules {
		r := &rules[i]
		some, all := false, true
		for _, a := range arrivals {
			if r.Selector.Matches(a) {
				some = true
			} else {
				all = false
			}
		}
		if !r.KeepAtLeast.IsZero() && some {
			if k := r.KeepAtLeast.After(start); k.After(out.KeepUntil) {
				out.KeepUntil = k
			}
		}
		if !r.DisposeAfter.IsZero() && all {
			if r.Action != RetentionDelete {
				allDelete = false
			}
			if dispose == nil || r.DisposeAfter.Compare(dispose.DisposeAfter) < 0 ||
				(r.DisposeAfter.Compare(dispose.DisposeAfter) == 0 && r.Name < dispose.Name) {
				dispose = r
			}
		}
	}
	if dispose == nil {
		return out
	}
	out.Rule, out.Action = dispose.Name, RetentionReview
	if allDelete {
		out.Action = RetentionDelete
	}
	out.Due = latest(dispose.DisposeAfter.After(start), out.KeepUntil, floor)
	return out
}

// latest returns the latest of ts.
func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}

// ClampRetention returns the times a change at now stores for a session
// that had old (had says it had any) and that the rules now give raw: a
// due time sooner than before is no sooner than now + notice and no later
// than before; a keep-until less than before stays at least until now +
// notice, and no later than before. A change that keeps longer, or makes
// due later, takes effect at once. It also reports whether it clamped
// anything, which marks the rule change's notice.
func ClampRetention(old, raw RetentionTimes, had bool, now time.Time, notice time.Duration) (RetentionTimes, bool) {
	if !had {
		return raw, false
	}
	out, clamped := raw, false
	until := now.Add(notice)
	switch {
	case raw.Due.IsZero():
	case old.Due.IsZero():
		// Never due before: due now no sooner than the notice's end.
		if raw.Due.Before(until) {
			out.Due, clamped = until, true
		}
	case raw.Due.Before(old.Due):
		if c := earliest(old.Due, latest(raw.Due, until)); !c.Equal(raw.Due) {
			out.Due, clamped = c, true
		}
	}
	if !old.KeepUntil.IsZero() && raw.KeepUntil.Before(old.KeepUntil) && old.KeepUntil.After(now) {
		if c := latest(raw.KeepUntil, earliest(old.KeepUntil, until)); !c.Equal(raw.KeepUntil) {
			out.KeepUntil, clamped = c, true
		}
	}
	// A session kept longer than it is due is due when it is no longer
	// kept.
	if !out.Due.IsZero() && out.KeepUntil.After(out.Due) {
		out.Due = out.KeepUntil
	}
	return out, clamped
}

// earliest returns the earlier of a and b.
func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// ValidateRetentionStart returns an Invalid error unless start, when not
// zero, is an import's retention start: not before 1970 and not after the
// job is made.
func ValidateRetentionStart(start, created time.Time) error {
	if start.IsZero() {
		return nil
	}
	if start.Before(time.Unix(0, 0)) || start.After(created) {
		return invalid("an import's retention start is not before 1970 and not after the job is made")
	}
	return master.ValidateTime("retention start", start)
}
