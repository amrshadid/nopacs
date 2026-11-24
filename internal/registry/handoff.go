package registry

import (
	"context"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A tenant is held read only (master.TenantState.ReadOnly) in two ways. An
// export makes it moved, through SetHandoff, so nothing is written after
// its snapshot. A fence check makes it fenced: the owner record on one of
// its storages names another installation, another tenant, or a later
// generation than its database holds, so another installation has taken the
// storage over and two installations must never write it. Either is kept in
// the master's row, so a restart reads it back, and stands until a person
// takes the tenant back (SetHandoff with ready): the server's own checks
// never lift it (master.CheckStateChange).

// Fence checks the owner records of a tenant's storages: it returns the
// reason the tenant is fenced (which storage, and why), or "" when every
// record lets this installation write. An error is a check that could not
// be made (a storage that does not answer): it fences nothing, and the
// check is made again at the next round. It is called with the tenant's
// entry locked, so it must not call the registry.
type Fence func(ctx context.Context, id, name string, st tenant.Store) (string, error)

// fence runs Config.Fence for e, whose store is open, unless it is already
// fenced, and holds e fenced when it says so. The caller holds e.op and
// settles e after.
func (r *Registry) fence(e *entry) {
	if r.cfg.Fence == nil || e.store == nil || e.readOnly == master.StateFenced {
		return
	}
	ctx, cancel := context.WithTimeout(e.ctx, r.cfg.OpenTimeout)
	defer cancel()
	reason, err := r.cfg.Fence(ctx, e.id, e.name, e.store)
	if err != nil {
		r.log.Warn("a tenant's owner records could not be checked; it is checked again at the next round", "tenant", e.name,
			"kind", fault.KindOf(err).String(), "message", fault.Message(err))
		return
	}
	if reason == "" {
		return
	}
	r.log.Warn("tenant fenced: another installation owns one of its storages", "tenant", e.name, "reason", reason)
	e.readOnly, e.readOnlyReason = master.StateFenced, fitReason(reason)
}

// CheckFence checks the tenant named name's owner records now, as the
// timer's checks do, and returns what Store returns after it: nil for a
// ready tenant, the master's refusal for a moved or fenced one. A step that
// writes to a storage asks it first (a purge, a relabel, a replace), so a
// tenant another installation took over since the last round writes
// nothing more.
func (r *Registry) CheckFence(name string) error {
	e := (*r.entries.Load())[name]
	if e == nil {
		_, err := r.Store(name)
		return err
	}
	e.op.Lock()
	if s := e.view.Load().state; !e.removed && e.store != nil && (s == master.StateReady || s == master.StateMoved) {
		r.fence(e)
		if e.readOnly == master.StateFenced {
			r.settle(e, master.StateReady, "")
		}
	}
	e.op.Unlock()
	_, err := r.Store(name)
	return err
}

// SetHandoff sets the state of the tenant named name, for a person's act:
// moved (an export hands it off), or ready (a reclaim takes a moved or
// fenced tenant back, or an export that failed before its summary gives it
// back). The master records it with audit, by by (master.Store.
// SetTenantState), and the registry's view follows at once. A tenant whose
// database does not answer is Unavailable, and is not changed.
func (r *Registry) SetHandoff(ctx context.Context, name string, state master.TenantState, reason string, audit master.AuditEntry, by master.Actor) error {
	return r.setHandoff(ctx, name, "", state, reason, audit, by)
}

// GiveBack makes the tenant named name ready again after an export of it
// that failed before its summary, only while it is still moved: a tenant
// fenced meanwhile (another installation's owner record found) stays
// fenced, and one already taken back is left as it is; either is a
// Precondition error naming its state. The server makes the change
// (master.ExportGiveBack), not the key that asked for the export: that key
// may have been revoked or have expired meanwhile, and a store refuses a
// change by such a key, which would leave the tenant moved with no
// transfer key anywhere. audit still names the key that asked.
func (r *Registry) GiveBack(ctx context.Context, name string, audit master.AuditEntry) error {
	return r.setHandoff(ctx, name, master.StateMoved, master.StateReady, "", audit, master.ExportGiveBack())
}

// CheckMoved returns nil when the tenant named name is held moved, read
// under its entry's lock, so a hand-off or a fence check that changes it
// has either ended before or waits until after; a Precondition error
// naming its state otherwise. An export asks it before it hands out its
// transfer key, so no key reaches anyone for a tenant writable here.
func (r *Registry) CheckMoved(name string) error {
	e := (*r.entries.Load())[name]
	if e == nil {
		return fault.New(fault.NotFound, "no such tenant")
	}
	e.op.Lock()
	defer e.op.Unlock()
	if e.removed {
		return fault.New(fault.NotFound, "no such tenant")
	}
	return handoffFrom(name, e, master.StateMoved)
}

// handoffFrom returns nil when e is held in the state from, a Precondition
// error naming the state it is in otherwise. The caller holds e.op.
func handoffFrom(name string, e *entry, from master.TenantState) error {
	if e.readOnly == from {
		return nil
	}
	now := e.readOnly
	if now == "" {
		now = e.view.Load().state
	}
	return fault.New(fault.Precondition, "the tenant "+name+" is "+string(now)+", no longer "+string(from))
}

// setHandoff is SetHandoff, refused unless the tenant is held in the state
// from when from is set.
func (r *Registry) setHandoff(ctx context.Context, name string, from, state master.TenantState, reason string, audit master.AuditEntry, by master.Actor) error {
	if state != master.StateMoved && state != master.StateReady {
		return fault.New(fault.Internal, "registry: a hand-off sets a tenant moved or ready")
	}
	e := (*r.entries.Load())[name]
	if e == nil {
		return fault.New(fault.NotFound, "no such tenant")
	}
	e.op.Lock()
	defer e.op.Unlock()
	if e.removed {
		return fault.New(fault.NotFound, "no such tenant")
	}
	if e.store == nil {
		return notReady(name, e.view.Load())
	}
	if from != "" {
		if err := handoffFrom(name, e, from); err != nil {
			return err
		}
	}
	if err := r.cfg.Master.SetTenantState(ctx, e.id, state, reason, audit, by); err != nil {
		return err
	}
	e.written, e.writtenReason, e.lastWrite = state, reason, r.cfg.Now()
	e.readOnly, e.readOnlyReason = "", ""
	if state.ReadOnly() {
		e.readOnly, e.readOnlyReason = state, reason
	}
	r.settle(e, master.StateReady, "")
	return nil
}
