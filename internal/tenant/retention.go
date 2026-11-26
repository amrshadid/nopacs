package tenant

import (
	"context"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// Retention is what the tenant's database keeps of its retention rules: the
// rules, the times each session is kept until and due at, and the clock
// those times are read against.
//
// # Rules and what they match
//
// A rule has a name, a selector (the whole tenant, or the receiver, a route
// of it, or the primary storage a scan was archived through, never a
// replica: CheckRetentionSelector), a keep-at-least period and a
// dispose-after period, either or both, and an action, review or delete. A
// session matches a rule's keep-at-least when any of its scans came
// through the selector, and its dispose-after only when all of them did,
// so the gentler reading wins (RetentionTimesOf).
//
// # Times stored per session
//
// Each session has a row keyed by its Study Instance UID, never an integer
// id, so a rebuild that makes the catalog's ids again leaves it standing:
// its start (when it was first archived, by the server's clock, or the
// retention start of the import job that archived it), its keep-until, its
// due time, the rule it is due under and that rule's action. The times are
// stored in the transaction of every change that moves them (a rule added,
// changed or removed; a new scan of the session; a delete published or
// undone; a rebuild's activation), so a listing reads them and computes
// nothing. A change that would make a session due sooner, or keep it less,
// is clamped: no sooner than the change's time plus the notice, and never
// later than before (ClampRetention). A session's start is never a DICOM
// date.
//
// # The clock
//
// The store keeps the latest time it trusted the server's clock. A check
// that finds the clock moved forward by more than the step allowed pauses
// retention: until ConfirmClock, nothing becomes due (every listing reads
// at the last trusted time), and no automatic purge starts
// (Corrections.CheckPurgeClock). A step back pauses nothing and leaves the
// time trusted where it was, never earlier: due times are then read at the
// clock's earlier reading, so going back only delays what is due, and the
// next forward step is measured from the latest time trusted. Every time
// stored while the clock reads earlier than the time trusted (a session's
// start, a commit's or a change's clamp, a rule's change time) is taken at
// the time trusted (RetentionClock.StoreTime), so a clock set back starts
// nothing early. A clock run fast in small steps is caught by the drift
// the caller measures against its monotonic clock
// (RetentionClockCheck.Drift), and an automatic purge's check pauses
// retention too when it sees a jump past the step a check stored. Every
// time comes from the caller; the store never reads a clock of its own.
//
// # Rule changes beside commits
//
// A change of rules and a commit that stores a session's times are
// ordered, one after the other: whichever comes second reads what the
// first committed, so a keep-at-least a change adds is never lost for a
// session a commit beside it makes.
type Retention interface {
	// AddRetentionRule adds rule r (CheckRetentionSelector, r.Validate)
	// and stores again the times of every session in the same transaction
	// with its audit entry, clamped by ch. A name the tenant has is a
	// Conflict.
	AddRetentionRule(ctx context.Context, r RetentionRule, ch RetentionChange, audit master.AuditEntry, guard Guard) (RetentionRule, error)
	// SetRetentionRule replaces the selector, periods and action of the
	// rule r names with r's, and stores the times again as
	// AddRetentionRule does. A rule that would stand as it stands changes
	// nothing (its update time kept); its audit entry is written all the
	// same. Any change ends the exemptions a person's undo gave under it.
	SetRetentionRule(ctx context.Context, r RetentionRule, ch RetentionChange, audit master.AuditEntry, guard Guard) (RetentionRule, error)
	// RemoveRetentionRule removes the rule named at once, storing the
	// times again clamped by ch, and answers it as it was. A name the
	// tenant does not have is NotFound.
	RemoveRetentionRule(ctx context.Context, name string, ch RetentionChange, audit master.AuditEntry, guard Guard) (RetentionRule, error)
	// RetentionRule answers the rule named, with the sessions due under it
	// at now, held ones included; NotFound when there is none.
	RetentionRule(ctx context.Context, name string, now time.Time) (RetentionRule, error)
	// ListRetentionRules answers a page of the rules by name, each with
	// its sessions due at now.
	ListRetentionRules(ctx context.Context, now time.Time, page master.Page) ([]RetentionRule, string, error)
	// DueSessions answers a page of the sessions due at f.Now (read at the
	// last trusted time while retention is paused), oldest due first, then
	// by Study Instance UID; held ones only with f.IncludeHeld, marked.
	DueSessions(ctx context.Context, f DueFilter, page master.Page) ([]DueSession, string, error)
	// SessionRetention answers the row of the session of the study, or
	// NotFound when it has none.
	SessionRetention(ctx context.Context, studyUID string) (SessionRetention, error)
	// CheckRetentionClock checks the server's clock against the last time
	// the store trusted it (RetentionClockCheck says how) and answers where
	// retention stands after.
	CheckRetentionClock(ctx context.Context, c RetentionClockCheck) (RetentionClock, error)
	// ConfirmClock ends a pause: the time a person read and typed is
	// within MaxClockDisagreement of c.Now (a precondition failure
	// otherwise), and c.Now becomes the time trusted, for retention and for
	// the purges of corrections, in one transaction with its audit entry.
	// It answers where retention stands and whether it was paused.
	ConfirmClock(ctx context.Context, c ClockConfirm, audit master.AuditEntry, guard Guard) (RetentionClock, bool, error)
	// RetentionClock answers where retention stands, changing nothing.
	RetentionClock(ctx context.Context) (RetentionClock, error)
	// QuarantineAging answers the oldest quarantined file held and the
	// files and bytes held, from an index and the running totals.
	QuarantineAging(ctx context.Context) (QuarantineAging, error)
}

// The bounds of retention.
const (
	// DefaultShortenNotice is retention.shorten-notice when nothing sets
	// it: a mistyped rule cannot make a year's data due tonight.
	DefaultShortenNotice = 30 * 24 * time.Hour
	// MinShortenNotice and MaxShortenNotice bound the notice.
	MinShortenNotice = 24 * time.Hour
	MaxShortenNotice = 365 * 24 * time.Hour
	// DefaultMaxClockStep is retention.max-clock-step when nothing sets
	// it: two days lets a server stop over a weekend without an admin.
	DefaultMaxClockStep = 48 * time.Hour
	// MinMaxClockStep and MaxMaxClockStep bound the step.
	MinMaxClockStep = time.Hour
	MaxMaxClockStep = 30 * 24 * time.Hour
	// MaxClockDisagreement is how far the time a person typed may be from
	// the server's clock for ConfirmClock to take it.
	MaxClockDisagreement = 5 * time.Minute
	// MaxRetentionReason bounds a person's reason, as a correction's is.
	MaxRetentionReason = 1024
)

// PauseClockJumped is the reason a pause gives: the clock moved forward
// too far.
const PauseClockJumped = "clock jumped"

// RetentionChange is the time of a change of rules and the notice it is
// clamped by.
type RetentionChange struct {
	// Now is when the change is made.
	Now time.Time
	// Notice is retention.shorten-notice now.
	Notice time.Duration
}

// Validate returns an Invalid error unless c is well formed.
func (c RetentionChange) Validate() error {
	if c.Notice < MinShortenNotice || c.Notice > MaxShortenNotice {
		return invalid("retention's notice is 1 to 365 days")
	}
	return master.ValidateTime("change time", c.Now)
}

// DueFilter keeps the due sessions a listing answers.
type DueFilter struct {
	// Rule keeps those due under the rule of this name; empty for all.
	Rule string
	// IncludeHeld lists held sessions too, marked held.
	IncludeHeld bool
	// Now is the time they are due at.
	Now time.Time
}

// Validate returns an Invalid error unless f is well formed.
func (f DueFilter) Validate() error {
	if f.Rule != "" {
		if err := ValidateName("rule", f.Rule); err != nil {
			return err
		}
	}
	return master.ValidateTime("time", f.Now)
}

// DueSession is one session due under a rule.
type DueSession struct {
	// StudyUID names the session; SubjectLabel and SessionLabel are its
	// labels now.
	StudyUID, SubjectLabel, SessionLabel string
	// Rule names the rule it is due under; Action is that rule's, delete
	// only when every rule whose dispose-after matches it says delete.
	Rule   string
	Action RetentionAction
	// Start is its start time; Due its due time stored.
	Start, Due time.Time
	// Held says an active hold covers it; Hold names one.
	Held bool
	Hold string
	// Instances and Bytes are what it holds.
	Instances, Bytes int64
	// Undone says a person undid a retention delete of it under its rule,
	// so the daily pass leaves it alone until a person disposes of it or
	// the rule changes.
	Undone bool
}

// SessionRetention is the retention row of one session.
type SessionRetention struct {
	// StudyUID names the session.
	StudyUID string
	// Start is its start time; Archived when it was first archived.
	Start, Archived time.Time
	// Floor is the time it is never due before (an import's retention
	// start: archived time plus the notice), zero for none.
	Floor time.Time
	// KeepUntil is the latest time a rule keeps it until, zero for none.
	KeepUntil time.Time
	// Due is its due time, zero when no rule makes it due or its session
	// is hidden by a delete.
	Due time.Time
	// Rule and Action are those of its due time.
	Rule   string
	Action RetentionAction
	// ExemptRule names the rule a person's undo of a retention delete
	// exempts it from, empty for none.
	ExemptRule string
}

// RetentionClockCheck is one check of the server's clock.
type RetentionClockCheck struct {
	// Now is the server's clock.
	Now time.Time
	// MaxStep is retention.max-clock-step; Notice retention.shorten-notice,
	// kept for the times a commit stores.
	MaxStep, Notice time.Duration
	// Pass says the check is the daily pass's, whose time it records when
	// retention is not paused.
	Pass bool
	// Drift is how far the server's clock moved beyond the time the
	// process measured by its monotonic clock, since the first check it
	// trusted after its start or after the last pause: past MaxStep the
	// check pauses, so a clock run fast in steps each within MaxStep is
	// caught. Zero for a caller that measures nothing.
	Drift time.Duration
}

// Validate returns an Invalid error unless c is well formed.
func (c RetentionClockCheck) Validate() error {
	if c.MaxStep < MinMaxClockStep || c.MaxStep > MaxMaxClockStep {
		return invalid("retention's clock step is 1 hour to 30 days")
	}
	if c.Notice < MinShortenNotice || c.Notice > MaxShortenNotice {
		return invalid("retention's notice is 1 to 365 days")
	}
	return master.ValidateTime("time", c.Now)
}

// ClockConfirm is a person's confirmation of the server's time.
type ClockConfirm struct {
	// Observed is the time the person read from a clock they trust.
	Observed time.Time
	// Now is the server's clock.
	Now time.Time
	// Reason is the person's reason (1 to 1024 bytes, no control
	// character).
	Reason string
}

// Validate returns an Invalid error unless c is well formed.
func (c ClockConfirm) Validate() error {
	if err := validateCorrectionReason("reason", c.Reason); err != nil {
		return err
	}
	if c.Observed.IsZero() {
		return invalid("a confirmation of the clock carries the time read")
	}
	return master.ValidateTime("time", c.Now)
}

// ValidateStudyUID returns an Invalid error unless s is a Study Instance
// UID (PS3.5 9.1).
func ValidateStudyUID(s string) error { return validateUID("Study Instance UID", s) }

// CheckClockAgreement returns a precondition failure unless the time typed
// is within MaxClockDisagreement of the server's: a server whose time is
// wrong, or a misread, is not confirmed.
func CheckClockAgreement(observed, now time.Time) error {
	d := now.Sub(observed)
	if d < 0 {
		d = -d
	}
	if d > MaxClockDisagreement {
		return Precondition("the time given differs from the server's clock by more than 5 minutes; it is not confirmed")
	}
	return nil
}

// RetentionClock is where a tenant's retention clock stands.
type RetentionClock struct {
	// Trusted is the latest time the clock was trusted; zero before the
	// first check.
	Trusted time.Time
	// LastPass is when the daily pass last checked; zero before the first.
	LastPass time.Time
	// Paused says retention waits for ConfirmClock; Reason is
	// PauseClockJumped; Step how far forward the clock moved; PausedAt the
	// clock's reading that paused it.
	Paused   bool
	Reason   string
	Step     time.Duration
	PausedAt time.Time
	// JustPaused says this check paused it: the caller writes its event
	// once.
	JustPaused bool
	// MaxStep and Notice are the settings the last check gave.
	MaxStep, Notice time.Duration
}

// Effective returns the time due times are read at for now: the last
// trusted time while paused, and never more than the step allowed past it,
// so nothing becomes due across a jump not yet checked.
func (c RetentionClock) Effective(now time.Time) time.Time {
	switch {
	case c.Trusted.IsZero():
		return now
	case c.Paused:
		return c.Trusted
	case now.Before(c.Trusted):
		return now
	case c.MaxStep > 0 && now.Sub(c.Trusted) > c.MaxStep:
		return c.Trusted.Add(c.MaxStep)
	}
	return now
}

// StoreTime returns the time a change at now stores retention times at (a
// session's start, the time a change is clamped from, a rule's change
// time): now, or the latest time trusted when the clock reads earlier. A
// clock set back would otherwise start a session early, and a session
// started two years back by a clock two years behind is due at once under
// a one-year rule once the clock is put right.
func (c RetentionClock) StoreTime(now time.Time) time.Time {
	if now.Before(c.Trusted) {
		return c.Trusted
	}
	return now
}

// QuarantineAging is what a status warns of the quarantine by.
type QuarantineAging struct {
	// Oldest is when the oldest file held was quarantined, zero when none
	// is held.
	Oldest time.Time
	// Files and Bytes are the files held and the bytes kept for them.
	Files, Bytes int64
}

// The refusals retention and holds make, each a case inside the
// precondition kind.
var (
	// ErrHeld is in the chain of a delete refused because an active hold
	// covers what it would remove (HeldRefusal names the hold).
	ErrHeld = errors.New("a hold covers the data")
	// ErrRetentionMinimum is in the chain of a person's delete refused
	// because a rule keeps the data at least until a time to come
	// (RetentionMinimum names the time).
	ErrRetentionMinimum = errors.New("a retention rule keeps the data")
	// ErrNotDue is in the chain of a retention delete of a session that is
	// not due under the rule, or that a person's undo exempts.
	ErrNotDue = errors.New("the session is not due under the rule")
	// ErrRetentionPaused is in the chain of a disposal refused while the
	// tenant's retention is paused for its clock.
	ErrRetentionPaused = errors.New("the tenant's retention is paused for its clock")
)

// HeldRefusal is the refusal of a delete an active hold covers.
type HeldRefusal struct {
	// Hold is the id of a hold that covers it.
	Hold string
}

// Error implements error.
func (h *HeldRefusal) Error() string { return "a hold covers the data (" + h.Hold + ")" }

// Is reports ErrHeld.
func (h *HeldRefusal) Is(target error) bool { return target == ErrHeld }

// HoldRefusalError returns the precondition failure of a delete the hold
// of that id covers.
func HoldRefusalError(hold string) error {
	return fault.Wrap(fault.Precondition, &HeldRefusal{Hold: hold}, "a hold covers the data the delete would remove (HELD)")
}

// RetentionMinimum is the refusal of a person's delete a rule keeps.
type RetentionMinimum struct {
	// KeepUntil is the latest time a rule keeps any of the data until.
	KeepUntil time.Time
}

// Error implements error.
func (r *RetentionMinimum) Error() string {
	return "a retention rule keeps the data until " + r.KeepUntil.UTC().Format(time.RFC3339)
}

// Is reports ErrRetentionMinimum.
func (r *RetentionMinimum) Is(target error) bool { return target == ErrRetentionMinimum }

// RetentionMinimumError returns the precondition failure of a person's
// delete that a rule keeps until keepUntil.
func RetentionMinimumError(keepUntil time.Time) error {
	return fault.Wrap(fault.Precondition, &RetentionMinimum{KeepUntil: keepUntil},
		"a retention rule keeps the data at least until "+keepUntil.UTC().Format(time.RFC3339)+
			"; a delete needs override_retention with its own reason (RETENTION_MINIMUM)")
}

// NotDueError returns the precondition failure of a retention delete of a
// session that is not due, saying why.
func NotDueError(why string) error { return fault.Wrap(fault.Precondition, ErrNotDue, why) }

// RetentionPausedError returns the precondition failure of a disposal
// while retention is paused.
func RetentionPausedError() error {
	return fault.Wrap(fault.Precondition, ErrRetentionPaused,
		"the tenant's retention is paused because its clock jumped; an admin confirms the time first")
}
