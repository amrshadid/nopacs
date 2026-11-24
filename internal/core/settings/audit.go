package settings

import (
	"errors"
	"time"
)

// The settings of the audit seal (audit_seal.proto, AuditSealService). Both
// are installation-only: the sealing pass and retention run for every log
// of the installation at once, and a tenant's admin must not be able to
// shorten how long the record of what was done in the tenant is kept.
const (
	// AuditSealInterval is how often the sealing pass runs
	// (ParseSealInterval). Entries not yet sealed can be removed without a
	// trace, so it bounds how long that stays possible.
	AuditSealInterval = "audit.seal-interval"
	// AuditRetention is how long sealed entries are kept, "0" to keep every
	// entry (ParseAuditRetention). Whole checkpoints older than it are
	// pruned; every checkpoint stays.
	AuditRetention = "audit.retention"
)

// The bounds of the audit seal's settings.
const (
	// MinSealInterval and MaxSealInterval bound audit.seal-interval: a minute,
	// so a pass, which reads every log of the installation, never runs back
	// to back; a day, so what can be removed without a trace stays a day's
	// entries at most.
	MinSealInterval = time.Minute
	MaxSealInterval = 24 * time.Hour
	// MinAuditRetention and MaxAuditRetention bound audit.retention when it
	// is not "0": ninety days, far longer than any seal interval, so an
	// entry is always sealed before it can be pruned, and long enough that
	// a mistyped period cannot erase the recent record (a verification
	// refuses a pruned checkpoint younger than this, so a pruned flag set by
	// hand on a recent one shows); and a hundred years, past any rule of
	// record keeping.
	MinAuditRetention = 90 * 24 * time.Hour
	MaxAuditRetention = 36500 * 24 * time.Hour
)

// auditDefinitions returns the definitions of the audit seal's settings,
// for Builtin.
func auditDefinitions() []Definition {
	return []Definition{{
		// Ten minutes: entries can be removed without a trace for at most
		// that long, and a pass every ten minutes costs a few reads a log.
		Name: AuditSealInterval, Default: "10m",
		Validate: func(v string) error { _, err := ParseSealInterval(v); return err },
	}, {
		// Off by default: an audit log is kept whole until a person decides
		// how long it must be kept, and exports it first.
		Name: AuditRetention, Default: "0",
		Validate: func(v string) error { _, err := ParseAuditRetention(v); return err },
	}}
}

// ParseSealInterval reads audit.seal-interval: a duration (ParseDuration)
// from 1m to 1d.
func ParseSealInterval(v string) (time.Duration, error) {
	return durationIn(v, MinSealInterval, MaxSealInterval, "1m", "1d")
}

// ParseAuditRetention reads audit.retention: "0" to keep every entry, which
// it returns as 0, or a duration (ParseDuration) from 90d to 36500d.
func ParseAuditRetention(v string) (time.Duration, error) {
	if v == "0" {
		return 0, nil
	}
	d, err := ParseDuration(v)
	if err != nil {
		return 0, errors.New("is 0 to keep every entry, or a whole number of days or hours from 90d to 36500d, such as 3650d")
	}
	if d < MinAuditRetention || d > MaxAuditRetention {
		return 0, errors.New("is 0 to keep every entry, or at least 90d and at most 36500d")
	}
	return d, nil
}
