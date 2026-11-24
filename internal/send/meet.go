package send

import (
	"context"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/jobmeet"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// reasonDatabase is the reason of a unit given back because the tenant's
// database did not answer a read of an instance's state.
const reasonDatabase = "the tenant's database did not answer"

// meet checks an instance against the archive as it is now, just before its
// transfer starts: a job outlives the state it was made in, so an instance a
// correction deleted since is refused as deleted_in_archive and never read,
// and one replaced since the stream was read is sent at its current
// revision. It returns the instance to send and its revision, or the result
// that settles it without a transfer, or the outcome that ends the unit.
func (u *unitRun) meet(in tenant.ScanInstance) (jobmeet.Meeting, *result, *outcome) {
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	m, err := jobmeet.Meet(ctx, u.p, u.fence, in)
	if err != nil {
		if u.ctx.Err() != nil {
			return m, nil, &outcome{stopped: true}
		}
		u.log.Warn("sender: the state of an instance could not be read", "reason", fault.Message(err))
		return m, nil, &outcome{temporary: true, reason: reasonDatabase}
	}
	if m.Deleted {
		r := u.deletedResult()
		return m, &r, nil
	}
	return m, nil, nil
}

// deletedResult is the result of an instance a correction deleted after the
// job was made: refused, counted, listed, and never read.
func (u *unitRun) deletedResult() result {
	u.log.Info("sender: an instance was deleted after the job was made; it is not sent")
	return refused(tenant.FailureDeletedInArchive, 0, detailDeleted)
}

// supersededByReplace reports whether the revision of an instance whose
// transfer has ended was replaced meanwhile. The transfer is not undone; a
// state that cannot be read is logged and counts as not replaced.
func (u *unitRun) supersededByReplace(uid string, rev int) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), opTimeout)
	defer cancel()
	replaced, err := jobmeet.Superseded(ctx, u.p, uid, rev)
	if err != nil {
		u.log.Warn("sender: whether an instance was replaced while it was sent could not be read", "reason", fault.Message(err))
		return false
	}
	return replaced
}

// tenantStopped returns the outcome of a unit whose tenant is no longer
// served as ready (moved, fenced, restoring, failed or removed), and whether
// it must stop: the unit is given back with the reason the registry states,
// its cursor kept, and tried again after the sender interval.
func (u *unitRun) tenantStopped() (outcome, bool) {
	_, err := u.sup.cfg.Tenants.Store(u.sr.t.name)
	if err == nil {
		return outcome{}, false
	}
	reason := fault.Message(err)
	if reason == "" {
		reason = "the tenant is not ready"
	}
	return outcome{release: true, reason: reason, due: u.sup.cfg.Now().Add(u.sup.cfg.Interval)}, true
}
