package settings

import (
	"errors"
	"time"
)

// ReceiverStartLimit is the longest a tenant's reception pipeline may take
// to start (ParseStartLimit). A start reconciles the incoming area with the
// database: it reads every file the database names there and every file on
// disk, so a large leftover after a crash takes long. A start that passes
// the limit is cut and tried again from its beginning, the tenant's
// receivers meanwhile refusing files as a temporary failure, so nothing is
// lost. The installation's value is every tenant's default; a tenant's own
// is set by the installation's admin, since the start holds one of the
// server's workers and the disk's reads.
const ReceiverStartLimit = "receiver.start-limit"

// The bounds of receiver.start-limit: a minute, the least in which a small
// tenant's start finishes on a slow disk; a day, past any recovery that
// should be left to run in one piece.
const (
	MinStartLimit = time.Minute
	MaxStartLimit = 24 * time.Hour
)

// startDefinitions returns the definitions of the pipeline start's
// settings, for Builtin.
func startDefinitions() []Definition {
	return []Definition{{
		// 15 minutes: a start cut short is begun again from the first
		// file, so a bound as short as a stop's could keep a tenant's
		// receivers down for good, while a start that has not finished
		// after a quarter of an hour is more likely stuck than slow.
		Name:                    ReceiverStartLimit,
		Default:                 "15m",
		TenantOverride:          true,
		TenantSetByInstallation: true,
		Validate:                func(v string) error { _, err := ParseStartLimit(v); return err },
	}}
}

// ParseStartLimit reads receiver.start-limit: a duration (ParseDuration)
// from 1m to 24h.
func ParseStartLimit(v string) (time.Duration, error) {
	d, err := ParseDuration(v)
	if err != nil || d < MinStartLimit || d > MaxStartLimit {
		return 0, errors.New("is a whole number of minutes or hours from 1m to 24h, such as 15m")
	}
	return d, nil
}
