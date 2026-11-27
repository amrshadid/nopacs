package settings

import (
	"errors"
	"strconv"
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

// CorrectionBatchMax is the most targets one batch delete names
// (correction.proto, DeleteBatch): a whole number from 1 to 10000
// (ParseBatchMax).
const CorrectionBatchMax = "correction.batch-max"

// MaxBatchMax bounds correction.batch-max: the contract's most targets of
// one batch.
const MaxBatchMax = 10000

// ParseBatchMax reads correction.batch-max: a whole number from 1 to
// MaxBatchMax, written without a sign or leading zeros.
func ParseBatchMax(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > MaxBatchMax || strconv.Itoa(n) != v {
		return 0, errors.New("is a whole number from 1 to " + strconv.Itoa(MaxBatchMax))
	}
	return n, nil
}

// correctionDefinitions returns the definitions of the correction settings,
// for Builtin.
func correctionDefinitions() []Definition {
	return []Definition{{
		// The contract's bound: a list of ten thousand targets previews in
		// one answer of a few MiB, and each target is still deleted alone.
		// Set on a tenant by the installation's admin only: it bounds what a
		// tenant's own admin can make go at once.
		Name:                    CorrectionBatchMax,
		Default:                 "10000",
		TenantOverride:          true,
		TenantSetByInstallation: true,
		Validate:                func(v string) error { _, err := ParseBatchMax(v); return err },
	}, {
		// Seven days: a mistyped label is noticed within a working week,
		// and a withdrawal is complete within the same week.
		Name:           DeleteGrace,
		Default:        "7d",
		TenantOverride: true,
		Validate:       func(v string) error { _, err := ParseDeleteGrace(v); return err },
	}}
}
