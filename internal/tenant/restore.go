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
	// UnhiddenDeletes returns the ids of the deletes committed that hide no
	// row: those AdoptCorrection made committed from their records (the
	// backup holds no catalog for them to hide anything in). A restore's
	// rebuild catalogs their data and HideDeletes hides it, so an undo
	// shows it again.
	UnhiddenDeletes(ctx context.Context) ([]string, error)
	// HideDeletes makes each delete given that is committed hide what the
	// catalog holds of its target, as its publish does, owning the
	// manifest and metadata its snapshots name, and answers what was
	// hidden. A target the catalog does not hold hides nothing; one it
	// holds under other labels is a precondition failure.
	HideDeletes(ctx context.Context, deletes []DeleteToHide) (Hidden, error)
	// OwnAdoptedReplace gives a replace AdoptCorrection made committed from
	// its record alone what its publish writes outside the catalog: the
	// superseded revision it owns (its size from sizeOf, the storage's),
	// which its purge removes; the revision in the instance's history; the
	// quarantined file still held of that instance and checksum, released.
	// Anything else changes nothing.
	OwnAdoptedReplace(ctx context.Context, rec record.Correction, storage string,
		sizeOf func(ctx context.Context, key string) (int64, error), now time.Time) error
	// OwnAdoptedEdit gives an edit AdoptCorrection made committed from its
	// records what its publish writes outside the catalog, for each of its
	// instances whose scan is on storage (one record of an edit on each
	// storage it touches): the superseded revision it owns (its size from
	// sizeOf, the storage's; one gone from the storage owns nothing), which
	// its purge removes, and the revision in the instance's history. An
	// instance owned already, or anything else, changes nothing. Such an
	// edit keeps no line of its old revisions, so it is not undone.
	OwnAdoptedEdit(ctx context.Context, rec record.Correction, storage string,
		sizeOf func(ctx context.Context, key string) (int64, error), now time.Time) error
	// HoldCommittedDeletes marks unresolved every delete committed that is
	// not (StateReasonRestoreHeld), so none is purged until an admin
	// confirms it, and answers how many it marked.
	HoldCommittedDeletes(ctx context.Context) (int64, error)
	// HoldEarlierSubjectLabels makes every delete of a subject that is
	// committed name, beside each subject label it names, every label that
	// subject had before a relabel published before the delete began, as a
	// delete's publish names them, so data arriving under a label a scanner
	// still knows the subject by is held; a relabel that began after the
	// delete (one that gave its label to another subject once the delete
	// was purged) is not followed. A delete AdoptCorrection made from its record
	// names only the label its record holds, and the relabel that gave up
	// an earlier label may be adopted after it (the records are read in
	// their storages' listing order), so a restore runs this once every
	// record is adopted. A delete purged is left as it is: an admin may
	// have allowed arrivals under one of its labels since
	// (Corrections.AllowArrivals). It answers how many labels it added.
	HoldEarlierSubjectLabels(ctx context.Context) (int64, error)
	// EndRebuilds ends every rebuild job not ended as canceled, with the
	// reason JobReasonEndedByRestore and its event, and answers their ids.
	// A database brought back may hold a rebuild a server was working when
	// it was copied, whose worker is gone; while it is not ended, the
	// rebuild a restore makes is refused (Jobs.CreateJob), so the restore
	// could never end. Each one's unit is left as a cancel leaves it, for
	// a worker that meets the job ended; its shadow is left for
	// Rebuilding.DropShadow.
	EndRebuilds(ctx context.Context, now time.Time) ([]string, error)
	// MarkQuarantineLost marks every quarantined file held whose bytes were
	// kept as incoming lost (QuarantineIncomingLost, no details, no bytes
	// kept), but those whose ids keep names: those bytes lived in the
	// server's incoming directory, which a backup does not hold; a
	// reconcile of databases restored in place keeps the files whose bytes
	// it found there, with their recorded checksum. It answers how many it
	// marked.
	MarkQuarantineLost(ctx context.Context, keep []string) (int64, error)
}

