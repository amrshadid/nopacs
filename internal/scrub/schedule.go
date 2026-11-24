package scrub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ScheduleMethod is the method the audit entry of a scrub the server makes
// by itself names.
const ScheduleMethod = "scrub.schedule"

// ScheduledJobID returns the id of the scrub the server makes of a storage
// after the scrub of id after ended ("" when the storage had none): one id
// per storage and scrub it follows, so a scheduler that runs again, on
// this server, on another of the tenant's or after a restart, makes
// nothing twice (the id is taken).
func ScheduledJobID(storage, after string) string {
	sum := sha256.Sum256([]byte(storage + "\x00" + after))
	return "scrub-" + hex.EncodeToString(sum[:12])
}

// due reports whether a storage whose last scrub is last (none when has is
// false) is due a scheduled scrub at now: one scrub.interval after the
// last one ended, so a change of scrub.interval moves the next scrub from
// that end and never starts one at once by itself. A storage never
// scrubbed is due at once; one whose scrub has not ended is not.
func due(last tenant.Job, has bool, interval time.Duration, now time.Time) bool {
	if !has {
		return true
	}
	if !last.State.Ended() {
		return false
	}
	return !now.Before(last.Ended.Add(interval))
}

// schedule makes the tenant's scheduled scrubs every ScheduleInterval, and
// at once when the tenant's worker starts, until the worker is retired.
func (t *tenantRun) schedule() {
	defer t.sup.wg.Done()
	for {
		t.scheduleOnce()
		timer := time.NewTimer(t.sup.cfg.ScheduleInterval)
		select {
		case <-t.retired:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// scheduleOnce makes, for each storage of the tenant, a scrub when it is
// due (due), unless scrub.interval is "0", the scrub was made already, or
// another scrub of the storage is queued or running.
func (t *tenantRun) scheduleOnce() {
	p, st := t.pipeline()
	if p == nil || t.isRetired() {
		return
	}
	ctx, cancel := context.WithTimeout(t.sup.hard, opTimeout)
	defer cancel()
	interval, err := t.sup.cfg.TenantSettings(st).ScrubInterval(ctx)
	if err != nil {
		t.log.Warn("scrubs: scrub.interval could not be read; no scrub is scheduled now", "reason", fault.Message(err))
		return
	}
	if interval <= 0 {
		return
	}
	u := &unitRun{t: t, sup: t.sup, p: p, ctx: ctx}
	storages, err := u.storageNames()
	if err != nil {
		t.log.Warn("scrubs: the tenant's storages could not be listed; no scrub is scheduled now", "reason", fault.Message(err))
		return
	}
	made := false
	for _, s := range storages {
		now := t.sup.cfg.Now()
		last, has, err := p.LastScrub(ctx, s)
		if err != nil {
			t.log.Warn("scrubs: a storage's last scrub could not be read; none is scheduled for it now", "storage", s,
				"reason", fault.Message(err))
			continue
		}
		if !due(last, has, interval, now) {
			continue
		}
		id := ScheduledJobID(s, last.ID)
		audit := master.AuditEntry{ID: t.sup.cfg.NewID(), Time: now, Method: ScheduleMethod, TenantName: t.name, Object: id,
			Outcome: master.OutcomeDone, Details: map[string]string{"kind": string(tenant.JobScrub), "storage": s}}
		_, err = p.CreateJob(ctx, tenant.NewJob{ID: id, Kind: tenant.JobScrub, Storage: s, Scheduled: true, Created: now}, audit, nil)
		switch fault.KindOf(err) {
		case fault.Conflict, fault.Precondition:
			// Made already, or a scrub of the storage runs.
		default:
			if err != nil {
				t.log.Warn("scrubs: a scheduled scrub could not be made", "storage", s, "reason", fault.Message(err))
				continue
			}
			made = true
			t.log.Info("scrubs: a scheduled scrub was made", "storage", s, "job", id)
		}
	}
	if made {
		t.sup.cfg.Wake(t.name)
		t.wake()
	}
}
