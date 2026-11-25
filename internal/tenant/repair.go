package tenant

import (
	"context"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// Repairs is what the tenant's database keeps of the repairs of a primary's
// objects from a replica: what each damaged object was before the good
// bytes were put over it, and the finding the repair cleared.
//
// A repair is written in two steps, each one transaction. First the record
// (RecordRepair): the object, the finding it mends, the replica the good
// bytes come from, the job, and the damaged object's size and checksum as
// found, before anything is written to the storage, so what was there is
// known whatever happens next. Then, once exactly the bytes the catalog
// names are put at the same key and read back, the end (CompleteRepair): the
// record's time of repair, the finding cleared by the repair's job, the
// instance's mark lifted (unless another finding still marks it), the
// finding's event, and an audit entry made by the server. A repair never
// writes the catalog: the bytes put are the ones it names.
//
// A replica's own damage is not a repair: it is copied again from the
// primary by the replicator (Replication.RecordReplicaDamage).
//
// A repair recorded under a fence holds its scan until it ends, is let go,
// or runs out, as repair_hold.go says: every change of the scan's objects
// is refused with ScanRepairingError meanwhile.
//
// A repair job's progress is kept with its repairs: CompleteRepair counts
// the finding repaired or already good on the job its record names, and
// RecordRepairOutcome counts one no replica holds good, each finding once a
// job; EndRepairJob ends the job and its unit together.
type Repairs interface {
	// RecordRepair records r, under r.Fence when given (the repair job's
	// unit), and answers it as stored and whether this call made it. The
	// finding must hold, be on r.Storage, and be of a kind a repair mends
	// (ScrubFindingKind's Mendable); r.Storage must not be a replica, and
	// r.From must be a replica of it; the catalog must still name at the
	// finding's key the bytes r expects (CheckRepairCatalog); no correction
	// touching the scan may be prepared, committed or purging, and no
	// switch of r.Storage or r.From accepted or swapped: each a
	// precondition failure otherwise, writing nothing. A finding the tenant
	// does not have is NotFound. Under the fence of a repair job's unit
	// whose job has ended or was canceled, it is refused with
	// RepairJobEndedError, writing nothing. Under a fence, the record holds
	// the scan until its HeldUntil, the due the fence's unit has now plus
	// RepairHoldGrace, unless it ends or is let go before. A record of the
	// same id ended already is answered as it stands; one not ended whose
	// hold under another fence has not run out is refused with
	// ScanRepairingError (that fence's worker may still be putting the
	// object); one not ended otherwise is checked again and holds the scan
	// under the new fence, never to an earlier end than it held to; and one
	// whose expected bytes are not r's is a precondition failure.
	RecordRepair(ctx context.Context, r RepairRecord) (Repair, bool, error)
	// ReleaseRepair ends the hold of the scan the repair of id holds under
	// fence, writing nothing else: the way out of an object whose put
	// failed or was not made, so a change of the scan waits no longer. A
	// repair ended, held under another fence, or holding nothing is left as
	// it is; one the tenant does not have is NotFound; a fence lost is
	// ErrLeaseLost, and the hold then runs to its HeldUntil.
	ReleaseRepair(ctx context.Context, fence WorkFence, id string) error
	// ScanRepairHeld reports whether a repair holds, at now, the scan of
	// the Series Instance UID: a reception reads it before it puts the
	// scan's sidecar and manifest, so a hold does not let it write them
	// and then refuse its commit.
	ScanRepairHeld(ctx context.Context, seriesUID string, now time.Time) (bool, error)
	// CompleteRepair ends the repair of id, under fence when given, as
	// Repairs says, in one transaction with audit, the server's (no Guard),
	// and answers it as stored; its hold of the scan ends with it. When the
	// catalog no longer names the bytes the repair expected
	// (CheckRepairCatalog: its hold ended with its lease and the scan
	// changed meanwhile), the repair ends Changed, its finding left
	// holding, and the audit entry's detail "outcome" says "changed"; unless
	// the bytes it found were the bytes it expected (nothing was put), the
	// key it may have put is recorded, in the same transaction, as a
	// finding of kind FindingOrphan on its storage, of the repair's job, so
	// bytes the catalog may no longer name are never left unrecorded. A
	// repair ended already is answered as it stands, writing nothing; one
	// the tenant does not have is NotFound; one whose finding a switch moved
	// to another storage since it was recorded is a precondition failure,
	// writing nothing. A repair of a repair job counts its finding on the
	// job (repaired, or already good when the bytes found were the bytes
	// expected) unless the job counted it before.
	CompleteRepair(ctx context.Context, fence *WorkFence, id string, now time.Time, audit master.AuditEntry) (Repair, error)
	// Repair answers the repair of id, or NotFound.
	Repair(ctx context.Context, id string) (Repair, error)
	// FindingRepairs answers the repairs of a finding, oldest first, at most
	// MaxFindingRepairs.
	FindingRepairs(ctx context.Context, findingID string) ([]Repair, error)
	// RepairFindings answers the findings of these ids, cleared or not, in
	// the order given, skipping an id the tenant does not have; at most
	// MaxRepairFindings ids.
	RepairFindings(ctx context.Context, ids []string) ([]ScrubFinding, error)
	// RecordRepairOutcome counts o on the repair job whose unit holds its
	// fence, with its tally, and answers the job: a finding the job counted
	// before is not counted again, but for one counted no good copy and now
	// repaired or good, which moves from refused to sent. A job ended is
	// answered as it stands. A unit not of a repair job is a precondition
	// failure.
	RecordRepairOutcome(ctx context.Context, o RepairOutcome) (Job, error)
	// EndRepairJob ends the repair job whose unit holds fence, done or
	// partial by its counts (EndState), completes the unit and ends every
	// hold of the job's repairs, in one transaction, and answers the job. A
	// job ended already (canceled) is answered as it stands, its unit
	// completed. A unit not of a repair job is a precondition failure.
	EndRepairJob(ctx context.Context, fence WorkFence, now time.Time) (Job, error)
}

// MaxFindingRepairs is the most repairs FindingRepairs answers.
const MaxFindingRepairs = 100

// Repair is one object of a primary put back from a replica.
type Repair struct {
	// ID names it: RepairID of its job and finding.
	ID string
	// Finding is the scrub finding it mends; Storage, Key, SOPInstanceUID
	// and Revision the object, as the finding names it.
	Finding, Storage, Key, SOPInstanceUID string
	Revision                              int
	// From names the replica the good bytes come from; Job the repair job.
	From, Job string
	// ExpectedSize and ExpectedSHA256 are the catalog's, the bytes put.
	ExpectedSize   int64
	ExpectedSHA256 SHA256
	// FoundSize and FoundSHA256 are what the damaged object was: nil and
	// zero for an object missing.
	FoundSize   *int64
	FoundSHA256 SHA256
	// SeriesUID is the Series Instance UID of the object's scan, the scan
	// the repair holds while it runs; the store's.
	SeriesUID string
	// Recorded is when it was recorded; Repaired when it ended, zero
	// until then.
	Recorded, Repaired time.Time
	// HeldUntil is when its hold of the scan runs out by itself: the due
	// its unit had at its record plus RepairHoldGrace. Zero when it holds
	// nothing (ended, let go, or recorded under no fence).
	HeldUntil time.Time
	// Changed says it ended without clearing its finding: the catalog no
	// longer named the bytes it expected.
	Changed bool
}

// Done reports whether the repair ended.
func (r Repair) Done() bool { return !r.Repaired.IsZero() }

// FoundExpected reports whether the object the repair found was already the
// bytes it expected, so nothing was put over it.
func (r Repair) FoundExpected() bool {
	return r.FoundSize != nil && *r.FoundSize == r.ExpectedSize && r.FoundSHA256 == r.ExpectedSHA256
}

// RepairRecord is what RecordRepair records.
type RepairRecord struct {
	// Fence is the repair unit's lease; nil for none.
	Fence *WorkFence
	// Repair is what it records; its Storage, Key, SOPInstanceUID and
	// Revision are read from the finding, and SeriesUID, Recorded, Repaired,
	// HeldUntil and Changed are the store's: each ignored.
	Repair Repair
	// Now is when it is recorded.
	Now time.Time
}

// RepairID returns the id of the repair of a finding by a job: made from
// both, so a write sent again names the same record.
func RepairID(job, finding string) string { return "rp-" + digest(job+"\x00"+finding) }

// Validate returns an Invalid error unless r can be recorded.
func (r RepairRecord) Validate() error {
	if r.Fence != nil {
		if err := r.Fence.Validate(); err != nil {
			return err
		}
	}
	p := r.Repair
	if err := master.ValidateID("repair id", p.ID); err != nil {
		return err
	}
	if err := master.ValidateID("finding id", p.Finding); err != nil {
		return err
	}
	if err := ValidateName("storage", p.Storage); err != nil {
		return err
	}
	if err := ValidateName("replica", p.From); err != nil {
		return err
	}
	if err := master.ValidateID("job id", p.Job); err != nil {
		return err
	}
	if p.ExpectedSize < 0 {
		return invalid("a repaired object's size is not negative")
	}
	if p.ExpectedSHA256.IsZero() {
		return invalid("a repair names the checksum the catalog holds")
	}
	if p.FoundSize != nil && *p.FoundSize < 0 {
		return invalid("a damaged object's size is not negative")
	}
	if p.FoundSize == nil && !p.FoundSHA256.IsZero() {
		return invalid("a missing object has no checksum found")
	}
	if p.Revision < 0 || p.Revision > maxFindingRevision {
		return invalid("a repaired object's revision is 0 to " + strconv.Itoa(maxFindingRevision))
	}
	return master.ValidateTime("repair time", r.Now)
}

// Mendable reports whether a repair mends a finding of kind k: an object
// missing or damaged, or a scan's manifest or metadata disagreeing or
// damaged, each put back whole from a replica. An orphan is not: no copy
// should hold it.
func (k ScrubFindingKind) Mendable() bool { return k.Valid() && k != FindingOrphan }

// CheckRepair returns a precondition failure unless finding f may be mended
// on storage from replica from: f holds, is on storage and is of a mendable
// kind; storage is not a replica; from is a replica of storage.
func CheckRepair(f ScrubFinding, storage, from Storage) error {
	switch {
	case storage.ReplicaOf != "":
		return Precondition("the storage " + storage.Name + " is a replica; the server copies its objects again by itself")
	case from.ReplicaOf != storage.Name:
		return Precondition("the storage " + from.Name + " is not a replica of " + storage.Name)
	case f.Storage != storage.Name:
		return Precondition("the finding is on another storage")
	case !f.Holds():
		return Precondition("the finding was cleared")
	case !f.Kind.Mendable():
		return Precondition("a repair does not mend a finding of kind " + string(f.Kind))
	}
	return nil
}
