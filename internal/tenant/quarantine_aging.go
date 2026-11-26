package tenant

import "time"

// QuarantineAgingChanged is what a tenant holds in quarantine passing one of
// its warnings (quarantine.warn-age, quarantine.warn-bytes), or no longer
// being past one, the contract's QuarantineAging message: one event for
// each change of which warnings stand, never one per look. The server
// reports it (AppendEvents), since it is a reading of the held files over
// time, not a change of one.
type QuarantineAgingChanged struct {
	// OldestAgeMicros is how long the oldest file held had been in
	// quarantine then, in microseconds; zero when none was held.
	OldestAgeMicros int64 `json:"oldest_age_us"`
	// HeldFiles and HeldBytes are the files held then and the bytes kept
	// for them.
	HeldFiles int64 `json:"held_files"`
	// HeldBytes is the bytes kept for the files held.
	HeldBytes int64 `json:"held_bytes"`
	// AgePassed says the age warning stands after this change.
	AgePassed bool `json:"age_passed"`
	// BytesPassed says the bytes warning stands after this change.
	BytesPassed bool `json:"bytes_passed"`
	// Recovered says no warning stands any more.
	Recovered bool `json:"recovered"`
}

// EventType implements EventPayload.
func (*QuarantineAgingChanged) EventType() EventType { return EventQuarantineAging }

// validate implements EventPayload.
func (p *QuarantineAgingChanged) validate() error {
	if p.OldestAgeMicros < 0 || p.HeldFiles < 0 || p.HeldBytes < 0 {
		return invalid("the age, files and bytes held in quarantine are not negative")
	}
	if p.Recovered && (p.AgePassed || p.BytesPassed) {
		return invalid("a recovery event has no warning standing")
	}
	return nil
}

// QuarantineWarning is what a tenant's quarantine amounts to against its
// two warning settings at one time.
type QuarantineWarning struct {
	// OldestAge is how long the oldest file held has been in quarantine;
	// zero when none is held, or when the clock reads before it.
	OldestAge time.Duration
	// Files and Bytes are the files held and the bytes kept for them.
	Files, Bytes int64
	// AgePassed is OldestAge past quarantine.warn-age.
	AgePassed bool
	// BytesPassed is Bytes past quarantine.warn-bytes; never set when that
	// is 0, which is no byte warning.
	BytesPassed bool
}

// Standing reports whether any warning stands.
func (w QuarantineWarning) Standing() bool { return w.AgePassed || w.BytesPassed }

// Warning measures a at now against warnAge and warnBytes (0 for no byte
// warning). One function answers both a status and the check that writes the
// events, so they never disagree about what is past.
func (a QuarantineAging) Warning(now time.Time, warnAge time.Duration, warnBytes int64) QuarantineWarning {
	w := QuarantineWarning{Files: a.Files, Bytes: a.Bytes}
	if !a.Oldest.IsZero() && now.After(a.Oldest) {
		w.OldestAge = now.Sub(a.Oldest)
	}
	w.AgePassed = w.OldestAge > warnAge
	w.BytesPassed = warnBytes > 0 && a.Bytes > warnBytes
	return w
}
