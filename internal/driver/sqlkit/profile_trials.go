package sqlkit

import (
	"context"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// RecordProfileTrial implements tenant.ProfileTrials.
func (k Deidentification) RecordProfileTrial(ctx context.Context, t tenant.ProfileTrial, audit master.AuditEntry, guard tenant.Guard) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if err := audit.Validate(); err != nil {
		return err
	}
	return k.Host.Write(ctx, "the profile trial cannot be stored", func(tx SendingTx) error {
		var found int64
		ok, err := tx.QueryRow(ctx, `SELECT version FROM profile_versions WHERE name = ? AND version = ?`,
			[]any{t.Profile.Name, t.Profile.Version}, &found)
		if err != nil {
			return err
		}
		if !ok {
			return fault.New(fault.NotFound, "the tenant has no such profile version")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO profile_trials (receiver, profile_name, profile_version, tried, key_id, sessions, files,
			changed, held) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (receiver, profile_name, profile_version) DO UPDATE SET tried = excluded.tried, key_id = excluded.key_id,
			sessions = excluded.sessions, files = excluded.files, changed = excluded.changed, held = excluded.held`,
			t.Receiver, t.Profile.Name, t.Profile.Version, k.D.Time(t.Tried), t.KeyID, t.Sessions, t.Files, t.Changed, t.Held); err != nil {
			return err
		}
		return tx.Audited(ctx, audit, guard)
	})
}

// ProfileTrial implements tenant.ProfileTrials.
func (k Deidentification) ProfileTrial(ctx context.Context, receiver string, ref tenant.ProfileRef) (tenant.ProfileTrial, error) {
	if err := tenant.ValidateName("receiver", receiver); err != nil {
		return tenant.ProfileTrial{}, err
	}
	var out tenant.ProfileTrial
	var tried DBTime
	err := k.Host.Read(ctx, "the profile trial cannot be read", func(tx SendingTx) error {
		ok, err := tx.QueryRow(ctx, `SELECT tried, key_id, sessions, files, changed, held FROM profile_trials
			WHERE receiver = ? AND profile_name = ? AND profile_version = ?`, []any{receiver, ref.Name, ref.Version}, &tried, &out.KeyID,
			&out.Sessions, &out.Files, &out.Changed, &out.Held)
		if err != nil {
			return err
		}
		if !ok {
			return fault.New(fault.NotFound, "the receiver was never tried with this profile version")
		}
		return nil
	})
	if err != nil {
		return tenant.ProfileTrial{}, err
	}
	out.Receiver, out.Profile, out.Tried = receiver, ref, tried.T
	return out, nil
}
