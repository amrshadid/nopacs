package supervise

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// gateState is why a tenant's receivers refuse new associations now, as the
// worker last saw it. It is replaced whole, so a listener reads it with one
// atomic load.
type gateState struct {
	// rebuilding: a rebuild that replaces the tenant's catalog is queued or
	// running, so no pipeline runs until it ends.
	rebuilding bool
	// notReady: the tenant's database does not answer, or no pipeline runs.
	notReady bool
	// incomingFull: the incoming area's disk is below the reserve.
	incomingFull bool
	// overLimit: the tenant holds as many incoming bytes as it may.
	overLimit bool
	// fullStorages are the tenant's storages below the reserve, by name.
	fullStorages map[string]bool
	// switching are the tenant's storages being switched to their
	// replicas, by name: every write to one waits until the switch ends.
	switching map[string]bool
}

// setGates replaces the gates with a copy changed by change.
func (t *tenantRun) setGates(change func(*gateState)) {
	old := t.gates.Load()
	g := *old
	change(&g)
	t.gates.Store(&g)
}

// refusal returns why run's receiver refuses new associations and stores
// now: the tenant's catalog being rebuilt or the tenant not ready, the
// receiver's storage being switched to its replica, the incoming disk or the
// receiver's storage below the reserve, the tenant over its incoming limit;
// "" when none. A rebuild is answered as a tenant not ready: a temporary
// failure the sender tries again (an association rejected as transient,
// PS3.8 9.3.4, or a store in an open one answered A700H, PS3.4 B.2.3;
// PS3.18 10.5.3, 503 with Retry-After), as for every gate here. A refusal
// for a switch is counted on the switch's gate, which records the count of
// its pause.
func (t *tenantRun) refusal(run *receiverRun) tenant.AssociationFailure {
	g := t.gates.Load()
	switch {
	case g.rebuilding, g.notReady:
		return tenant.FailureTenantNotReady
	case g.switching[*run.storage.Load()]:
		if gate := t.switchGate(); gate != nil {
			gate.CountRefused(*run.storage.Load())
		}
		return tenant.FailureStorageSwitching
	case g.incomingFull:
		return tenant.FailureNearlyFull
	case g.fullStorages[*run.storage.Load()]:
		return tenant.FailureNearlyFull
	case g.overLimit:
		return tenant.FailureIncomingLimit
	}
	return ""
}

// upHealth returns the health of a receiver whose listener runs: up, or
// down for the gate that holds it, in the order refusal reads them. A
// receiver whose storage is being switched refuses every association until
// the switch ends, so it is not answered up meanwhile; the contract names
// no reason of its own for it, so it is down for another reason, with the
// phrase that says which.
func (t *tenantRun) upHealth(r tenant.Receiver, now time.Time) Health {
	g := t.gates.Load()
	switch {
	case g.rebuilding:
		return down(tenant.DownTenantNotReady, tenant.PhraseTenantRebuilding, now)
	case g.notReady:
		return down(tenant.DownTenantNotReady, tenant.PhraseTenantNotReady, now)
	case g.switching[r.Storage]:
		return down(tenant.DownOther, tenant.PhraseStorageSwitching, now)
	case g.incomingFull:
		return down(tenant.DownNearlyFull, tenant.PhraseIncomingFull, now)
	case g.fullStorages[r.Storage]:
		return down(tenant.DownNearlyFull, tenant.PhraseStorageFull, now)
	case g.overLimit:
		return down(tenant.DownIncomingLimit, tenant.PhraseIncomingLimit, now)
	}
	return Health{State: tenant.ReceiverUp, Since: now}
}

// spaceState is what the worker last found of each storage, so a change is
// reported once.
type spaceState struct {
	storages map[string]bool
	// usage is each storage's space as last measured, for View.
	usage map[string]StorageSpace
}

