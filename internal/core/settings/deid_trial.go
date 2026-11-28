package settings

import "errors"

// DeidChangeTrialSessions is how many of a receiver's last receptions (its
// newest sessions) a new profile version is tried over before the receiver
// is pointed at it (ParseChangeTrialSessions). A tenant's admin sets it
// beside the other limits of the tenant's de-identification.
const DeidChangeTrialSessions = "deid.change-trial-sessions"

// The bounds of deid.change-trial-sessions.
const (
	// DefaultChangeTrialSessions is the default: a few weeks of a busy
	// receiver, a day of a quiet one.
	DefaultChangeTrialSessions = 20
	// MaxChangeTrialSessions is the largest: a trial reads a file of each
	// scan of each session within one request.
	MaxChangeTrialSessions = 200
)

// ParseChangeTrialSessions reads deid.change-trial-sessions: a whole number
// from 1 to MaxChangeTrialSessions.
func ParseChangeTrialSessions(v string) (int, error) {
	n, err := wholeNumber(v, MaxChangeTrialSessions)
	if err != nil || n < 1 {
		return 0, errors.New("is a whole number from 1 to 200")
	}
	return int(n), nil
}

// deidTrialDefinitions are the definitions of the settings of change
// control, for Builtin.
func deidTrialDefinitions() []Definition {
	return []Definition{{
		Name:           DeidChangeTrialSessions,
		Default:        "20",
		TenantOverride: true,
		Validate:       func(v string) error { _, err := ParseChangeTrialSessions(v); return err },
	}}
}
