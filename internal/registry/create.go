package registry

import (
	"context"
	"crypto/subtle"
	"errors"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// NewTenant is a tenant to create.
type NewTenant struct {
	// ID is the tenant's id, made by the caller, so a repeated request
	// names the same tenant.
	ID string
	// Name is the tenant's name (master.ValidateTenantName).
	Name string
	// Driver is the database type, one of Config.Drivers.
	Driver string
	// Address is the database's address with its password; empty for a
	// type that needs none (SQLite). It is sealed before it is stored and
	// appears in nothing the registry returns, logs or records.
	Address string
}

// Create creates a tenant, whole or not at all, as the API contract's
// CreateTenant says, and audits the request by how it ended.
//
// It writes the tenant's row as upgrading together with a system audit
// entry saying the creation started (StartMethod), which carries the
// request's id so the two can be joined; then it opens and migrates the
// tenant's database. When the tenant is ready, the state write that records
// it carries the request's own entry, audit, with outcome done. When the
// database cannot be opened or upgraded, the removal of the row carries the
// request's entry with outcome failed, the reason (the registry's phrases and
// the driver's safe message) and the detail undone; the tenant is
// unregistered and the failure returned: Unavailable when the database does
// not answer or its tables cannot be made, Invalid when it refuses the
// address or its file, Internal for the rest (an address that does not open
// with this key file, a driver that panicked). So the request's entry is
// written once, with its real outcome, in the same transaction as the change
// that settles the creation. Create sets the entry's outcome and reason, the
// caller's being replaced, and its tenant name when the caller left it
// empty. If the row cannot be removed (the master does not answer), the
// tenant stays registered in the state the master has, the request's entry
// is not written, and the error says so.
//
// by is who asks for the creation. The master checks it in the transaction
// that writes the row (master.Store), so a key revoked, expired or stripped
// of its role since the request was checked writes nothing, and Create
// returns the master's refusal (master.RefusalOf finds it). Once the row is
// written the creation is under way, and the writes that settle it (ready
// with the request's entry, or the removal that undoes it) are the server's
// own, as they are when a start finishes a creation a crash interrupted: a
// revocation while the database is being opened does not leave a creation
// half made. A repeat, which writes no row, has by checked in the state write
// that records it ready, when there is one.
//
// It reports whether it wrote the request's entry, with whatever outcome, so
// the caller records the request itself exactly when Create did not: a
// creation refused or failing before anything was written, a refusal of by,
// or an entry the master could not take.
//
// It is safe to repeat. Only a crash after the row is written leaves an
// upgrading row and the started entry, and the next start finishes it with a
// system entry naming the request that began it (DetailCreationRequest). A
// Create again with the same id, name, database type and address finishes
// the first one's work, or finds it done, and records its own request (with
// the state write that makes the tenant ready, or with AppendAudit when
// there is nothing to write). A tenant of that name with another id, type or
// address is Conflict. A repeat that cannot open the database records its
// request as failed and leaves the row, which it did not write.
func (r *Registry) Create(ctx context.Context, nt NewTenant, audit master.AuditEntry, by master.Actor) (master.Tenant, bool, error) {
	if err := r.enter(); err != nil {
		return master.Tenant{}, false, err
	}
	defer r.wg.Done()
	if err := master.ValidateID("tenant id", nt.ID); err != nil {
		return master.Tenant{}, false, err
	}
	if err := master.ValidateTenantName(nt.Name); err != nil {
		return master.Tenant{}, false, err
	}
	if err := by.Validate(); err != nil {
		return master.Tenant{}, false, err
	}
	drv, ok := r.cfg.Drivers[nt.Driver]
	if !ok {
		return master.Tenant{}, false, fault.New(fault.Invalid, "this server has no driver for the database type "+nt.Driver)
	}
	// The address is checked by its driver before anything is written, so
	// an address the driver refuses never leaves a row behind. The driver's
	// error is replaced by a fixed message of its kind: whatever it says,
	// it was handed the address.
	if _, err := drv.Location(nt.Name, nt.Address); err != nil {
		return master.Tenant{}, false, fault.New(fault.KindOf(err), "the database address is not one the database type "+nt.Driver+" takes")
	}
	audit = withDetails(audit, map[string]string{DetailDatabaseType: nt.Driver})
	audit.Outcome, audit.Reason = master.OutcomeDone, ""
	if audit.TenantName == "" {
		audit.TenantName = nt.Name
	}
	// The request's entry is written later, with a state change or a
	// removal that add their details; it is checked now, at its largest, so
	// nothing is written for an entry the master would refuse at the end.
	if err := audit.ValidateWith(master.DetailOldState, master.DetailNewState, DetailUndone); err != nil {
		return master.Tenant{}, false, err
	}
	r.listing.RLock()
	defer r.listing.RUnlock()
	defer r.names.lock(nt.Name)()

	row, wrote, err := r.findOrWrite(ctx, nt, audit, by)
	if err != nil {
		return master.Tenant{}, false, err
	}
	if r.afterRow != nil {
		if err := r.afterRow(); err != nil {
			return master.Tenant{}, false, err
		}
	}

	e := r.register(row)
	if e == nil {
		if r.closed.Load() {
			return master.Tenant{}, false, fault.New(fault.Unavailable, "the server is shutting down")
		}
		return master.Tenant{}, false, fault.New(fault.Conflict, "a tenant named "+nt.Name+" is being replaced; try again")
	}
	e.op.Lock()
	defer e.op.Unlock()
	if e.removed {
		return master.Tenant{}, false, fault.New(fault.NotFound, "the tenant was removed while it was being created")
	}
	// The state write that settles the creation is this call's, with the
	// request's entry, so the open does not write one of its own.
	e.holdWrite = true
	if e.store == nil || e.view.Load().state != master.StateReady {
		r.refresh(e, true)
	}
	e.holdWrite = false
	v := e.view.Load()
	if v.state == master.StateReady {
		// The row this call wrote was checked against by; the write that
		// settles it is the server's. A repeat wrote no row, so its own
		// state write is where by is checked.
		settleBy := master.Server()
		if !wrote {
			settleBy = by
		}
		err := r.recordReady(ctx, e, audit, settleBy)
		if err == nil {
			row.State, row.StateReason = v.state, v.reason
			return row, true, nil
		}
		if fault.KindOf(err) == fault.NotFound {
			r.unregister(e)
			r.retireLocked(e)
			return master.Tenant{}, false, fault.New(fault.NotFound, "the tenant was removed while it was being created")
		}
		if _, refused := master.RefusalOf(err); refused {
			// The repeat may no longer act: the request is the caller's to
			// record. The master's state follows the view, as on any check.
			r.settle(e, v.state, v.reason)
			return master.Tenant{}, false, err
		}
		// The master did not record it ready: the creation did not settle,
		// so it is undone as a failure.
		e.lastKind = fault.Unavailable
		v = &view{state: master.StateUnreachable, reason: fitReason("the master did not record the tenant as ready: " + fault.Message(err))}
	}
	kind := e.lastKind
	if kind == fault.Internal && v.state == master.StateUnreachable {
		kind = fault.Unavailable
	}
	failed := audit
	failed.Outcome, failed.Reason = master.OutcomeFailed, fitAuditReason(v.reason)
	if !wrote {
		// A repeat leaves the row it did not write; the master's state
		// follows the view as on any check.
		r.settle(e, v.state, v.reason)
		recorded := true
		if err := r.recordRequest(ctx, failed); err != nil {
			recorded = false
			r.log.Warn("a failed creation's request could not be recorded", "tenant", e.name,
				"kind", fault.KindOf(err).String(), "message", fault.Message(err))
		}
		return master.Tenant{}, recorded, fault.New(kind, "the tenant's database cannot be used: "+v.reason)
	}
	recorded, err := r.undo(ctx, e, failed.WithDetails(DetailUndone, "true"))
	if err != nil {
		return master.Tenant{}, false, fault.New(kind, "the tenant was not created, and its row could not be removed ("+
			fault.Message(err)+"); it is "+string(v.state)+": "+v.reason)
	}
	return master.Tenant{}, recorded, fault.New(kind, "the tenant was not created: "+v.reason)
}

// findOrWrite returns the tenant nt names, and whether this call wrote its
// row. A new row is written as upgrading with a system entry saying the
// creation started, carrying the request's id and its entry's id; the
// request's own entry is written when the creation settles. A tenant that
// exists with the same id, type and address is a repeat: its row is
// returned. A row whose write returned an error but committed (the master
// answered late) is found by reading the master again, so it is not left
// behind unopened. The row is written by by, whom the master checks in the
// same transaction; a refusal is returned as it is.
func (r *Registry) findOrWrite(ctx context.Context, nt NewTenant, audit master.AuditEntry, by master.Actor) (master.Tenant, bool, error) {
	row, err := r.cfg.Master.TenantByName(ctx, nt.Name)
	switch {
	case err == nil:
		if err := r.repeat(row, nt); err != nil {
			return master.Tenant{}, false, err
		}
		return row, false, nil
	case fault.KindOf(err) != fault.NotFound:
		return master.Tenant{}, false, err
	}
	sealed, err := r.cfg.Keys.Seal(addressContext(nt.ID), []byte(nt.Address))
	if err != nil {
		return master.Tenant{}, false, err
	}
	row = master.Tenant{
		ID:            nt.ID,
		Name:          nt.Name,
		Driver:        nt.Driver,
		SealedAddress: sealed,
		State:         master.StateUpgrading,
		Created:       r.cfg.Now(),
	}
	started := master.AuditEntry{
		ID:         r.cfg.NewID(),
		Time:       r.cfg.Now(),
		Method:     StartMethod,
		TenantName: nt.Name,
		Object:     "tenant/" + nt.Name,
		Outcome:    master.OutcomeDone,
		RequestID:  audit.RequestID,
		Details: map[string]string{
			DetailActor:        ActorSystem,
			DetailDatabaseType: nt.Driver,
			DetailRequestEntry: audit.ID,
		},
	}
	werr := r.cfg.Master.CreateTenant(ctx, row, started, by)
	if werr == nil {
		return row, true, nil
	}
	if _, refused := master.RefusalOf(werr); refused {
		// Refused before anything was written: there is no row to find.
		return master.Tenant{}, false, werr
	}
	existing, rerr := r.cfg.Master.TenantByName(ctx, nt.Name)
	if rerr != nil {
		return master.Tenant{}, false, werr
	}
	if fault.KindOf(werr) == fault.Conflict {
		// Another creation of the same name wrote first.
		if err := r.repeat(existing, nt); err != nil {
			return master.Tenant{}, false, err
		}
		return existing, false, nil
	}
	if existing.ID == nt.ID {
		// The write committed although the call failed: this call wrote it.
		return existing, true, nil
	}
	return master.Tenant{}, false, werr
}

// repeat returns nil when row is the tenant nt asks for, so a creation that
// finds it is a repeat, and Conflict otherwise. The addresses are compared
// in constant time, after the stored one is opened, so a repeat with another
// address is refused instead of silently keeping the old one.
func (r *Registry) repeat(row master.Tenant, nt NewTenant) error {
	if row.ID != nt.ID || row.Driver != nt.Driver {
		return fault.New(fault.Conflict, "a tenant named "+nt.Name+" exists")
	}
	stored, err := r.cfg.Keys.Open(addressContext(row.ID), row.SealedAddress)
	if err != nil {
		return fault.New(fault.Internal, "the database address of the tenant "+nt.Name+" does not open with this key file")
	}
	defer clear(stored)
	if subtle.ConstantTimeCompare(stored, []byte(nt.Address)) != 1 {
		return fault.New(fault.Conflict, "a tenant named "+nt.Name+" exists with another database address")
	}
	return nil
}

// recordReady writes the tenant's state as ready with the request's entry,
// by by, in one master transaction, or, when the master already holds ready
// (a repeat of a finished creation), records the request alone. The caller
// holds e.op.
func (r *Registry) recordReady(ctx context.Context, e *entry, audit master.AuditEntry, by master.Actor) error {
	if e.written == master.StateReady && e.writtenReason == "" {
		return r.recordRequest(ctx, audit)
	}
	if err := r.cfg.Master.SetTenantState(ctx, e.id, master.StateReady, "", audit, by); err != nil {
		return err
	}
	e.written, e.writtenReason, e.lastWrite = master.StateReady, "", r.cfg.Now()
	return nil
}

// recordRequest writes a request's entry that goes with no change. An entry
// whose id is taken was written by the request being repeated
// (master.ErrDuplicateAuditID), so it is already recorded.
func (r *Registry) recordRequest(ctx context.Context, audit master.AuditEntry) error {
	err := r.cfg.Master.AppendAudit(ctx, audit)
	if errors.Is(err, master.ErrDuplicateAuditID) {
		return nil
	}
	return err
}

// undo removes the row a failed creation wrote, in one master transaction
// with the request's entry (outcome failed, the reason, the detail undone),
// as the server, and retires the entry. A row already gone has its request
// recorded alone. It reports whether the request's entry was written. The
// caller holds e.op. On another error the entry stays registered, as the
// master still has its row.
func (r *Registry) undo(ctx context.Context, e *entry, failed master.AuditEntry) (bool, error) {
	recorded := true
	err := r.cfg.Master.RemoveTenant(ctx, e.id, failed, master.Server())
	switch {
	case err == nil:
	case fault.KindOf(err) == fault.NotFound:
		if rerr := r.recordRequest(ctx, failed); rerr != nil {
			recorded = false
			r.log.Warn("a failed creation's request could not be recorded", "tenant", e.name,
				"kind", fault.KindOf(rerr).String(), "message", fault.Message(rerr))
		}
	default:
		r.log.Warn("a failed creation could not be undone", "tenant", e.name,
			"kind", fault.KindOf(err).String(), "message", fault.Message(err))
		return false, err
	}
	r.unregister(e)
	r.retireLocked(e)
	return recorded, nil
}

// fitAuditReason cuts a text to the length an audit entry's reason may have.
func fitAuditReason(s string) string {
	if len(s) <= master.MaxAuditReasonLength {
		return s
	}
	return fitReason(s[:master.MaxAuditReasonLength])
}

// Remove unregisters the tenant named name and removes its row and grants
// from the master, with the caller's audit entry, to which it adds the
// database type and the data's location. It leaves the tenant's data where it
// is, so a mistaken removal loses no data, and says where. Its store is
// closed after the row is gone; a request holding it fails as Unavailable
// from then on. When the master has no tenant of that name, which a removal
// that committed and then failed leaves behind, any entry of that name is
// unregistered and closed too, and the answer is NotFound. by is who asks for
// the removal; the master checks it in the removal's transaction, and a
// refusal removes nothing.
func (r *Registry) Remove(ctx context.Context, name string, audit master.AuditEntry, by master.Actor) (Removed, error) {
	if err := r.enter(); err != nil {
		return Removed{}, err
	}
	defer r.wg.Done()
	if err := master.ValidateTenantName(name); err != nil {
		return Removed{}, err
	}
	if err := by.Validate(); err != nil {
		return Removed{}, err
	}
	r.listing.RLock()
	defer r.listing.RUnlock()
	defer r.names.lock(name)()
	row, err := r.cfg.Master.TenantByName(ctx, name)
	if err != nil {
		if fault.KindOf(err) == fault.NotFound {
			r.retireName(name)
		}
		return Removed{}, err
	}
	out := Removed{Driver: row.Driver}
	if drv, ok := r.cfg.Drivers[row.Driver]; ok {
		if address, err := r.cfg.Keys.Open(addressContext(row.ID), row.SealedAddress); err == nil {
			if loc, err := drv.Location(name, string(address)); err == nil {
				out.Location = loc
			}
			clear(address)
		}
	}
	details := map[string]string{DetailDatabaseType: row.Driver}
	if len(out.Location) <= master.MaxAuditDetailValueLength {
		details[DetailLocation] = out.Location
	}
	if err := r.cfg.Master.RemoveTenant(ctx, row.ID, withDetails(audit, details), by); err != nil {
		if fault.KindOf(err) == fault.NotFound {
			r.retireName(name)
		}
		return Removed{}, err
	}
	r.retireName(name)
	return out, nil
}

// withDetails returns a copy of audit whose details also hold add, without
// replacing a detail the caller set and without going past the bounds of an
// entry, so the master never refuses the change for a detail the registry
// added.
func withDetails(audit master.AuditEntry, add map[string]string) master.AuditEntry {
	details := make(map[string]string, len(audit.Details)+len(add))
	for k, v := range audit.Details {
		details[k] = v
	}
	for k, v := range add {
		if _, set := details[k]; !set && len(details) < master.MaxAuditDetails {
			details[k] = v
		}
	}
	audit.Details = details
	return audit
}
