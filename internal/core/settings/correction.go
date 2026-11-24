package settings

import (
	"errors"
	"strings"
	"time"
)

// DeleteGrace is how long a delete's bytes stay on the storage, unserved,
// before the server purges them (correction.proto, "The grace period and
// the purge"): a period from "0d" to "90d" (ParseDeleteGrace). It is
// ordinary: a tenant's admin may override the installation's value for the
// tenant, since how long a mistyped delete can be undone is the tenant's
// to weigh against how soon a withdrawal must be complete.
const DeleteGrace = "delete.grace"

// MaxDeleteGrace bounds delete.grace: ninety days.
const MaxDeleteGrace = 90 * 24 * time.Hour

// ParseDeleteGrace reads delete.grace: a duration (ParseDuration) of at
// most 90d, or a zero count of any of its units ("0d", "0s"), for a delete
// purged at the server's next pass.
func ParseDeleteGrace(v string) (time.Duration, error) {
	bad := errors.New(`is a period from 0d to 90d, such as 7d, 36h or 0d`)
	if len(v) == 2 && v[0] == '0' && strings.ContainsRune("smhd", rune(v[1])) {
		return 0, nil
	}
	d, err := ParseDuration(v)
	if err != nil || d > MaxDeleteGrace {
		return 0, bad
	}
	return d, nil
}

// correctionDefinitions returns the definitions of the correction settings,
// for Builtin.
func correctionDefinitions() []Definition {
	return []Definition{{
		// Seven days: a mistyped label is noticed within a working week,
		// and a withdrawal is complete within the same week.
		Name:           DeleteGrace,
		Default:        "7d",
		TenantOverride: true,
		Validate:       func(v string) error { _, err := ParseDeleteGrace(v); return err },
	}}
}
