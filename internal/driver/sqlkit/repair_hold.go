package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A repair's hold of its scan (tenant/repair_hold.go): RecordRepair checks
// the catalog and that nothing changes the scan, and records the hold with
// its end, the due of its unit's lease then plus tenant.RepairHoldGrace; a
// change of the scan's objects (a correction's prepare, publish and purge,
// a reception's commit) looks for a hold in its own transaction and is
// refused while one stands.
//
// The end is kept on the repair's row, not read from its unit's: a worker
// that lost the database keeps putting until its own deadline, before that
// due, whether or not another worker has claimed the unit since and moved
// its due, and the hold must outlast that put.

// repairHoldSQL selects a repair holding a scan at a time given as its
// first argument: one not ended nor let go whose hold has not run out. The
// scan's condition follows (repairs_holding).
const repairHoldSQL = `SELECT 1 FROM repairs WHERE open_work IS NOT NULL AND hold_until > ? AND series_uid `

// checkNoHold returns tenant.ScanRepairingError when a repair holds, at
// now, a scan the condition on series_uid selects with its arguments.
func (k Sending) checkNoHold(ctx context.Context, tx SendingTx, now time.Time, cond string, args ...any) error {
	// #nosec G202 -- cond is a constant of this package's callers; values are parameters.
	held, err := exists(ctx, tx, repairHoldSQL+cond+` LIMIT 1`, append([]any{k.D.Time(now)}, args...)...)
	if err != nil {
		return err
	}
	if held {
		return tenant.ScanRepairingError()
	}
	return nil
}

// CheckRepairHold refuses, with tenant.ScanRepairingError, a reception's
// commit to the scan of seriesUID at now while a repair holds it. A driver
// calls it in CommitScan once the scan's row is locked (updated), so a
// repair recorded meanwhile, which locks that row, is seen.
func (k Sending) CheckRepairHold(ctx context.Context, tx SendingTx, seriesUID string, now time.Time) error {
	return k.checkNoHold(ctx, tx, now, `= ?`, seriesUID)
}

// ScanRepairHeld implements tenant.Repairs.
func (k Sending) ScanRepairHeld(ctx context.Context, seriesUID string, now time.Time) (bool, error) {
	if err := master.ValidateTime("time", now); err != nil {
		return false, err
	}
	var held bool
	err := k.Host.Read(ctx, "the scan's repairs cannot be read", func(tx SendingTx) error {
		var err error
		held, err = exists(ctx, tx, repairHoldSQL+`= ? LIMIT 1`, k.D.Time(now), seriesUID)
		return err
	})
	return held, err
}

// ReleaseRepair implements tenant.Repairs.
func (k Sending) ReleaseRepair(ctx context.Context, fence tenant.WorkFence, id string) error {
	if err := fence.Validate(); err != nil {
		return err
	}
	if err := master.ValidateID("repair id", id); err != nil {
		return err
	}
	return k.Host.Write(ctx, "the repair's hold cannot be ended", func(tx SendingTx) error {
		if err := tx.CheckFence(ctx, fence); err != nil {
			return err
		}
		if found, err := exists(ctx, tx, `SELECT 1 FROM repairs WHERE id = ?`, id); err != nil || !found {
			if err != nil {
				return err
			}
			return notFound("the tenant has no repair with this id")
		}
		_, err := tx.Exec(ctx, `UPDATE repairs SET open_work = NULL, open_attempt = NULL, hold_until = NULL
			WHERE id = ? AND open_work = ? AND open_attempt = ?`, id, fence.ID, fence.Attempt)
		return err
	})
}

// holdEnd returns when a repair recorded now under fence stops holding its
// scan by itself: the due the fence's unit has, plus tenant.RepairHoldGrace.
// It refuses the record, with tenant.RepairJobEndedError, when the unit is
// a repair job's and the job has ended or was canceled, so a job canceled
// while it runs puts nothing more.
func (k Sending) holdEnd(ctx context.Context, tx SendingTx, fence tenant.WorkFence) (time.Time, error) {
	var due DBTime
	found, err := tx.QueryRow(ctx, `SELECT due FROM work WHERE id = ? AND attempt = ?`, []any{fence.ID, fence.Attempt}, &due)
	if err != nil {
		return time.Time{}, err
	}
	if !found {
		return time.Time{}, tenant.LeaseLost()
	}
	job, err := listingJobOf(ctx, tx, fence.ID)
	if err != nil {
		return time.Time{}, err
	}
	if job != "" {
		r, err := jobInTx(ctx, tx, job, k.D.Share)
		if err != nil {
			return time.Time{}, err
		}
		if r.job.Kind == tenant.JobRepair && r.job.State.Ended() {
			return time.Time{}, tenant.RepairJobEndedError()
		}
	}
	return due.T.Add(tenant.RepairHoldGrace), nil
}

