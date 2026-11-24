package backup

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// One writer per storage. Two installations writing one storage would each
// hold only their own in-process locks, and each would hide the other's
// writes. The owner record on each storage (record.Owner) names the
// installation, the tenant and a generation; an import and a restore write
// it one generation further before the tenant opens, an export before its
// snapshot and a reclaim before the tenant is writable again (Advance). A running server reads
// it when a tenant starts, on each check of its tenants, and before each
// purge, relabel and replace step; a record that names another
// installation or tenant, a later generation than the tenant's database
// holds, or that names this installation without a MAC its key file makes,
// fences the tenant (read only). The check is between writes, not a lock:
// two servers started within one check of each other could both write
// until the next.

// OwnerCheck reads and judges the owner records of a tenant's storages for
// a running server.
type OwnerCheck struct {
	// Installation is this installation's id.
	Installation string
	// Keys is the installation key file, which checks a record's MAC.
	Keys *secret.Keys
	// Opener opens a tenant's storages: for reading, and to write an owner
	// record taken one generation further. A server gives it its record of
	// cleaned storages (place.Opener.Cleanups).
	Opener place.Opener
}

// ownerFinding is what one storage's owner record says.
type ownerFinding struct {
	storage string
	// row is the storage's row, to open it again for writing.
	row tenant.Storage
	// found is whether the storage holds a record.
	found bool
	// foreign is why the record does not let this installation write at
	// the generation held: "" when it does.
	foreign string
	// record is the record read, when it could be.
	record record.Owner
}

