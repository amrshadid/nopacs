package settingvalue

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/settings"
)

// ReplicaRepair returns replica.repair as it applies on the tenant: "manual"
// or "auto".
func (t Tenant) ReplicaRepair(ctx context.Context) (string, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.ReplicaRepair)
	return typed(settings.ReplicaRepair, v, err, settings.ParseReplicaRepair)
}

// RepairLease returns replica.repair-lease as it applies on the tenant.
func (t Tenant) RepairLease(ctx context.Context) (time.Duration, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.ReplicaRepairLease)
	return typed(settings.ReplicaRepairLease, v, err, settings.ParseRepairLease)
}

// LagAlert returns replica.lag-alert as it applies on the tenant.
func (t Tenant) LagAlert(ctx context.Context) (time.Duration, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.ReplicaLagAlert)
	return typed(settings.ReplicaLagAlert, v, err, settings.ParseLagAlert)
}

// ReplicaRate returns replica.rate as it applies on the tenant: bytes a
// second put on one replica, 0 for no bound.
func (t Tenant) ReplicaRate(ctx context.Context) (int64, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.ReplicaRate)
	return typed(settings.ReplicaRate, v, err, settings.ParseReplicaRate)
}

// RecallWait returns recall.wait as it applies on the tenant.
func (t Tenant) RecallWait(ctx context.Context) (time.Duration, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.RecallWait)
	return typed(settings.RecallWait, v, err, settings.ParseRecallWait)
}

// ArchiveSample returns scrub.archive-sample as it applies on the tenant:
// the share, from 0 to 1, of an archive-tier storage's online objects whose
// bytes a scrub reads.
func (t Tenant) ArchiveSample(ctx context.Context) (float64, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.ScrubArchiveSample)
	return typed(settings.ScrubArchiveSample, v, err, settings.ParseArchiveSample)
}

// ShortenNotice returns retention.shorten-notice as it applies on the
// tenant.
func (t Tenant) ShortenNotice(ctx context.Context) (time.Duration, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.RetentionShortenNotice)
	return typed(settings.RetentionShortenNotice, v, err, settings.ParseShortenNotice)
}

// MaxClockStep returns retention.max-clock-step as it applies on the
// tenant.
func (t Tenant) MaxClockStep(ctx context.Context) (time.Duration, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.RetentionMaxClockStep)
	return typed(settings.RetentionMaxClockStep, v, err, settings.ParseMaxClockStep)
}

// MaxPerPass returns retention.max-per-pass as it applies on the tenant.
func (t Tenant) MaxPerPass(ctx context.Context) (int, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.RetentionMaxPerPass)
	return typed(settings.RetentionMaxPerPass, v, err, settings.ParseMaxPerPass)
}

// QuarantineWarnAge returns quarantine.warn-age as it applies on the tenant.
func (t Tenant) QuarantineWarnAge(ctx context.Context) (time.Duration, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.QuarantineWarnAge)
	return typed(settings.QuarantineWarnAge, v, err, settings.ParseQuarantineWarnAge)
}

// QuarantineWarnBytes returns quarantine.warn-bytes as it applies on the
// tenant, 0 for no warning.
func (t Tenant) QuarantineWarnBytes(ctx context.Context) (int64, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.QuarantineWarnBytes)
	return typed(settings.QuarantineWarnBytes, v, err, settings.ParseSize)
}
