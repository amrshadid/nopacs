package tenant

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
)

// Restoring is what a restore asks of a tenant's database beyond loading
// it (package snapshot): taking the tenant's storages over, and bringing
// the corrections it holds up to what the storages' correction records say
// happened after the backup. A driver implements it beside Store; a restore
// runs it with no server running.
type Restoring interface {
	// StorageOwners returns the owner generation the database holds for
	// each storage, in order of storage name.
	StorageOwners(ctx context.Context) ([]StorageOwner, error)
	// SetStorageOwner records that the database wrote the owner record of
	// generation on the storage at now. A generation lower than the one
	// held is a precondition failure that changes nothing.
	SetStorageOwner(ctx context.Context, storage string, generation int64, now time.Time) error
	// AdoptCorrection brings the database's row of the correction a
	// storage's record names up to the record (AdoptCorrection's rules
	// below), and answers what it did.
	AdoptCorrection(ctx context.Context, rec record.Correction, storage string, now time.Time) (Adopted, error)
	// RelinkHidden points the rows a committed delete hid at the catalog
	// rows they hang from, by name, once the catalog is rebuilt under new
	// ids, so an undo puts them back under the right parent: a hidden
	// session's subject, a hidden scan's session, a hidden instance's scan,
	// when the delete did not hide that parent too. It answers how many rows
	// it changed.
	RelinkHidden(ctx context.Context) (int64, error)
	// MarkQuarantineLost marks every quarantined file held whose bytes were
	// kept as incoming lost (QuarantineIncomingLost, no details, no bytes
	// kept): those bytes lived in the server's incoming directory, which a
	// backup does not hold. It answers how many it marked.
	MarkQuarantineLost(ctx context.Context) (int64, error)
}

// StorageOwner is the owner generation a tenant's database holds for one
// storage.
type StorageOwner struct {
	// Storage is the storage's name.
	Storage string
	// Generation is the generation of the owner record the database wrote.
	Generation int64
	// Written is when it was written.
	Written time.Time
}

// Adopted is what AdoptCorrection did.
type Adopted string

// What AdoptCorrection does with a record.
//
// A record on a storage is not authenticated: anyone who can write to the
// storage can write one. So a record never moves a row the backup holds,
// and never makes a row in an end it cannot come back from.
//
// A record whose correction has no row gets one, its reason unknown (a
// record holds only the reason's digest; StateReasonUnresolved says so),
// with the record's scans of that storage. A delete, whatever its record
// says (prepared, committed, purging, purged, undone or abandoned), becomes
// committed and unresolved, naming its data so an arrival of it is held:
// an admin decides it by an undo or a purge, and a record alone can neither
// purge data nor end a delete. A prepared or abandoned relabel or replace,
// whose writes no row can finish or undo, becomes abandoned; one published
// becomes committed and unresolved, never purged. The same correction's
// record on another storage adds that storage's scans to the row it made.
// The row keeps the record's state as the state its records were written
// with, so a start writes them again from the row.
//
// A record ahead of its row (record.State.After), or naming another kind,
// disagrees with the backup: the row stays as it is (a delete in its grace
// period stays in it, with what it hid and what names its data), and the
// record is answered AdoptedDisagrees; a record ahead is marked as the
// state the records were written with, so a start writes them again from
// the row. A record behind or equal to its row changes nothing: a start
// rewrites it from the row.
const (
	// AdoptedAdded is a row made from the record.
	AdoptedAdded Adopted = "added"
	// AdoptedDisagrees is a record that disagrees with the backup's row,
	// which stays as it is.
	AdoptedDisagrees Adopted = "disagrees"
	// AdoptedScans is a row made from another storage's record, given this
	// storage's scans.
	AdoptedScans Adopted = "scans"
	// AdoptedNone is a row already as far as the record, or further.
	AdoptedNone Adopted = ""
)