// recordStray records the key repair r may have put as an orphan on its
// storage, of the scan finding f names, found by the repair's job, with its
// event: the repair ended after the catalog changed, so what its put left
// there may be bytes no catalog row explains, and a later scrub of the scan
// reads the key again (found good once nothing or a named object is
// there). An orphan that holds already at the key is enough; nothing is
// written then. An orphan never marks an instance.
func (k Sending) recordStray(ctx context.Context, tx SendingTx, r tenant.Repair, f tenant.ScrubFinding, now time.Time) error {
	id := tenant.ScrubFindingID(r.ID, r.Storage, r.Key, tenant.FindingOrphan)
	n, err := tx.Exec(ctx, `INSERT INTO scrub_findings (`+findingColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, '', 0, NULL, NULL, NULL, NULL, ?, ?, ?, ?, NULL, '', '') ON CONFLICT DO NOTHING`,
		id, string(tenant.FindingOrphan), r.Storage, r.Key, f.Path.Subject, f.Path.Session, f.Path.Scan,
		r.Job, k.D.Time(now), r.Job, k.D.Time(now))
	if err != nil || n == 0 {
		return err
	}
	out, found, err := findingIn(ctx, tx, findingByIDSQL, id)
	if err != nil {
		return err
	}
	if !found {
		return fault.New(fault.Internal, "an orphan just recorded cannot be read back")
	}
	e, err := tenant.ScrubFindingEvent(out, false, r.Job, now)
	if err != nil {
		return err
	}
	return tx.WriteEvents(ctx, e)
}

// checkCorrectionNotHeld refuses a step of the correction of id at now while
// a repair holds one of the scans it touches (correction_scans).
func (k Sending) checkCorrectionNotHeld(ctx context.Context, tx SendingTx, id string, now time.Time) error {
	return k.checkNoHold(ctx, tx, now, `IN (SELECT series_uid FROM correction_scans WHERE correction_id = ?)`, id)
}

// checkScanStill refuses a repair of the scan of series while a correction
// touching it is prepared, committed or purging, or while a switch of the
// storage or the replica is accepted or swapped: the first would change
// the catalog's bytes under the put, the second the place the put lands.
func (k Sending) checkScanStill(ctx context.Context, tx SendingTx, series, storage, from string) error {
	busy, err := exists(ctx, tx, `SELECT 1 FROM correction_scans AS cs JOIN corrections AS c ON c.id = cs.correction_id
		WHERE cs.series_uid = ? AND c.state IN ('prepared', 'committed', 'purging') LIMIT 1`, series)
	if err != nil {
		return err
	}
	if busy {
		return tenant.Precondition("a correction of the scan is under way; the repair waits for its end")
	}
	switching, err := exists(ctx, tx, `SELECT 1 FROM storage_switches WHERE state IN ('accepted', 'swapped')
		AND (primary_name IN (?, ?) OR replica_name IN (?, ?)) LIMIT 1`, storage, from, storage, from)
	if err != nil {
		return err
	}
	if switching {
		return tenant.Precondition("a switch of the storage is running")
	}
	return nil
}

// repairCatalogIn reads what the catalog holds now of the object finding f
// names (tenant.RepairCatalog), the scan's row read with lock, and the
// scan's Series Instance UID.
func repairCatalogIn(ctx context.Context, tx SendingTx, f tenant.ScrubFinding, lock string) (tenant.RepairCatalog, string, error) {
	var c tenant.RepairCatalog
	var scanID int64
	var series string
	var manifest []byte
	p := f.Path
	found, err := tx.QueryRow(ctx, `SELECT id, series_uid, storage, manifest_sha256 FROM scans WHERE subject_folded = ?
		AND session_folded = ? AND folded = ? AND subject_label = ? AND session_label = ? AND label = ?`+lock,
		[]any{label.Fold(p.Subject), label.Fold(p.Session), label.Fold(p.Scan), p.Subject, p.Session, p.Scan},
		&scanID, &series, &c.Storage, &manifest)
	if err != nil || !found {
		return c, "", err
	}
	c.ScanFound, c.Manifest = true, ToSHA256(manifest)
	if key, err := layout.ManifestKey(p); err == nil {
		c.ManifestKey = key
	}
	if f.SOPInstanceUID == "" {
		return c, series, nil
	}
	var raw string
	var sum []byte
	found, err = tx.QueryRow(ctx, `SELECT storage_key, byte_offset, byte_length, size, coalesce(archived_sha256, sha256)
		FROM instances WHERE scan_id = ? AND sop_instance_uid = ?`, []any{scanID, f.SOPInstanceUID},
		&raw, &c.Offset, &c.Length, &c.Size, &sum)
	if err != nil || !found {
		return c, series, err
	}
	key, err := instanceKey(raw, p, f.SOPInstanceUID)
	if err != nil {
		return c, series, err
	}
	c.InstanceFound, c.Key, c.SHA256 = true, key, ToSHA256(sum)
	return c, series, nil
}
