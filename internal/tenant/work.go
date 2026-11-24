package tenant

import (
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// WorkState is where a unit of work is. Its text is what a store keeps.
type WorkState string

// The states.
const (
	// WorkQueued is a unit waiting until it is due.
	WorkQueued WorkState = "queued"
	// WorkLeased is a unit a worker holds until its lease ends.
	WorkLeased WorkState = "leased"
	// WorkDone is a unit finished.
	WorkDone WorkState = "done"
	// WorkFailed is a unit whose attempts ran out, held for a person.
	WorkFailed WorkState = "failed"
)

// DefaultWorkAttempts is the attempts a unit is allowed when it states none.
const DefaultWorkAttempts = 10

// FailureWorkGaveUp is the failure a reception is given when its unit of
// work is held as failed: its attempts ran out, or the lease of its last
// attempt ended with no worker left.
const FailureWorkGaveUp = "its work gave up after its last attempt"

// Work is a unit of work in a tenant's database.
type Work struct {
	// ID is the unit's id, given by the caller.
	ID string
	// Kind says what sort of work it is, such as a reception's step; a
	// claim names the kind it takes. Under the rule of a tenant's name.
	Kind string
	// Subject is what it works on, for a unit that belongs to no reception.
	Subject string
	// ReceptionID names the reception the unit carries through its steps,
	// empty for a unit of no reception. When the unit is held as failed,
	// its reception takes the Fail step in the same transaction, so a
	// reception never outlives the last worker that could end it.
	ReceptionID string
	// UniqueKey, when not empty, queues the unit once: no second unit with
	// the key is queued while one is queued or leased.
	UniqueKey string
	// State is where it is. Answered by a store.
	State WorkState
	// Due is when it may be claimed while queued, and when its lease ends
	// while leased.
	Due time.Time
	// Attempt counts the claims so far; it is the fence of every write of
	// the worker that holds the unit. Answered by a store.
	Attempt int
	// MaxAttempts is how many claims it is allowed before it is held as
	// failed; 0 means DefaultWorkAttempts.
	MaxAttempts int
	// Owner names the worker holding the lease. Answered by a store.
	Owner string
	// LastError is the reason its last attempt failed. Answered by a
	// store.
	LastError string
	// Created and Updated are when it was queued and last changed.
	Created, Updated time.Time
}

// Validate returns an Invalid error unless w can be queued.
func (w Work) Validate() error {
	if err := master.ValidateID("work id", w.ID); err != nil {
		return err
	}
	if err := ValidateName("work kind", w.Kind); err != nil {
		return err
	}
	if err := validateText("work subject", w.Subject, 0, MaxWorkTextLength); err != nil {
		return err
	}
	if err := validateText("work unique key", w.UniqueKey, 0, MaxWorkTextLength); err != nil {
		return err
	}
	if w.ReceptionID != "" {
		if err := master.ValidateID("reception id", w.ReceptionID); err != nil {
			return err
		}
	}
	if w.MaxAttempts < 0 || w.MaxAttempts > MaxWorkAttempts {
		return invalid("a unit of work is allowed 1 to 100 attempts, or 0 for the default")
	}
	return master.ValidateTime("due time", w.Due)
}

// WorkFence is what a worker's write is fenced by: the unit, and the attempt
// whose lease the worker holds. A claim that takes the unit over makes a new
// attempt, and every write with the old one changes nothing.
type WorkFence struct {
	// ID names the unit.
	ID string
	// Attempt is the attempt the worker's claim returned.
	Attempt int
}

// Fence returns the fence of the attempt w is at.
func (w Work) Fence() WorkFence { return WorkFence{ID: w.ID, Attempt: w.Attempt} }

// Validate returns an Invalid error unless f is well formed.
func (f WorkFence) Validate() error {
	if err := master.ValidateID("work id", f.ID); err != nil {
		return err
	}
	if f.Attempt < 1 {
		return invalid("a fence's attempt is at least 1")
	}
	return nil
}

// ValidateWorkReason returns an Invalid error unless reason, why an attempt
// failed, is 1 to MaxWorkTextLength bytes of UTF-8 with no NUL byte.
func ValidateWorkReason(reason string) error {
	return validateText("failed attempt's reason", reason, 1, MaxWorkTextLength)
}

// ValidateLease returns an Invalid error unless lease is MinLease to
// MaxLease and now is a time a store keeps.
func ValidateLease(now time.Time, lease time.Duration) error {
	if lease < MinLease || lease > MaxLease {
		return invalid("a lease is 1 second to 1 hour")
	}
	return master.ValidateTime("time", now)
}

// The delays between attempts: the first retry waits RetryBase, each next
// one twice the last, up to RetryMax.
const (
	RetryBase = 10 * time.Second
	RetryMax  = time.Hour
)

// RetryDelay is how long a unit waits after its attempt failed before it is
// due again: RetryBase doubled for each attempt before, at most RetryMax. It
// grows so that a storage that does not answer is not asked again at once,
// and has no random part so that every driver queues a retry at the same
// time; workers are few, so retries of many units do not arrive together.
func RetryDelay(attempt int) time.Duration {
	d := RetryBase
	for i := 1; i < attempt && d < RetryMax; i++ {
		d *= 2
	}
	return min(d, RetryMax)
}
