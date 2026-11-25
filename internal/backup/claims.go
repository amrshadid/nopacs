package backup

import (
	"context"
	"errors"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What a restore and a reconcile read of the storages' owner records
// before they write anything of their own, and how they judge one.

// judge says whether the owner record r, as read, lets the claim take the
// storage, and returns the generation to write one past. A record that
// names another installation or tenant than the claim expects, or a
// generation past the one held, says someone took the storage over after
// the file was made (another installation it was handed to, or a restore
// since, which may still be writing to it); so does a record naming this
// installation whose MAC does not verify under its key file, which this
// installation did not write. Either is refused unless the claim says to
// take it over, since two installations writing one storage each hide the
// other's writes. other says the refusal is of a record, read whole,
// naming an installation that is neither the one the claim expects nor
// this one: that installation is the storage's writer now, so taking it
// over is offered only for a claim known to be stale.
func (c ownerClaim) judge(r ownerRead) (gen int64, other bool, err error) {
	gen = c.held
	from, what, again, command := c.from, c.what, c.again, c.command
	if from == "" {
		from = c.installation
	}
	if what == "" {
		what = "the backup"
	}
	if again == "" {
		again = "run the restore again with --take-over, so this installation owns it (a restore interrupted after it took the storage " +
			"over is run again the same way)"
	}
	if command == "" {
		command = "the restore"
	}
	if !r.found {
		return gen, false, nil
	}
	if r.decodeErr != nil {
		if c.takeOver {
			return gen, false, nil
		}
		return 0, false, fault.Wrap(fault.Invalid, r.decodeErr, "the storage "+c.storage+"'s owner record cannot be read; "+again)
	}
	o := r.record
	forged := ""
	if o.Installation == c.installation && record.VerifyOwner(o, c.keys) != nil {
		forged = " (its MAC does not verify under this key file: this installation did not write it)"
	}
	names := "the storage " + c.storage + "'s owner record names installation " + output.Clean(o.Installation) + ", tenant " + output.Clean(o.Tenant) +
		", at owner generation " + strconv.FormatInt(o.Generation, 10) + forged + ", and " + what + " holds generation " +
		strconv.FormatInt(c.held, 10) + " for installation " + from + ", tenant " + c.tenantID
	switch {
	case c.takeOver:
	case o.Installation != from && o.Installation != c.installation:
		return 0, true, fault.New(fault.Conflict, names+": that installation took the storage over (a hand-off, or a restore of its "+
			"own) and writes it now. Leave the storage to it; only if you know its claim is stale (that installation is gone and will never "+
			"write the storage again), run "+command+" again with --take-over")
	case o.Installation != from || o.Tenant != c.tenantID || o.Generation > c.held || forged != "":
		return 0, false, fault.New(fault.Conflict, names+": another installation, or a restore made since, took it over. Stop whatever "+
			"uses it now, then "+again)
	}
	return max(gen, o.Generation), false, nil
}

// claimsOf reads the owner record of every storage of each tenant and
// judges it as takeOwner would, writing nothing, before a restore or a
// reconcile writes anything of its own (a key suspended, an owner record):
// a storage refused part way through the tenants would leave the storages
// before it taken over and the installation's keys suspended for a run
// that cannot end. It returns, by the tenants' order, the refusal of a
// record naming another installation (ownerClaim.judge's other), which
// leaves that tenant to the installation that writes it now; "" for a
// tenant whose every storage may be taken. Any other refusal, or a storage
// that cannot be read, is the error.
func claimsOf(ctx context.Context, cfg RestoreConfig, m master.Store, installation string, tenants []openedTenant) ([]string, error) {
	opener := place.Opener{Values: settingvalue.Reader{Master: m}, Keys: cfg.Keys, StaleUploadAge: -1}
	out := make([]string, len(tenants))
	for i, t := range tenants {
		if err := claimOf(ctx, cfg, opener, installation, t, &out[i]); err != nil {
			return out, prefixed(err, "backup: the tenant "+t.row.Name+" cannot be reconciled with its storages: ")
		}
	}
	return out, nil
}

// claimOf is claimsOf for one tenant, putting in other the refusal of a
// record naming another installation.
func claimOf(ctx context.Context, cfg RestoreConfig, opener place.Opener, installation string, t openedTenant, other *string) error {
	p, ok := t.store.(tenant.Pipeline)
	rs, ok2 := t.store.(tenant.Restoring)
	if !ok || !ok2 {
		return fault.New(fault.Internal, "backup: this database type cannot be reconciled")
	}
	held, err := rs.StorageOwners(ctx)
	if err != nil {
		return err
	}
	generations := map[string]int64{}
	for _, o := range held {
		generations[o.Storage] = o.Generation
	}
	storages, err := listStorages(ctx, p)
	if err != nil {
		return err
	}
	pt := place.TenantOf(t.row.ID, t.row.Name, t.store)
	for _, s := range storages {
		bs, err := opener.OpenForReading(ctx, pt, s)
		if err != nil {
			return prefixed(err, "the storage "+s.Name+" cannot be opened: ")
		}
		r, err := readOwner(ctx, bs, s.Name)
		_ = bs.Close()
		if err != nil {
			return err
		}
		_, isOther, err := restoreClaim(cfg, installation, t.row.ID, s.Name, generations[s.Name]).judge(r)
		if err != nil && isOther {
			*other = fault.Message(err)
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// fenceTenant leaves the tenant whose storage another installation writes
// fenced in the master (read only, serving no data) for why, with an audit
// entry of the restore's or the reconcile's, unless it is fenced already:
// neither takes its storages.
func fenceTenant(ctx context.Context, cfg RestoreConfig, m master.Store, row master.Tenant, why string, now time.Time) error {
	if row.State == master.StateFenced {
		return nil
	}
	user, method := restoreUser, restoreMethod
	if cfg.reconcileOnly {
		user, method = reconcileUser, reconcileMethod
	}
	reason := fitText(why, master.MaxStateReasonLength)
	e := master.AuditEntry{ID: server.NewID(), Time: now, LocalUser: user, Method: method, TenantName: row.Name,
		Object: "tenant/" + row.Name, Outcome: master.OutcomeDone,
		Reason: "the tenant left fenced, not reconciled: another installation's owner record is on one of its storages"}
	return m.SetTenantState(ctx, row.ID, master.StateFenced, reason, e, master.LocalOperator())
}

// fitText returns s cut to at most n bytes, at a character's start.
func fitText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// prefixed returns err with prefix put before its message, of its kind,
// wrapping what err wraps: wrapping err itself would repeat its message
// in the error's text (fault.Error.Error), once as the message and once
// as the cause's.
func prefixed(err error, prefix string) error {
	var e *fault.Error
	if errors.As(err, &e) {
		return fault.Wrap(e.Kind(), e.Unwrap(), prefix+e.Message())
	}
	return fault.Wrap(fault.Internal, err, prefix+fault.Message(err))
}

// endRebuilds ends every rebuild job the tenant's database holds not
// ended (tenant.Restoring.EndRebuilds) and drops the shadow of each, and
// of every rebuild ended before whose shadow is still there
// (tenant.Rebuilding.ShadowsLeft): a scratch schema still marked as
// another job's refuses the restore's rebuild, and its shadow is the whole
// catalog's UIDs and labels, left nowhere a restore does not know of.
func endRebuilds(ctx context.Context, rs tenant.Restoring, p tenant.Pipeline, now time.Time) error {
	ended, err := rs.EndRebuilds(ctx, now)
	if err != nil {
		return err
	}
	left, err := p.ShadowsLeft(ctx)
	if err != nil {
		return err
	}
	dropped := map[string]bool{}
	for _, id := range append(ended, left...) {
		if dropped[id] {
			continue
		}
		dropped[id] = true
		if err := p.DropShadow(ctx, id); err != nil {
			return prefixed(err, "the shadow of the rebuild job "+id+" cannot be removed: ")
		}
	}
	return nil
}