// inspect reads every storage's owner record of the tenant whose id is id
// and judges each against the generations its database holds. An error is
// a storage that could not be opened or read.
//
// The records are read first and the database's generations after. An
// advance (Advance) writes the database's generation before the record, so
// a record read here is at or below the generation read after it, even
// when an export's or a reclaim's advance runs beside this check (the
// registry's fence check runs every 30 s, under its own lock, not the
// export's). Read the other way round, an advance landing between the two
// reads would show a record one generation past the database, and fence a
// tenant this installation is exporting, as if another had taken it over.
func (c OwnerCheck) inspect(ctx context.Context, id, name string, st tenant.Store) ([]ownerFinding, map[string]int64, error) {
	p, ok := st.(tenant.Pipeline)
	rs, ok2 := st.(tenant.Restoring)
	if !ok || !ok2 {
		return nil, nil, fault.New(fault.Internal, "backup: this database type keeps no owner generations")
	}
	storages, err := listStorages(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	pt := place.TenantOf(id, name, st)
	read := make([]ownerRead, 0, len(storages))
	for _, s := range storages {
		bs, err := c.Opener.OpenForReading(ctx, pt, s)
		if err != nil {
			return nil, nil, fault.Wrap(fault.Unavailable, err, "the storage "+s.Name+" cannot be opened: "+fault.Message(err))
		}
		r, err := readOwner(ctx, bs, s.Name)
		_ = bs.Close()
		if err != nil {
			return nil, nil, err
		}
		read = append(read, r)
	}
	held, err := rs.StorageOwners(ctx)
	if err != nil {
		return nil, nil, err
	}
	generations := map[string]int64{}
	for _, o := range held {
		generations[o.Storage] = o.Generation
	}
	out := make([]ownerFinding, 0, len(storages))
	for i, s := range storages {
		f, err := c.judge(read[i], id, s.Name, generations[s.Name])
		if err != nil {
			return nil, nil, err
		}
		f.row = s
		out = append(out, f)
	}
	return out, generations, nil
}

// ownerRead is one storage's owner record as read, before it is judged.
type ownerRead struct {
	// found is whether the storage holds a record.
	found bool
	// record is the record, when it decoded; decodeErr why it did not.
	record    record.Owner
	decodeErr error
}

// readOwner reads one storage's owner record.
func readOwner(ctx context.Context, bs blob.Store, storage string) (ownerRead, error) {
	rc, err := bs.Read(ctx, layout.OwnerKey, 0, -1)
	if fault.KindOf(err) == fault.NotFound {
		return ownerRead{}, nil
	}
	if err != nil {
		return ownerRead{}, fault.Wrap(fault.Unavailable, err, "the owner record of the storage "+storage+" cannot be read: "+fault.Message(err))
	}
	o, derr := record.DecodeOwner(rc)
	_ = rc.Close()
	return ownerRead{found: true, record: o, decodeErr: derr}, nil
}

// judge says whether one storage's owner record, as read, lets this
// installation write the tenant at the generation held.
func (c OwnerCheck) judge(r ownerRead, tenantID, storage string, held int64) (ownerFinding, error) {
	f := ownerFinding{storage: storage}
	if !r.found {
		return f, nil
	}
	o := r.record
	f.found = true
	prefix := "the storage " + storage + "'s owner record "
	switch {
	case r.decodeErr != nil:
		f.foreign = prefix + "is not one this installation writes"
		return f, nil
	case o.Installation != c.Installation:
		f.foreign = prefix + "names installation " + output.Clean(o.Installation) + " at generation " + strconv.FormatInt(o.Generation, 10) +
			": that installation took the storage over"
	case o.Tenant != tenantID:
		f.foreign = prefix + "names another tenant, " + output.Clean(o.Tenant)
	case o.Generation > held:
		f.foreign = prefix + "is at generation " + strconv.FormatInt(o.Generation, 10) + ", past the " + strconv.FormatInt(held, 10) +
			" this tenant's database holds: a restore or an import since took the storage over"
	default:
		if err := record.VerifyOwner(o, c.Keys); err != nil {
			if !errors.Is(err, record.ErrUnauthenticated) {
				return f, fault.Wrap(fault.Internal, err, "the owner record cannot be checked")
			}
			f.foreign = prefix + "names this installation but its MAC does not verify under this key file: this installation did not write it"
		}
	}
	f.record = o
	return f, nil
}

// Fence implements the registry's fence check: the reason the tenant is
// fenced, naming the first storage whose owner record does not let this
// installation write it, or "" when none.
func (c OwnerCheck) Fence(ctx context.Context, id, name string, st tenant.Store) (string, error) {
	found, _, err := c.inspect(ctx, id, name, st)
	if err != nil {
		return "", err
	}
	for _, f := range found {
		if f.foreign != "" {
			return f.foreign, nil
		}
	}
	return "", nil
}

// Reclaimable returns nil when a hand-off of the tenant may be undone: every
// storage's owner record still names this installation, this tenant and
// the generation its database holds, and verifies (or, for a storage this
// tenant never wrote a record on, there is still none). A record one
// generation behind the database is accepted too: it is what an Advance
// that recorded the next generation in the database and then failed to
// write the record leaves, and nobody else writes a record naming this
// installation that verifies. Any other is a Precondition error naming the
// storage: an import, or a restore, has taken it over. A storage that
// cannot be read is Unavailable.
func (c OwnerCheck) Reclaimable(ctx context.Context, id, name string, st tenant.Store) error {
	found, held, err := c.inspect(ctx, id, name, st)
	if err != nil {
		return err
	}
	return reclaimable(found, held)
}

// reclaimable is Reclaimable's judgment of what inspect found.
func reclaimable(found []ownerFinding, held map[string]int64) error {
	for _, f := range found {
		gen := held[f.storage]
		switch {
		case f.foreign != "":
			return fault.New(fault.Precondition, f.foreign+"; the tenant is not taken back")
		case !f.found && gen > 0:
			return fault.New(fault.Precondition, "the storage "+f.storage+" holds no owner record, and this tenant wrote one at generation "+
				strconv.FormatInt(gen, 10)+": someone removed it; the tenant is not taken back")
		case f.found && f.record.Generation != gen && f.record.Generation != gen-1:
			return fault.New(fault.Precondition, "the storage "+f.storage+"'s owner record is at generation "+strconv.FormatInt(f.record.Generation, 10)+
				", not the "+strconv.FormatInt(gen, 10)+" this tenant holds; the tenant is not taken back")
		}
	}
	return nil
}

// Advance takes every storage's owner record of the tenant one generation
// past both the record found and the generation its database holds,
// naming this installation and signed with its key file, and returns the
// generations written. An export calls it before its snapshot, so the
// export holds the newest generation and no storage is left without a
// record; a reclaim calls it before the tenant is writable again, so any
// export made before meets a later generation than it holds and its import
// is refused (takeOwner) unless the operator takes the storage over. A
// record that does not let this installation write (OwnerCheck.Fence) is a
// Precondition error, and nothing is written.
//
// The database is written before the record: a failure between the two
// leaves the record one generation behind, at the generation the last
// whole advance wrote, still past every export made before that advance,
// and Fence and Reclaimable accept it; the other order would leave a record
// past the database, which fences the tenant.
func (c OwnerCheck) Advance(ctx context.Context, id, name string, st tenant.Store) ([]Owner, error) {
	found, held, err := c.inspect(ctx, id, name, st)
	if err != nil {
		return nil, err
	}
	return c.advance(ctx, id, name, st, found, held)
}

// Reclaim is Reclaimable, then Advance on what it read: the owner records
// a hand-off no import followed left, taken one generation further before
// the tenant is writable again.
func (c OwnerCheck) Reclaim(ctx context.Context, id, name string, st tenant.Store) ([]Owner, error) {
	found, held, err := c.inspect(ctx, id, name, st)
	if err != nil {
		return nil, err
	}
	if err := reclaimable(found, held); err != nil {
		return nil, err
	}
	return c.advance(ctx, id, name, st, found, held)
}

// advance is Advance on what inspect found.
func (c OwnerCheck) advance(ctx context.Context, id, name string, st tenant.Store, found []ownerFinding, held map[string]int64) ([]Owner, error) {
	for _, f := range found {
		if f.foreign != "" {
			return nil, fault.New(fault.Precondition, f.foreign+"; its owner record is not taken further")
		}
	}
	rs, ok := st.(tenant.Restoring)
	if !ok {
		return nil, fault.New(fault.Internal, "backup: this database type keeps no owner generations")
	}
	pt := place.TenantOf(id, name, st)
	out := make([]Owner, 0, len(found))
	for _, f := range found {
		gen := held[f.storage]
		if f.found {
			gen = max(gen, f.record.Generation)
		}
		gen, err := nextGeneration(f.storage, gen)
		if err != nil {
			return out, err
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		if err := rs.SetStorageOwner(ctx, f.storage, gen, now); err != nil {
			return out, err
		}
		// A store the server opened earlier may still be open on the
		// storage with a Put in flight, which a cleanup of the temporary
		// directory would remove. With the server's record of cleaned
		// storages, Open removes what a crash left only at the first open
		// of the process, before any Put there, and opens beside
		// otherwise; without one, the open is beside.
		open := c.Opener.OpenBeside
		if c.Opener.Cleanups != nil {
			open = c.Opener.Open
		}
		bs, err := open(ctx, pt, f.row)
		if err != nil {
			return out, fault.Wrap(fault.Unavailable, err, "the storage "+f.storage+" cannot be opened: "+fault.Message(err))
		}
		err = writeOwner(ctx, bs, record.Owner{Installation: c.Installation, Tenant: id, Generation: gen, Written: now}, c.Keys)
		_ = bs.Close()
		if err != nil {
			return out, fault.Wrap(fault.KindOf(err), err, "the storage "+f.storage+"'s owner record cannot be written: "+fault.Message(err))
		}
		out = append(out, Owner{Storage: f.storage, Generation: gen})
	}
	return out, nil
}