// checkSpace reads the reserve, the incoming disk's space, the space of
// each storage an enabled receiver writes to, and the tenant's incoming
// bytes against its limit, and sets the gates. A storage that crossed its
// reserve, either way, makes one StorageSpaceChanged event. A reading that
// fails keeps the gate as it was: a disk that cannot be read now is not
// called full, nor a full one called empty.
func (t *tenantRun) checkSpace(st tenant.Store, p tenant.Pipeline, rows []tenant.Receiver) {
	ctx, cancel := context.WithTimeout(t.sup.ctx, opTimeout)
	defer cancel()
	g := *t.gates.Load()
	reserve, err := t.sup.values().Reserve(ctx)
	if err != nil {
		t.log.Warn("receivers: the reserve could not be read; space is not checked", "reason", fault.Message(err))
		return
	}
	if f := t.sup.cfg.IncomingUsage; f != nil {
		if u, err := f(ctx); err == nil {
			g.incomingFull = u.Known && u.Free < reserve.Of(u.Total)
		}
	}
	full := map[string]bool{}
	usage := map[string]StorageSpace{}
	var events []tenant.EventPayload
	if t.space.storages == nil {
		t.space.storages = map[string]bool{}
	}
	for _, name := range storagesOf(rows) {
		was := t.space.storages[name]
		is, used, free, known := t.storageFull(ctx, p, name, reserve)
		if !known {
			is = was
		} else {
			usage[name] = StorageSpace{FreeBytes: free, NearlyFull: is}
		}
		full[name] = is
		if is == was || !known {
			continue
		}
		t.space.storages[name] = is
		state := tenant.StorageRecovered
		if is {
			state = tenant.StorageNearlyFull
		}
		fb := free
		events = append(events, &tenant.StorageSpaceChanged{Storage: name, State: state, UsedBytes: used, FreeBytes: &fb})
	}
	g.fullStorages = full
	t.space.usage = usage
	g.overLimit = t.overLimit(ctx, st)
	g.switching = t.switchingOf(ctx, storagesOf(rows), g.switching)
	t.gates.Store(&g)
	t.appendEvents(events, t.sup.cfg.Now())
}

// switchGate returns the gate of writes to the tenant's storages, nil when
// the supervisor is given none.
func (t *tenantRun) switchGate() SwitchGate {
	if t.sup.cfg.Switches == nil {
		return nil
	}
	return t.sup.cfg.Switches(t.name)
}

// switchingOf returns which of the storages named are being switched to
// their replicas now. A storage whose state cannot be read keeps what was
// last seen of it (was): a gate that does not answer neither opens nor
// closes the receivers.
func (t *tenantRun) switchingOf(ctx context.Context, names []string, was map[string]bool) map[string]bool {
	gate := t.switchGate()
	if gate == nil {
		return nil
	}
	out := map[string]bool{}
	for _, name := range names {
		on, err := gate.Switching(ctx, name)
		if err != nil {
			on = was[name]
		}
		if on {
			out[name] = true
		}
	}
	return out
}

// storageFull reads the space of the storage named name: whether its free
// space is below the reserve of its size, and its used and free bytes, when
// the storage can say.
func (t *tenantRun) storageFull(ctx context.Context, p tenant.Pipeline, name string, reserve settings.Reserve) (full bool, used, free int64, known bool) {
	open := t.sup.cfg.OpenStorage
	if open == nil {
		return false, 0, 0, false
	}
	bs, err := open(ctx, t.name, p, name)
	if err != nil {
		return false, 0, 0, false
	}
	defer func() { _ = bs.Close() }()
	u, err := bs.Usage(ctx)
	if err != nil || !u.Known {
		return false, 0, 0, false
	}
	return u.Free < reserve.Of(u.Total), u.Used, u.Free, true
}

// overLimit reports whether the tenant holds as many bytes in the incoming
// area as its setting incoming.limit allows; never when it has no limit,
// or its database keeps no counts or does not answer.
func (t *tenantRun) overLimit(ctx context.Context, st tenant.Store) bool {
	limit, err := t.sup.values().OnTenant(st).IncomingLimit(ctx)
	if err != nil {
		return t.gates.Load().overLimit
	}
	if limit <= 0 {
		return false
	}
	feed, ok := st.(tenant.Events)
	if !ok {
		return false
	}
	snap, err := feed.StatusSnapshot(ctx)
	if err != nil {
		return t.gates.Load().overLimit
	}
	return snap.IncomingBytes >= limit
}

// storagesOf returns the storages the enabled receivers of rows write to,
// each once.
func storagesOf(rows []tenant.Receiver) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		if r.Enabled && !seen[r.Storage] {
			seen[r.Storage] = true
			out = append(out, r.Storage)
		}
	}
	return out
}