// JobReasonEndedByRestore is the reason of a rebuild job EndRebuilds ended.
const JobReasonEndedByRestore = "canceled: ended by a restore or a reconcile"

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

// Hidden is what HideDeletes hid: how many of the deletes that hid
// something are unresolved, and the scans they hid whole or some
// instances of, as a rebuild tallies what it leaves out
// (TallyUnresolvedCorrection, TallyLeftOutDeleted).
type Hidden struct {
	Unresolved, Scans int64
}

// DeleteToHide is a delete HideDeletes hides, with what was read of its
// scans' manifest and metadata on the storages, by Series Instance UID,
// which its purge may remove while their bytes are these.
type DeleteToHide struct {
	ID        string
	Snapshots map[string]ScanSnapshot
}

// Adopted is what AdoptCorrection did.
type Adopted string

// What AdoptCorrection does with a record.
//
// The restore hands it only records whose MAC verified under the key file
// for the tenant (record.DecodeCorrection): what one says is the tenant's
// own word. It is read by one rule, the one a rebuild reads a record with
// no row by (record.Correction.Alone), and a record never makes a row in an
// end it cannot come back from: no row is made purged.
//
// A record whose correction has no row gets one, its reason unknown (a
// record holds only the reason's digest; StateReasonUnresolved says so),
// with the record's scans of that storage. A delete committed, purging,
// purged or still prepared becomes committed and unresolved, naming its
// data so an arrival of it is held: an admin decides it by an undo or a
// purge, and a record alone never purges data. A delete abandoned or undone
// becomes so, naming nothing, its data shown. A prepared or abandoned
// relabel or replace, whose writes no row can finish or undo, becomes
// abandoned; one published becomes committed and unresolved, never purged.
// The same correction's record on another storage adds that storage's
// scans to the row it made. The row keeps the record's state as the state
// its records were written with, so a start writes them again from the row.
//
// A row the backup holds prepared, whose record is ahead of it
// (record.State.After), was backed up before its request was answered: it
// is moved to the state the same rule reads its record in, its reason
// StateReasonFromRecord, and answered AdoptedMoved; were it left prepared,
// a start would roll it back against what the storages hold.
//
// A delete the backup holds committed whose record says undone was undone
// after the backup: the engine writes that record only once the database's
// undo has committed. It is undone as an undo does it (what it hid shown,
// what names its data dropped), its reason StateReasonFromRecord, and
// answered AdoptedMoved; kept committed, its purge would remove data the
// tenant took the delete back from.
//
// A delete row the backup holds prepared whose record still says prepared
// (published, its record's rewrite failed) is taken as committed and
// unresolved, as a record with no row is, and answered AdoptedMoved.
//
// A record of a purge ahead of a row past prepared, or a record naming
// another kind, disagrees with the backup: the row stays as it is (a
// delete in its grace period stays in it, with what it hid and what names
// its data; the purge began past its point of no return, and the row's own
// purge ends it), and the record is answered AdoptedDisagrees; a record
// ahead is marked as the state the records were written with, so a start
// writes them again from the row. A record behind or equal to its row
// changes nothing: a start rewrites it from the row.
const (
	// AdoptedAdded is a row made from the record.
	AdoptedAdded Adopted = "added"
	// AdoptedMoved is a row the backup holds behind its record (prepared,
	// or a delete committed and undone since), moved to the state its
	// record says.
	AdoptedMoved Adopted = "moved"
	// AdoptedDisagrees is a record that disagrees with the backup's row,
	// which stays as it is.
	AdoptedDisagrees Adopted = "disagrees"
	// AdoptedScans is a row made from another storage's record, given this
	// storage's scans.
	AdoptedScans Adopted = "scans"
	// AdoptedNone is a row already as far as the record, or further.
	AdoptedNone Adopted = ""
)
