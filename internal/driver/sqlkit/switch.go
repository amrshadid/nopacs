package sqlkit

import (
	"context"
	"slices"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The switches of a primary to its replica (tenant.Switching), on the
// storage_switches table of each driver's tenant baseline schema, and the
// one transaction that swaps two storages' places (SwapStorages). Every
// change takes the tenant's configuration lock first, so a switch, an
// attach, a detach and a change of a storage never pass each other.

// switchColumns are a switch's columns as the store reads them.
const switchColumns = `id, primary_name, replica_name, state, step, confirm_token, generation, skip_verification, skip_reason,
	key_id, accepted_at, swapped_at, ended_at, refusal, scans_drained, associations_refused, pause_us`

// scanSwitch reads one row of switchColumns.
func scanSwitch(r Row) (tenant.StorageSwitch, error) {
	var s tenant.StorageSwitch
	var state, step string
	var pause int64
	var accepted, swapped, ended DBTime
	err := r.Scan(&s.ID, &s.Primary, &s.Replica, &state, &step, &s.Token, &s.Generation, &s.SkipVerification, &s.SkipReason,
		&s.KeyID, &accepted, &swapped, &ended, &s.Refusal, &s.ScansDrained, &s.AssociationsRefused, &pause)
	s.State, s.Step, s.Pause = tenant.SwitchState(state), tenant.SwitchStepName(step), time.Duration(pause)*time.Microsecond
	s.Accepted, s.Swapped, s.Ended = accepted.T, swapped.T, ended.T
	return s, err
}

// switchWhere reads the first switch the condition keeps, in tx, and
// whether there is one.
func switchWhere(ctx context.Context, tx SendingTx, where string, args ...any) (tenant.StorageSwitch, bool, error) {
	var out tenant.StorageSwitch
	found := false
	err := tx.Query(ctx, `SELECT `+switchColumns+` FROM storage_switches WHERE `+where, args, func(r Row) error {
		if found {
			return nil
		}
		var err error
		out, err = scanSwitch(r)
		found = true
		return err
	})
	return out, found, err
}

// receiversArchivingTo returns the names of the receivers whose storage, or
// a route of whose, is the storage named, in order.
func receiversArchivingTo(ctx context.Context, tx SendingTx, storage string) ([]string, error) {
	var out []string
	err := tx.Query(ctx, `SELECT name, storage, config FROM receivers ORDER BY name`, nil, func(r Row) error {
		var name, own, config string
		if err := r.Scan(&name, &own, &config); err != nil {
			return err
		}
		var c dimseConfig
		if err := FromJSON(config, &c); err != nil {
			return err
		}
		if own == storage || slices.Contains(routingOf(c).Storages(), storage) {
			out = append(out, name)
		}
		return nil
	})
	return out, err
}

// switchFactsIn reads SwitchFacts in tx, the storages locked with lock.
func (k Sending) switchFactsIn(ctx context.Context, tx SendingTx, primary, replica, lock string) (tenant.SwitchFacts, error) {
	var f tenant.SwitchFacts
	var found bool
	var err error
	for _, x := range []struct {
		name string
		to   *tenant.Storage
	}{{primary, &f.Primary}, {replica, &f.Replica}} {
		if *x.to, found, err = storageIn(ctx, tx, x.name, lock); err != nil {
			return f, err
		}
		if !found {
			return f, notFound("the tenant has no storage named " + x.name)
		}
	}
	if _, err := tx.QueryRow(ctx, `SELECT count(*) FROM scans AS s LEFT JOIN replica_scans AS r ON r.replica = ?
		AND r.series_uid = s.series_uid WHERE s.storage = ? AND (r.series_uid IS NULL OR r.state <> 'in_step' OR NOT `+sameManifest+`)`,
		[]any{replica, primary}, &f.ScansNotInStep); err != nil {
		return f, err
	}
	if _, err := tx.QueryRow(ctx, `SELECT count(*) FROM scrub_findings WHERE storage = ? AND cleared_at IS NULL`, []any{replica},
		&f.ReplicaFindings); err != nil {
		return f, err
	}
	if f.CorrectionPrepared, err = exists(ctx, tx, `SELECT 1 FROM corrections AS c JOIN correction_scans AS cs ON cs.correction_id = c.id
		WHERE c.state = 'prepared' AND cs.storage = ?`, primary); err != nil {
		return f, err
	}
	if f.RebuildRunning, err = exists(ctx, tx, rebuildNotEnded+k.D.NotEnded); err != nil {
		return f, err
	}
	running, found, err := switchWhere(ctx, tx, `state IN ('accepted', 'swapped') ORDER BY accepted_at, id`)
	if err != nil {
		return f, err
	}
	if found {
		f.Running = &running
	}
	if f.Receivers, err = receiversArchivingTo(ctx, tx, primary); err != nil {
		return f, err
	}
	var done DBTime
	if _, err := tx.QueryRow(ctx, `SELECT backfill_done_at FROM replica_totals WHERE replica = ?`, []any{replica}, &done); err != nil {
		return f, err
	}
	f.BackfillDone = done.T
	return f, nil
}

// checkNoSwitch refuses the correction of id, its scans written in tx, while
// a switch of the storage of one of them runs (accepted or swapped): a
// relabel's copies, a replace's new revision, made on the primary's place
// while the switch drains, would stay on the place the swap gives the
// replica's name. The caller holds the tenant's configuration lock, which
// BeginSwitch takes too, so the two never pass each other.
func checkNoSwitch(ctx context.Context, tx SendingTx, id string) error {
	busy, err := exists(ctx, tx, `SELECT 1 FROM correction_scans AS cs JOIN storage_switches AS w ON w.primary_name = cs.storage
		OR w.replica_name = cs.storage WHERE cs.correction_id = ? AND w.state IN ('accepted', 'swapped')`, id)
	if err != nil {
		return err
	}
	if busy {
		return tenant.Precondition("a switch of the target's storage is running")
	}
	return nil
}

// SwitchFacts implements tenant.Switching.
func (k Sending) SwitchFacts(ctx context.Context, primary, replica string) (tenant.SwitchFacts, error) {
	if err := tenant.ValidateName("storage", primary); err != nil {
		return tenant.SwitchFacts{}, err
	}
	if err := tenant.ValidateName("storage", replica); err != nil {
		return tenant.SwitchFacts{}, err
	}
	var out tenant.SwitchFacts
	err := k.Host.Read(ctx, "the switch's facts cannot be read", func(tx SendingTx) error {
		var err error
		out, err = k.switchFactsIn(ctx, tx, primary, replica, "")
		return err
	})
	return out, err
}

// BeginSwitch implements tenant.Switching.
func (k Sending) BeginSwitch(ctx context.Context, s tenant.SwitchStart, audit master.AuditEntry, guard tenant.Guard) (tenant.StorageSwitch, bool, error) {
	if err := s.Validate(); err != nil {
		return tenant.StorageSwitch{}, false, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.StorageSwitch{}, false, err
	}
	var out tenant.StorageSwitch
	var made bool
	err := k.Host.Write(ctx, "the switch cannot be begun", func(tx SendingTx) error {
		made = false
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		used, found, err := switchWhere(ctx, tx, `confirm_token = ?`, s.Token)
		if err != nil {
			return err
		}
		if found {
			out = used
			return tx.Audited(ctx, audit, guard)
		}
		f, err := k.switchFactsIn(ctx, tx, s.Primary, s.Replica, k.D.Lock)
		if err != nil {
			return err
		}
		if err := tenant.CheckSwitch(f); err != nil {
			return err
		}
		taken, err := exists(ctx, tx, `SELECT 1 FROM storage_switches WHERE id = ?`, s.ID)
		if err != nil {
			return err
		}
		if taken {
			return fault.New(fault.Conflict, "a switch with this id exists")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO storage_switches (`+switchColumns+`) VALUES (?, ?, ?, 'accepted', '', ?, ?, ?, ?, ?, ?,
			NULL, NULL, '', 0, 0, 0)`, s.ID, s.Primary, s.Replica, s.Token, f.Primary.SwitchGeneration, k.D.Bool(s.SkipVerification),
			s.SkipReason, audit.KeyID, k.D.Time(s.Now)); err != nil {
			return err
		}
		made = true
		if out, _, err = switchWhere(ctx, tx, `id = ?`, s.ID); err != nil {
			return err
		}
		return tx.Audited(ctx, audit, guard)
	})
	if err != nil {
		return tenant.StorageSwitch{}, false, err
	}
	return out, made, nil
}

// lockSwitch reads, locked, the switch of id; NotFound when there is none.
func (k Sending) lockSwitch(ctx context.Context, tx SendingTx, id string) (tenant.StorageSwitch, error) {
	s, found, err := switchWhere(ctx, tx, `id = ?`+k.D.Lock, id)
	if err != nil {
		return s, err
	}
	if !found {
		return s, notFound("the tenant has no switch with this id")
	}
	return s, nil
}

// SwitchStep implements tenant.Switching.
func (k Sending) SwitchStep(ctx context.Context, id string, step tenant.SwitchStepName, now time.Time) (tenant.StorageSwitch, error) {
	if err := master.ValidateID("switch id", id); err != nil {
		return tenant.StorageSwitch{}, err
	}
	if step != tenant.SwitchPausing && step != tenant.SwitchDraining && step != tenant.SwitchOwning {
		return tenant.StorageSwitch{}, fault.New(fault.Invalid, "a switch's step is pausing, draining or owning")
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.StorageSwitch{}, err
	}
	var out tenant.StorageSwitch
	err := k.Host.Write(ctx, "the switch's step cannot be recorded", func(tx SendingTx) error {
		s, err := k.lockSwitch(ctx, tx, id)
		if err != nil {
			return err
		}
		if s.State != tenant.SwitchAccepted {
			return tenant.Precondition("the switch is " + string(s.State) + "; only an accepted switch takes a step")
		}
		if _, err := tx.Exec(ctx, `UPDATE storage_switches SET step = ? WHERE id = ?`, string(step), id); err != nil {
			return err
		}
		s.Step = step
		out = s
		return nil
	})
	return out, err
}

// SwapStorages implements tenant.Switching.
func (k Sending) SwapStorages(ctx context.Context, sw tenant.SwitchSwap, audit master.AuditEntry) (tenant.StorageSwitch, error) {
	if err := sw.Validate(); err != nil {
		return tenant.StorageSwitch{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.StorageSwitch{}, err
	}
	var out tenant.StorageSwitch
	err := k.Host.Write(ctx, "the storages cannot be swapped", func(tx SendingTx) error {
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		s, err := k.lockSwitch(ctx, tx, sw.ID)
		if err != nil {
			return err
		}
		out = s
		switch s.State {
		case tenant.SwitchSwapped, tenant.SwitchDone:
			return nil
		case tenant.SwitchRefused:
			return tenant.Precondition("the switch was refused: " + s.Refusal)
		}
		f, err := k.switchFactsIn(ctx, tx, s.Primary, s.Replica, k.D.Lock)
		if err != nil {
			return err
		}
		switch {
		case f.Replica.ReplicaOf != f.Primary.Name:
			return tenant.Precondition(tenant.BlockerNotReplica)
		case f.Replica.Tier.Archive():
			return tenant.Precondition(tenant.BlockerArchive)
		case f.Primary.SwitchGeneration != s.Generation:
			return tenant.Precondition("the primary's place changed since the switch was accepted")
		case f.ScansNotInStep > 0:
			return tenant.Precondition(tenant.BlockerNotInStep(f.ScansNotInStep))
		case f.ReplicaFindings > 0:
			return tenant.Precondition(tenant.BlockerFinding)
		// A correction or a rebuild made after the switch began would write
		// to the primary's old place, or a catalog read from it, once the
		// swap moved the name; the begin refuses one already there, this
		// one made since.
		case f.CorrectionPrepared:
			return tenant.Precondition(tenant.BlockerCorrectionPrepared)
		case f.RebuildRunning:
			return tenant.Precondition(tenant.BlockerRebuild)
		}
		if err := k.swapPlaces(ctx, tx, s.Primary, s.Replica, sw); err != nil {
			return err
		}
		if err := k.recordInStep(ctx, tx, s.Primary, s.Replica, sw.Now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE storage_switches SET state = 'swapped', swapped_at = ?, scans_drained = ?,
			associations_refused = ?, pause_us = ? WHERE id = ?`, k.D.Time(sw.Now), sw.ScansDrained, sw.AssociationsRefused,
			sw.Pause.Microseconds(), sw.ID); err != nil {
			return err
		}
		if out, err = k.lockSwitch(ctx, tx, sw.ID); err != nil {
			return err
		}
		e, err := tenant.SwitchEvent(out, f.Primary.SwitchGeneration+1, sw.Now)
		if err != nil {
			return err
		}
		if err := tx.WriteEvents(ctx, e); err != nil {
			return err
		}
		return tx.Audited(ctx, audit, nil)
	})
	if err != nil {
		return tenant.StorageSwitch{}, err
	}
	return out, nil
}

// RefuseSwitch implements tenant.Switching.
func (k Sending) RefuseSwitch(ctx context.Context, id, refusal string, now time.Time, audit master.AuditEntry) (tenant.StorageSwitch, error) {
	if err := master.ValidateID("switch id", id); err != nil {
		return tenant.StorageSwitch{}, err
	}
	if err := tenant.ValidateRefusal(refusal); err != nil {
		return tenant.StorageSwitch{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.StorageSwitch{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.StorageSwitch{}, err
	}
	var out tenant.StorageSwitch
	err := k.Host.Write(ctx, "the switch cannot be refused", func(tx SendingTx) error {
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		s, err := k.lockSwitch(ctx, tx, id)
		if err != nil {
			return err
		}
		out = s
		switch s.State {
		case tenant.SwitchRefused:
			return nil
		case tenant.SwitchSwapped, tenant.SwitchDone:
			return tenant.Precondition("the switch's swap committed; it stands")
		}
		if _, err := tx.Exec(ctx, `UPDATE storage_switches SET state = 'refused', refusal = ?, ended_at = ? WHERE id = ?`, refusal,
			k.D.Time(now), id); err != nil {
			return err
		}
		out.State, out.Refusal, out.Ended = tenant.SwitchRefused, refusal, now
		var generation int64
		if _, err := tx.QueryRow(ctx, `SELECT coalesce((SELECT generation FROM storage_generations WHERE storage = ?), 0)`,
			[]any{s.Primary}, &generation); err != nil {
			return err
		}
		e, err := tenant.SwitchEvent(out, generation, now)
		if err != nil {
			return err
		}
		if err := tx.WriteEvents(ctx, e); err != nil {
			return err
		}
		return tx.Audited(ctx, audit, nil)
	})
	if err != nil {
		return tenant.StorageSwitch{}, err
	}
	return out, nil
}

// FinishSwitch implements tenant.Switching.
func (k Sending) FinishSwitch(ctx context.Context, id string, now time.Time) (tenant.StorageSwitch, error) {
	if err := master.ValidateID("switch id", id); err != nil {
		return tenant.StorageSwitch{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.StorageSwitch{}, err
	}
	var out tenant.StorageSwitch
	err := k.Host.Write(ctx, "the switch cannot be finished", func(tx SendingTx) error {
		s, err := k.lockSwitch(ctx, tx, id)
		if err != nil {
			return err
		}
		out = s
		switch s.State {
		case tenant.SwitchDone:
			return nil
		case tenant.SwitchSwapped:
		default:
			return tenant.Precondition("the switch is " + string(s.State) + "; only a swapped switch is finished")
		}
		if _, err := tx.Exec(ctx, `UPDATE storage_switches SET state = 'done', ended_at = ? WHERE id = ?`, k.D.Time(now), id); err != nil {
			return err
		}
		out.State, out.Ended = tenant.SwitchDone, now
		return nil
	})
	return out, err
}

// Switch implements tenant.Switching.
func (k Sending) Switch(ctx context.Context, id string) (tenant.StorageSwitch, error) {
	if err := master.ValidateID("switch id", id); err != nil {
		return tenant.StorageSwitch{}, err
	}
	var out tenant.StorageSwitch
	err := k.Host.Read(ctx, "the switch cannot be read", func(tx SendingTx) error {
		s, found, err := switchWhere(ctx, tx, `id = ?`, id)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no switch with this id")
		}
		out = s
		return nil
	})
	return out, err
}

// UnfinishedSwitches implements tenant.Switching.
func (k Sending) UnfinishedSwitches(ctx context.Context) ([]tenant.StorageSwitch, error) {
	var out []tenant.StorageSwitch
	err := k.Host.Read(ctx, "the switches running cannot be read", func(tx SendingTx) error {
		out = nil
		return tx.Query(ctx, `SELECT `+switchColumns+` FROM storage_switches WHERE state IN ('accepted', 'swapped')
			ORDER BY accepted_at, id`, nil, func(r Row) error {
			s, err := scanSwitch(r)
			out = append(out, s)
			return err
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LastSwitch implements tenant.Switching.
func (k Sending) LastSwitch(ctx context.Context, storage string) (tenant.StorageSwitch, bool, error) {
	if err := tenant.ValidateName("storage", storage); err != nil {
		return tenant.StorageSwitch{}, false, err
	}
	var out tenant.StorageSwitch
	var found bool
	err := k.Host.Read(ctx, "the storage's last switch cannot be read", func(tx SendingTx) error {
		var err error
		out, found, err = switchWhere(ctx, tx, `primary_name = ? OR replica_name = ? ORDER BY accepted_at DESC, id DESC LIMIT 1`,
			storage, storage)
		return err
	})
	return out, found, err
}
