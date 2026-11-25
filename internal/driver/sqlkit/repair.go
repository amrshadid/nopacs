package sqlkit

import (
	"context"
	"maps"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The repairs of a primary's objects from a replica (tenant.Repairs), on
// the repairs table of each driver's tenant baseline schema, and the
// scrub's findings and marks they clear (scrub.go). A repair's hold of its
// scan is in repair_hold.go; a repair job's rows and counts in
// repair_job.go.

// repairColumns are a repair's columns as the store reads them.
const repairColumns = `id, finding_id, storage, object_key, sop_instance_uid, revision, from_replica, job_id, expected_size,
	expected_sha256, found_size, found_sha256, recorded_at, repaired_at, series_uid, outcome, hold_until`

// The outcomes a repair's row keeps once it ended.
const (
	repairOutcomeRepaired = "repaired"
	repairOutcomeChanged  = "changed"
)

// scanRepair reads one row of repairColumns.
func scanRepair(r Row) (tenant.Repair, error) {
	var out tenant.Repair
	var expected, found []byte
	var recorded, repaired, held DBTime
	var outcome string
	err := r.Scan(&out.ID, &out.Finding, &out.Storage, &out.Key, &out.SOPInstanceUID, &out.Revision, &out.From, &out.Job,
		&out.ExpectedSize, &expected, &out.FoundSize, &found, &recorded, &repaired, &out.SeriesUID, &outcome, &held)
	out.ExpectedSHA256, out.FoundSHA256 = ToSHA256(expected), ToSHA256(found)
	out.Recorded, out.Repaired, out.HeldUntil = recorded.T, repaired.T, held.T
	out.Changed = outcome == repairOutcomeChanged
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
		// The configuration's lock orders the record with a correction's
		// steps and a switch's start, which take it first too; the scan's
		// row, locked below, orders it with a reception's commit to the
		// scan, which updates that row before it looks for a hold.
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		var work, attempt, until any
		var end time.Time
		if rec.Fence != nil {
			var err error
			if end, err = k.holdEnd(ctx, tx, *rec.Fence); err != nil {
				return err
			}
			work, attempt, until = rec.Fence.ID, rec.Fence.Attempt, k.D.Time(end)
		}
		old, held, err := repairIn(ctx, tx, p.ID, k.D.Lock)
		if err != nil {
			return err
		}
		if held && old.Done() {
			out = old
			return nil
		}
		if held && (old.ExpectedSHA256 != p.ExpectedSHA256 || (old.SOPInstanceUID != "" && old.ExpectedSize != p.ExpectedSize)) {
			return tenant.Precondition(tenant.RepairCatalogChanged + ": the bytes expected are not those the repair was recorded with")
		}
		if held && old.HeldUntil.After(rec.Now) {
			// Another attempt's hold that has not run out: its worker may
			// still be putting this object, and a record under this fence
			// would let the hold end, by this attempt's end, while that put
			// may still land. It waits for that hold to run out.
			var oldWork string
			var oldAttempt int
			if _, err := tx.QueryRow(ctx, `SELECT open_work, open_attempt FROM repairs WHERE id = ?`, []any{p.ID},
				&oldWork, &oldAttempt); err != nil {
				return err
			}
			if rec.Fence == nil || oldWork != rec.Fence.ID || oldAttempt != rec.Fence.Attempt {
				return tenant.ScanRepairingError()
			}
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
		want := p
		want.Storage, want.Key, want.SOPInstanceUID, want.Revision = f.Storage, f.Key, f.SOPInstanceUID, f.Revision
		cat, series, err := repairCatalogIn(ctx, tx, f, k.D.Lock)
		if err != nil {
			return err
		}
		if err := tenant.CheckRepairCatalog(want, cat); err != nil {
			return err
		}
		if err := k.checkScanStill(ctx, tx, series, p.Storage, p.From); err != nil {
			return err
		}
		if held {
			// A write sent again under the same fence never moves its
			// hold's end sooner.
			if rec.Fence != nil && old.HeldUntil.After(end) {
				until = k.D.Time(old.HeldUntil)
			}
			if _, err := tx.Exec(ctx, `UPDATE repairs SET open_work = ?, open_attempt = ?, hold_until = ? WHERE id = ?`,
				work, attempt, until, p.ID); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `INSERT INTO repairs (`+repairColumns+`, open_work, open_attempt)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, '', ?, ?, ?)`,
				p.ID, f.ID, f.Storage, f.Key, f.SOPInstanceUID, f.Revision, p.From, p.Job, p.ExpectedSize, p.ExpectedSHA256[:],
				nullInt(p.FoundSize), nullSHA(p.FoundSHA256), k.D.Time(rec.Now), series, until, work, attempt); err != nil {
				return err
			}
			made = true
		}
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
		// The catalog read again: a hold that ended with its lease let the
		// scan change under the put, and bytes the catalog no longer names
		// clear nothing.
		cat, _, err := repairCatalogIn(ctx, tx, f, "")
		if err != nil {
			return err
		}
		outcome := repairOutcomeRepaired
		if tenant.CheckRepairCatalog(r, cat) != nil {
			outcome = repairOutcomeChanged
			audit.Details = maps.Clone(audit.Details)
			if audit.Details == nil {
				audit.Details = map[string]string{}
			}
			audit.Details["outcome"] = repairOutcomeChanged
			if !r.FoundExpected() {
				if err := k.recordStray(ctx, tx, r, f, now); err != nil {
					return err
				}
			}
		} else if f.Holds() {
			if _, err := k.clearInTx(ctx, tx, r.Job, f, now); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE repairs SET repaired_at = ?, outcome = ?, open_work = NULL, open_attempt = NULL, hold_until = NULL
			WHERE id = ?`, k.D.Time(now), outcome, id); err != nil {
			return err
		}
		out.Repaired, out.Changed, out.HeldUntil = now, outcome == repairOutcomeChanged, time.Time{}
		if outcome == repairOutcomeRepaired {
			kind := tenant.RepairRepaired
			if r.FoundExpected() {
				kind = tenant.RepairAlreadyGood
			}
			if err := k.countJobRepair(ctx, tx, r, kind, now); err != nil {
				return err
			}
		}
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

// RepairFindings implements tenant.Repairs.
func (k Sending) RepairFindings(ctx context.Context, ids []string) ([]tenant.ScrubFinding, error) {
	if len(ids) > tenant.MaxRepairFindings {
		return nil, fault.New(fault.Invalid, "at most 1000 findings are read at once")
	}
	for _, id := range ids {
		if err := master.ValidateID("finding id", id); err != nil {
			return nil, err
		}
	}
	var out []tenant.ScrubFinding
	err := k.Host.Read(ctx, "the findings cannot be read", func(tx SendingTx) error {
		out = nil
		for _, id := range ids {
			f, found, err := findingIn(ctx, tx, findingByIDSQL, id)
			if err != nil {
				return err
			}
			if found {
				out = append(out, f)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
