package reception

import (
	"errors"
	"time"
)

// Timing is what Completion decides from, as the reception's record holds
// it.
type Timing struct {
	// Opened is when the reception took its first file.
	Opened time.Time
	// LastFile is when it took its latest file. The zero time, and any
	// time before Opened, counts as Opened: no file is older than the
	// reception's first.
	LastFile time.Time
	// Quiet is how long without a file makes a reception complete.
	Quiet time.Duration
	// MaxOpen is the longest a reception stays open, however often files
	// come: a sender that never stops still has its data archived.
	MaxOpen time.Duration
}

// Reason says why a reception is complete.
type Reason int

// The reasons.
const (
	// NotYet is a reception that is not complete.
	NotYet Reason = iota
	// Quieted is a reception that has had no file for its quiet period.
	Quieted
	// TimedOut is a reception open for its longest time.
	TimedOut
)

// Decision is the answer of Completion.
type Decision struct {
	// Complete reports whether the reception is complete now.
	Complete bool
	// Reason is why, or NotYet.
	Reason Reason
	// Due is the earliest time the reception will be complete if no file
	// comes before then: when to ask again.
	Due time.Time
	// Opened and LastFile are the times the decision counted from. They
	// differ from the Timing given only when ClockBack is set.
	Opened, LastFile time.Time
	// ClockBack reports that now is before Opened or LastFile: the clock
	// was set back since they were taken. The decision then counts from
	// now in their place, so a clock set back by an hour does not hold a
	// reception open for an extra hour, and the caller stores Opened and
	// LastFile so the next decision counts from them too.
	ClockBack bool
}

// Completion decides whether a reception with timing t is complete at now.
// It is quiet when now is at least LastFile + Quiet, timed out when now is
// at least Opened + MaxOpen, and quiet wins when both hold.
//
// Times compare as Go compares them: by the monotonic clock when both carry
// a reading of it (both from time.Now in this process), which no change of
// the wall clock affects, and by the wall clock otherwise, as after a
// restart, when the times come from the database. A clock set back is
// handled as Decision.ClockBack says; a clock set forward cannot be told
// from time passing, and completes a reception early. It refuses a
// non-positive Quiet or MaxOpen.
func Completion(t Timing, now time.Time) (Decision, error) {
	if t.Quiet <= 0 || t.MaxOpen <= 0 {
		return Decision{}, errors.New("reception: the quiet period and the longest open time must be positive")
	}
	d := Decision{Opened: t.Opened, LastFile: t.LastFile}
	if now.Before(d.Opened) {
		d.Opened, d.ClockBack = now, true
	}
	if d.LastFile.IsZero() || d.LastFile.Before(d.Opened) {
		d.LastFile = d.Opened
	}
	if now.Before(d.LastFile) {
		d.LastFile, d.ClockBack = now, true
	}
	quietAt, maxAt := d.LastFile.Add(t.Quiet), d.Opened.Add(t.MaxOpen)
	switch {
	case !now.Before(quietAt):
		d.Complete, d.Reason = true, Quieted
	case !now.Before(maxAt):
		d.Complete, d.Reason = true, TimedOut
	}
	d.Due = quietAt
	if maxAt.Before(quietAt) {
		d.Due = maxAt
	}
	return d, nil
}
