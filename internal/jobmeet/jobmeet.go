// Package jobmeet lets a job's unit meet the archive as it is now, before
// each instance it reads, rather than as it was when the job was made: a
// job outlives the state it was made in, and a correction (a delete, or a
// replace of an instance's file) may commit between the unit's page of the
// instance stream and the instance's turn.
//
// A unit asks Meet for each instance of its stream just before the
// instance's transfer or read starts. An instance a correction deleted is
// reported as such, to be refused with tenant.FailureDeletedInArchive and
// never read; an instance replaced since the stream was read is returned as
// the catalog holds it now, so the unit reads the revision current when the
// transfer starts. After a transfer that cannot be taken back, Superseded
// says whether a replace committed meanwhile.
package jobmeet

import (
	"context"
	"strings"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// TallySupersededDuringSend is the tally of a send job's instances whose
// file was replaced while the instance was being sent: the transfer is not
// undone, and a later send sends the new revision.
const TallySupersededDuringSend = "superseded_during_send"

// Archive is what a unit asks of its tenant's database: tenant.Pipeline
// holds all three.
type Archive interface {
	// ResolveInstances answers what became of each instance revision.
	ResolveInstances(ctx context.Context, refs []tenant.InstanceRef) ([]tenant.ResolvedInstance, error)
	// JobScanInstances reads a page of the unit's stream.
	JobScanInstances(ctx context.Context, fence tenant.WorkFence, after string, limit int) ([]tenant.ScanInstance, error)
	// DeletedJobInstances reads a page of the instances of the unit's
	// stream a committed delete hid, which the stream no longer shows.
	DeletedJobInstances(ctx context.Context, fence tenant.WorkFence, after string, limit int) ([]tenant.ScanInstance, error)
}

// Meeting is how an instance stands now.
type Meeting struct {
	// Deleted says the instance is no longer held: a correction deleted it,
	// or it is held no more. Instance is then the row as the stream gave
	// it.
	Deleted bool
	// Instance is the instance as the catalog holds it now: the row the
	// stream gave when its revision is still current, else the current
	// row.
	Instance tenant.ScanInstance
	// Revision is the revision Instance's file is, the one to read.
	Revision int
}

// refreshTries bounds the rows read past an instance's place in the stream
// while looking for its current row.
const refreshTries = 8

// Meet returns how in stands now. It costs one read of the catalog by
// instance, and a second only when the instance was replaced since the
// stream was read. An instance replaced that the unit's stream no longer
// shows (a forward job streams by the reception that archived it) is
// answered as the stream gave it, still whole on its storage until the
// replace's purge, with its own revision, so Superseded counts it after the
// transfer.
func Meet(ctx context.Context, a Archive, fence tenant.WorkFence, in tenant.ScanInstance) (Meeting, error) {
	rev := tenant.RevisionOfKey(in.Key)
	got, err := a.ResolveInstances(ctx, []tenant.InstanceRef{{UID: in.SOPInstanceUID, Revision: rev}})
	if err != nil {
		return Meeting{}, err
	}
	m := Meeting{Instance: in, Revision: rev}
	if len(got) != 1 {
		return m, nil
	}
	switch r := got[0]; {
	case r.Disposition == tenant.ResolvedCurrent:
		return m, nil
	case r.Disposition == tenant.ResolvedSuperseded && r.CurrentRevision > 0:
		fresh, found, err := refresh(ctx, a, fence, in.SOPInstanceUID)
		if err != nil {
			return Meeting{}, err
		}
		if found {
			m.Instance, m.Revision = fresh, tenant.RevisionOfKey(fresh.Key)
		}
		return m, nil
	}
	// Deleted, no longer held, or a revision of an instance a delete hid.
	m.Deleted = true
	return m, nil
}

// Superseded reports whether the revision rev of the instance uid, whose
// transfer has ended, is no longer the one held: a replace committed while
// it was sent. An instance deleted meanwhile is not superseded.
func Superseded(ctx context.Context, a Archive, uid string, rev int) (bool, error) {
	got, err := a.ResolveInstances(ctx, []tenant.InstanceRef{{UID: uid, Revision: rev}})
	if err != nil || len(got) != 1 {
		return false, err
	}
	return got[0].Disposition == tenant.ResolvedSuperseded && got[0].CurrentRevision > 0, nil
}

// refresh reads the current row of the instance uid from the unit's stream,
// by a read of one row just before its place.
func refresh(ctx context.Context, a Archive, fence tenant.WorkFence, uid string) (tenant.ScanInstance, bool, error) {
	after := before(uid)
	for range refreshTries {
		page, err := a.JobScanInstances(ctx, fence, after, 1)
		if err != nil {
			return tenant.ScanInstance{}, false, err
		}
		if len(page) == 0 || page[0].SOPInstanceUID > uid {
			return tenant.ScanInstance{}, false, nil
		}
		if page[0].SOPInstanceUID == uid {
			return page[0], true, nil
		}
		after = page[0].SOPInstanceUID
	}
	return tenant.ScanInstance{}, false, nil
}

// before returns a UID that sorts before uid and, but for a few UIDs of
// the same prefix, directly before it: uid without its last character, and
// without a dot it then ends in. The empty string, the start of a stream,
// when nothing is left.
func before(uid string) string {
	if len(uid) == 0 {
		return ""
	}
	p := strings.TrimRight(uid[:len(uid)-1], ".")
	if tenant.ValidateInstanceUID(p) != nil {
		return ""
	}
	return p
}
