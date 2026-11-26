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
	// ReplicaLagAlert is how long a replica's oldest scan not in step may
	// wait before the server reports the replica behind with an event, and
	// again when it is back under it (ParseLagAlert).
	ReplicaLagAlert = "replica.lag-alert"
	// ReplicaRate is the most bytes a second the replicator puts on one
	// replica, or "0" for no bound (ParseReplicaRate). It is read at each
	// claim of scans to copy.
	ReplicaRate = "replica.rate"
	// RecallWait is how long a recall may stay requested or in progress
	// before the server gives it up as failed (ParseRecallWait).
	RecallWait = "recall.wait"
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
	// MinLagAlert and MaxLagAlert bound replica.lag-alert: at least a
	// minute, the period the lag is looked at; at most thirty days.
	MinLagAlert = time.Minute
	MaxLagAlert = 30 * 24 * time.Hour
	// MinReplicaRate and MaxReplicaRate bound replica.rate when it is not
	// "0": at least 1 MiB a second, so a backfill of a large primary ends;
	// at most 10 GiB a second, past any link to a replica.
	MinReplicaRate = 1 << 20
	MaxReplicaRate = 10 << 30
	// MinRecallWait and MaxRecallWait bound recall.wait: at least an hour,
	// the quickest a standard restore of a cold class takes; at most thirty
	// days, past the slowest bulk restore.
	MinRecallWait = time.Hour
	MaxRecallWait = 30 * 24 * time.Hour
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

// ParseLagAlert reads replica.lag-alert: a duration from 1m to 30d.
func ParseLagAlert(v string) (time.Duration, error) {
	return boundedDuration(v, MinLagAlert, MaxLagAlert, "is a period from 1m to 30d, such as 1h")
}

// ParseReplicaRate reads replica.rate: "0" for no bound, or a size
// (ParseSize) from 1MiB to 10GiB, the bytes a second put on one replica.
// Zero means no bound.
func ParseReplicaRate(v string) (int64, error) {
	if v == "0" {
		return 0, nil
	}
	n, err := ParseSize(v)
	if err != nil || n < MinReplicaRate || n > MaxReplicaRate {
		return 0, errors.New(`is "0" for no bound, or a size from 1MiB to 10GiB, put each second`)
	}
	return n, nil
}

// ParseRecallWait reads recall.wait: a duration from 1h to 30d.
func ParseRecallWait(v string) (time.Duration, error) {
	return boundedDuration(v, MinRecallWait, MaxRecallWait, "is a period from 1h to 30d, such as 48h")
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
		// An hour: a replica that has not caught up within an hour of a
		// commit is stuck (a storage down, a key revoked), not busy, and a
		// person should look.
		Name:           ReplicaLagAlert,
		Default:        "1h",
		TenantOverride: true,
		Validate:       func(v string) error { _, err := ParseLagAlert(v); return err },
	}, {
		// No bound: each put has a time limit, and a bound a site did not
		// choose would make large objects miss it on a fast link; a site
		// whose link to a replica is shared sets one.
		Name:           ReplicaRate,
		Default:        "0",
		TenantOverride: true,
		Validate:       func(v string) error { _, err := ParseReplicaRate(v); return err },
	}, {
		// Two days: a bulk restore of the deepest class takes up to 48
		// hours; one not back by then is lost or refused by the store.
		Name:           RecallWait,
		Default:        "48h",
		TenantOverride: true,
		Validate:       func(v string) error { _, err := ParseRecallWait(v); return err },
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
		// admin, and a clock set years ahead pauses retention. One value for
		// the installation: the clock is the server's, not a tenant's.
		Name:     RetentionMaxClockStep,
		Default:  "2d",
		Validate: func(v string) error { _, err := ParseMaxClockStep(v); return err },
	}, {
		// A thousand sessions a day: a rule that makes a large backlog due at
		// once disposes of it over days, each day's deletes small enough for
		// a person to read in the next morning's list.
		// Set on a tenant by the installation's admin only: it bounds what a
		// tenant's own admin can make go at once.
		Name:                    RetentionMaxPerPass,
		Default:                 "1000",
		TenantOverride:          true,
		TenantSetByInstallation: true,
		Validate:                func(v string) error { _, err := ParseMaxPerPass(v); return err },
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
