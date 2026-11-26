package settings

import (
	"errors"
	"strconv"
	"time"
)

// The settings of replicas, retention and the quarantine's warnings
// (replica.proto, retention.proto). Each is ordinary unless its comment says
// otherwise: a tenant's admin may override the installation's value for the
// tenant.
const (
	// ReplicaRepair is whether a primary's damaged or missing object found by
	// a scrub is repaired from a replica by a person (manual) or by the server
	// by itself (auto) (ParseReplicaRepair).
	ReplicaRepair = "replica.repair"
	// ReplicaRepairLease is how long a repair job's claim holds its unit, and
	// so the longest one put of an object may take (ParseRepairLease). It is
	// read before each claim of a repair job's unit.
	ReplicaRepairLease = "replica.repair-lease"
	// RetentionShortenNotice is how long after a change of retention rules
	// that would make data due sooner, or keep it less, the change may take
	// effect at the soonest (ParseShortenNotice). Set on a tenant by the
	// installation's admin only: it is what stands between a mistyped rule and
	// a disposal.
	RetentionShortenNotice = "retention.shorten-notice"
	// RetentionMaxClockStep is how far forward the server's clock may move
	// between two checks before retention pauses for a person to confirm the
	// time (ParseMaxClockStep). Set on a tenant by the installation's admin
	// only, for the same reason.
	RetentionMaxClockStep = "retention.max-clock-step"
	// RetentionMaxPerPass is the most sessions one daily pass of a delete
	// rule disposes of (ParseMaxPerPass).
	RetentionMaxPerPass = "retention.max-per-pass" //nolint:gosec // G101: a setting's name, not a credential.
	// QuarantineWarnAge is the age of the oldest held file past which a
	// tenant's status warns (ParseQuarantineWarnAge).
	QuarantineWarnAge = "quarantine.warn-age"
	// QuarantineWarnBytes is the bytes held in quarantine past which a
	// tenant's status warns, or "0" for no warning (ParseSize).
	QuarantineWarnBytes = "quarantine.warn-bytes"
)

// The bounds of these settings.
const (
	// MinRepairLease and MaxRepairLease bound replica.repair-lease: at least
	// 30 seconds, so a put keeps 15 seconds past the margin a repair cuts it
	// short by before its lease's due; at most an hour, the longest lease a
	// unit of work holds.
	MinRepairLease = 30 * time.Second
	MaxRepairLease = time.Hour
	// MinShortenNotice and MaxShortenNotice bound retention.shorten-notice.
	MinShortenNotice = 24 * time.Hour
	MaxShortenNotice = 365 * 24 * time.Hour
	// MinMaxClockStep and MaxMaxClockStep bound retention.max-clock-step.
	MinMaxClockStep = time.Hour
	MaxMaxClockStep = 30 * 24 * time.Hour
	// MaxPerPass bounds retention.max-per-pass.
	MaxPerPass = 100000
	// MinQuarantineWarnAge and MaxQuarantineWarnAge bound
	// quarantine.warn-age: a day to ten years.
	MinQuarantineWarnAge = 24 * time.Hour
	MaxQuarantineWarnAge = 3650 * 24 * time.Hour
)

// ParseReplicaRepair reads replica.repair: "manual" or "auto".
func ParseReplicaRepair(v string) (string, error) {
	if v != "manual" && v != "auto" {
		return "", errors.New("is manual or auto")
	}
	return v, nil
}

// ParseRepairLease reads replica.repair-lease: a duration (ParseDuration)
// from 30s to 1h.
func ParseRepairLease(v string) (time.Duration, error) {
	return boundedDuration(v, MinRepairLease, MaxRepairLease, "is a period from 30s to 1h, such as 1m")
}

// ParseShortenNotice reads retention.shorten-notice: a duration from 1d to
// 365d.
func ParseShortenNotice(v string) (time.Duration, error) {
	return boundedDuration(v, MinShortenNotice, MaxShortenNotice, "is a period from 1d to 365d, such as 30d")
}

// ParseMaxClockStep reads retention.max-clock-step: a duration from 1h to
// 30d.
func ParseMaxClockStep(v string) (time.Duration, error) {
	return boundedDuration(v, MinMaxClockStep, MaxMaxClockStep, "is a period from 1h to 30d, such as 2d")
}

// ParseQuarantineWarnAge reads quarantine.warn-age: a duration from 1d to
// 3650d.
func ParseQuarantineWarnAge(v string) (time.Duration, error) {
	return boundedDuration(v, MinQuarantineWarnAge, MaxQuarantineWarnAge, "is a period from 1d to 3650d, such as 30d")
}

// ParseMaxPerPass reads retention.max-per-pass: a whole number from 1 to
// 100000, written without a sign or leading zeros.
func ParseMaxPerPass(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > MaxPerPass || strconv.Itoa(n) != v {
		return 0, errors.New("is a whole number from 1 to 100000")
	}
	return n, nil
}

// boundedDuration reads v (ParseDuration) held to [least, most], refusing
// with bad otherwise.
func boundedDuration(v string, least, most time.Duration, bad string) (time.Duration, error) {
	d, err := ParseDuration(v)
	if err != nil || d < least || d > most {
		return 0, errors.New(bad)
	}
	return d, nil
}

// replicaDefinitions returns the definitions of the settings of replicas,
// retention and the quarantine's warnings, for Builtin.
func replicaDefinitions() []Definition {
	return []Definition{{
		// Manual: damage is a person's to see before the server writes to
		// the storage again, and reads already turn to a replica in step, so
		// waiting loses nothing.
		Name:           ReplicaRepair,
		Default:        "manual",
		TenantOverride: true,
		Validate:       func(v string) error { _, err := ParseReplicaRepair(v); return err },
	}, {
		// A minute: a put of 45 seconds carries the 4 GiB a receiver accepts
		// by default at 100 MiB a second; a tenant whose objects are larger,
		// or whose primary is slower, lengthens it.
		Name:           ReplicaRepairLease,
		Default:        "1m",
		TenantOverride: true,
		Validate:       func(v string) error { _, err := ParseRepairLease(v); return err },
	}, {
		// Thirty days: a rule mistyped on a Friday is seen long before
		// anything it shortened becomes due.
		Name:                    RetentionShortenNotice,
		Default:                 "30d",
		TenantOverride:          true,
		TenantSetByInstallation: true,
		Validate:                func(v string) error { _, err := ParseShortenNotice(v); return err },
	}, {
		// Two days: a server stopped over a weekend starts again without an
		// admin, and a clock set years ahead pauses retention.
		Name:                    RetentionMaxClockStep,
		Default:                 "2d",
		TenantOverride:          true,
		TenantSetByInstallation: true,
		Validate:                func(v string) error { _, err := ParseMaxClockStep(v); return err },
	}, {
		// A thousand sessions a day: a rule that makes a large backlog due at
		// once disposes of it over days, each day's deletes small enough for
		// a person to read in the next morning's list.
		Name:           RetentionMaxPerPass,
		Default:        "1000",
		TenantOverride: true,
		Validate:       func(v string) error { _, err := ParseMaxPerPass(v); return err },
	}, {
		// Thirty days: a held file is a person's to decide on within a month.
		Name:           QuarantineWarnAge,
		Default:        "30d",
		TenantOverride: true,
		Validate:       func(v string) error { _, err := ParseQuarantineWarnAge(v); return err },
	}, {
		// Off: the incoming area's own limit already bounds what is held.
		Name:           QuarantineWarnBytes,
		Default:        "0",
		TenantOverride: true,
		Validate:       func(v string) error { _, err := ParseSize(v); return err },
	}}
}
