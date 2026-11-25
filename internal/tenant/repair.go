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
type Repairs interface {
	// RecordRepair records r, under r.Fence when given (the repair job's
	// unit), and answers it as stored and whether this call made it: a
	// record of the same id is answered as it stands. The finding must hold,
	// be on r.Storage, and be of a kind a repair mends (ScrubFindingKind's
	// Mendable); r.Storage must not be a replica, and r.From must be a
	// replica of it: each a precondition failure otherwise. A finding the
	// tenant does not have is NotFound.
	RecordRepair(ctx context.Context, r RepairRecord) (Repair, bool, error)
	// CompleteRepair ends the repair of id, under fence when given, as
	// Repairs says, in one transaction with audit, the server's (no Guard),
	// and answers it as stored. A repair ended already is answered as it
	// stands, writing nothing; one the tenant does not have is NotFound;
	// one whose finding a switch moved to another storage since it was
	// recorded is a precondition failure, writing nothing.
	CompleteRepair(ctx context.Context, fence *WorkFence, id string, now time.Time, audit master.AuditEntry) (Repair, error)
	// Repair answers the repair of id, or NotFound.
	Repair(ctx context.Context, id string) (Repair, error)
	// FindingRepairs answers the repairs of a finding, oldest first, at most
	// MaxFindingRepairs.
	FindingRepairs(ctx context.Context, findingID string) ([]Repair, error)
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
	// Recorded is when it was recorded; Repaired when it ended, zero
	// until then.
	Recorded, Repaired time.Time
}

// Done reports whether the repair ended.
func (r Repair) Done() bool { return !r.Repaired.IsZero() }

// RepairRecord is what RecordRepair records.
type RepairRecord struct {
	// Fence is the repair unit's lease; nil for none.
	Fence *WorkFence
	// Repair is what it records; its Storage, Key, SOPInstanceUID and
	// Revision are read from the finding, and Recorded and Repaired are the
	// store's: each ignored.
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
