package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The repairs of a primary's objects from a replica (tenant.Repairs), on
// the repairs table of each driver's tenant baseline schema, and the
// scrub's findings and marks they clear (scrub.go).

// repairColumns are a repair's columns as the store reads them.
const repairColumns = `id, finding_id, storage, object_key, sop_instance_uid, revision, from_replica, job_id, expected_size,
	expected_sha256, found_size, found_sha256, recorded_at, repaired_at`

// scanRepair reads one row of repairColumns.
func scanRepair(r Row) (tenant.Repair, error) {
	var out tenant.Repair
	var expected, found []byte
	var recorded, repaired DBTime
	err := r.Scan(&out.ID, &out.Finding, &out.Storage, &out.Key, &out.SOPInstanceUID, &out.Revision, &out.From, &out.Job,
		&out.ExpectedSize, &expected, &out.FoundSize, &found, &recorded, &repaired)
	out.ExpectedSHA256, out.FoundSHA256 = ToSHA256(expected), ToSHA256(found)
	out.Recorded, out.Repaired = recorded.T, repaired.T
	return out, err
}

// repairIn reads a repair in tx with the lock given, and whether there is
// one.
func repairIn(ctx context.Context, tx SendingTx, id, lock string) (tenant.Repair, bool, error) {
	var out tenant.Repair
	found := false
	err := tx.Query(ctx, `SELECT `+repairColumns+` FROM repairs WHERE id = ?`+lock, []any{id}, func(r Row) error {
		var err error
		out, err = scanRepair(r)
		found = true
		return err
	})
	return out, found, err
}

// RecordRepair implements tenant.Repairs.
func (k Sending) RecordRepair(ctx context.Context, rec tenant.RepairRecord) (tenant.Repair, bool, error) {
	if err := rec.Validate(); err != nil {
		return tenant.Repair{}, false, err
	}
	p := rec.Repair
	var out tenant.Repair
	var made bool
	err := k.Host.Write(ctx, "the repair cannot be recorded", func(tx SendingTx) error {
		made = false
		if rec.Fence != nil {
			if err := tx.CheckFence(ctx, *rec.Fence); err != nil {
				return err
			}
		}
		old, held, err := repairIn(ctx, tx, p.ID, "")
		if err != nil {
			return err
		}
		if held {
			out = old
			return nil
		}
		f, found, err := findingIn(ctx, tx, findingByIDSQL+k.D.Share, p.Finding)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no scrub finding with this id")
		}
		storage, found, err := storageIn(ctx, tx, p.Storage, k.D.Share)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no storage named " + p.Storage)
		}
		from, found, err := storageIn(ctx, tx, p.From, k.D.Share)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no storage named " + p.From)
		}
		if err := tenant.CheckRepair(f, storage, from); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO repairs (`+repairColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
			p.ID, f.ID, f.Storage, f.Key, f.SOPInstanceUID, f.Revision, p.From, p.Job, p.ExpectedSize, p.ExpectedSHA256[:],
			nullInt(p.FoundSize), nullSHA(p.FoundSHA256), k.D.Time(rec.Now)); err != nil {
			return err
		}
		made = true
		out, _, err = repairIn(ctx, tx, p.ID, "")
		return err
	})
	if err != nil {
		return tenant.Repair{}, false, err
	}
	return out, made, nil
}

// CompleteRepair implements tenant.Repairs.
func (k Sending) CompleteRepair(ctx context.Context, fence *tenant.WorkFence, id string, now time.Time, audit master.AuditEntry) (tenant.Repair, error) {
	if fence != nil {
		if err := fence.Validate(); err != nil {
			return tenant.Repair{}, err
		}
	}
	if err := master.ValidateID("repair id", id); err != nil {
		return tenant.Repair{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Repair{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Repair{}, err
	}
	var out tenant.Repair
	err := k.Host.Write(ctx, "the repair cannot be ended", func(tx SendingTx) error {
		if fence != nil {
			if err := tx.CheckFence(ctx, *fence); err != nil {
				return err
			}
		}
		r, found, err := repairIn(ctx, tx, id, k.D.Lock)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no repair with this id")
		}
		out = r
		if r.Done() {
			return nil
		}
		f, found, err := findingIn(ctx, tx, findingByIDSQL+k.D.Lock, r.Finding)
		if err != nil {
			return err
		}
		if !found {
			return fault.New(fault.Internal, "a repair names a finding the tenant does not have")
		}
		// A switch's swap moves a finding with its place to the other name;
		// the repair recorded before it put its bytes where the old name
		// pointed, which says nothing of the place the finding now names.
		if f.Storage != r.Storage {
			return tenant.Precondition("the repair's finding moved to the storage " + f.Storage + " since the repair was recorded")
		}
		if f.Holds() {
			if _, err := k.clearInTx(ctx, tx, r.Job, f, now); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE repairs SET repaired_at = ? WHERE id = ?`, k.D.Time(now), id); err != nil {
			return err
		}
		out.Repaired = now
		return tx.Audited(ctx, audit, nil)
	})
	if err != nil {
		return tenant.Repair{}, err
	}
	return out, nil
}

// Repair implements tenant.Repairs.
func (k Sending) Repair(ctx context.Context, id string) (tenant.Repair, error) {
	if err := master.ValidateID("repair id", id); err != nil {
		return tenant.Repair{}, err
	}
	var out tenant.Repair
	err := k.Host.Read(ctx, "the repair cannot be read", func(tx SendingTx) error {
		r, found, err := repairIn(ctx, tx, id, "")
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no repair with this id")
		}
		out = r
		return nil
	})
	return out, err
}

// FindingRepairs implements tenant.Repairs.
func (k Sending) FindingRepairs(ctx context.Context, findingID string) ([]tenant.Repair, error) {
	if err := master.ValidateID("finding id", findingID); err != nil {
		return nil, err
	}
	var out []tenant.Repair
	err := k.Host.Read(ctx, "the finding's repairs cannot be read", func(tx SendingTx) error {
		out = nil
		return tx.Query(ctx, `SELECT `+repairColumns+` FROM repairs WHERE finding_id = ? ORDER BY recorded_at, id LIMIT ?`,
			[]any{findingID, tenant.MaxFindingRepairs}, func(r Row) error {
				x, err := scanRepair(r)
				out = append(out, x)
				return err
			})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
