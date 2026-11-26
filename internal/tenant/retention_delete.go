package tenant

import "time"

// SessionKept is what a delete's check of retention reads of one session
// of its target, under the lock its prepare holds.
type SessionKept struct {
	// Found says the session has a retention row.
	Found bool
	// Stored are its times as stored; Rules are the times the rules as they
	// stand now give from its start and what its scans arrived through
	// (RetentionTimesOf). Both are read: a stored time is what every
	// listing showed, and the rules read again are what a commit beside a
	// rule's change could not have stored.
	Stored, Rules RetentionTimes
	// Exempt names the rule a person's undo of a retention delete exempts
	// it from, "" for none.
	Exempt string
}

// RetentionDeleteCheck is the facts a delete's check of retention decides
// on.
type RetentionDeleteCheck struct {
	// Origin is the rule and due time of a delete a disposal makes; nil for
	// a person's.
	Origin *RetentionOrigin
	// Override says a person's delete goes over a rule's keep-at-least.
	Override bool
	// Now is when the delete is prepared.
	Now time.Time
	// Sessions are the target's sessions; a disposal's names one.
	Sessions []SessionKept
	// Studies are the Study Instance UIDs of every session the target
	// holds now, read for a disposal's delete, which must hold exactly the
	// one its origin names.
	Studies []string
	// Paused says retention is paused for its clock; Effective is the time
	// due times are read at (RetentionClock.Effective). Both are read only
	// for a disposal's delete.
	Paused    bool
	Effective time.Time
}

// CheckRetentionDelete returns retention's refusal of a delete, or nil.
//
// A person's delete is refused (RETENTION_MINIMUM) when a rule keeps any of
// its sessions past Now, by the stored keep or the keep the rules give now,
// whichever is later, unless it overrides; the refusal names the latest
// keep. A disposal's delete is refused while retention is paused, and
// (NotDue) unless its session is due under its rule at Effective and kept
// no longer, both as stored and as the rules give now, so a session never
// goes sooner than either says; the daily pass's is also refused for a
// session a person's undo exempts from that rule.
func CheckRetentionDelete(c RetentionDeleteCheck) error {
	o := c.Origin
	if o == nil {
		var until time.Time
		for _, s := range c.Sessions {
			for _, k := range []time.Time{s.Stored.KeepUntil, s.Rules.KeepUntil} {
				if k.After(c.Now) && k.After(until) {
					until = k
				}
			}
		}
		if !until.IsZero() && !c.Override {
			return RetentionMinimumError(until)
		}
		return nil
	}
	if err := ValidateStudyUID(o.StudyUID); err != nil {
		return err
	}
	// The job listed the session by its labels, which a relabel since may
	// have given to another session: deleting by the labels alone would
	// delete a session the disposal never listed.
	if len(c.Studies) != 1 || c.Studies[0] != o.StudyUID {
		return NotDueError("the target no longer holds the session the disposal listed")
	}
	if c.Paused {
		return RetentionPausedError()
	}
	due := func(t RetentionTimes) bool {
		return t.Rule == o.Rule && !t.Due.IsZero() && !t.Due.After(c.Effective) && !t.KeepUntil.After(c.Effective)
	}
	var s SessionKept
	if len(c.Sessions) > 0 {
		s = c.Sessions[0]
	}
	switch {
	case !s.Found || !due(s.Stored):
		return NotDueError("the session is not due under the rule " + o.Rule + " now")
	case o.Scheduled && s.Exempt == o.Rule:
		return NotDueError("a person undid a retention delete of the session; the daily pass leaves it alone")
	case !due(s.Rules):
		return NotDueError("the rules as they stand do not make the session due under " + o.Rule + " now")
	}
	return nil
}
