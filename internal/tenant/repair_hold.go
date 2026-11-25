package tenant

import (
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

// A repair holds its scan still around its put. The put of a replica's
// bytes to the primary cannot share a transaction with the catalog, so
// without a hold a delete published and purged while the put streams has
// its object put back after the purge removed it, and a purge that rewrites
// the scan's manifest has the old manifest put over the new one. So
// RecordRepair checks, in its transaction, that the catalog still names the
// bytes the repair expects and that nothing changing the scan is under way,
// and records the repair as holding the scan; while it holds, every change
// of that scan's objects is refused with ScanRepairing, which is retried:
// a repair is one object, short, and a reception's step or a person's call
// that meets it succeeds a moment later.
//
// The hold ends when the repair ends (CompleteRepair), when its worker lets
// it go on any other way out of the object (ReleaseRepair), when its job
// ends (EndRepairJob), or at its HeldUntil: the due its unit's lease had at
// the record, plus RepairHoldGrace. A worker cut off from the database
// cannot let go, and its put may still be streaming; so every put of a
// repair ends, by the worker's own deadline, before that due, and the grace
// past it covers a put cut at its deadline that the storage still lands,
// and the difference between the worker's clock and the database's. A
// worker that stopped never blocks the scan for longer than its lease and
// the grace.

// RepairHoldGrace is how long a repair's hold stands past the due its
// unit's lease had when the repair was recorded: longer than a storage may
// take to land a put its writer cut at its deadline, plus the difference
// between a worker's clock and the database's. A change of the scan waits
// that long after a worker that lost the database, never less, so the
// bytes it was putting cannot land after the change.
const RepairHoldGrace = 2 * time.Minute

// ScanRepairing is the phrase of the refusal of a change to a scan while a
// repair of one of its objects holds it.
const ScanRepairing = "a repair of an object of the scan is in progress; try again shortly"

// ErrScanRepairing is in the chain of that refusal, so a caller tells it
// from a precondition that will not pass by itself.
var ErrScanRepairing = errors.New(ScanRepairing)

// ScanRepairingError returns the refusal of a change to a scan a repair
// holds: a precondition with ErrScanRepairing in its chain.
func ScanRepairingError() error {
	return fault.Wrap(fault.Precondition, ErrScanRepairing, ScanRepairing)
}

// RepairJobEnded is the phrase of a repair refused because its job has
// ended or was canceled: a job canceled while it runs puts nothing more.
const RepairJobEnded = "the repair job has ended or was canceled; it repairs nothing more"

// ErrRepairJobEnded is in the chain of that refusal, so the job's worker
// stops its walk on it rather than going on to the next object.
var ErrRepairJobEnded = errors.New(RepairJobEnded)

// RepairJobEndedError returns the refusal of a repair whose job has ended:
// a precondition with ErrRepairJobEnded in its chain.
func RepairJobEndedError() error {
	return fault.Wrap(fault.Precondition, ErrRepairJobEnded, RepairJobEnded)
}

// RepairCatalogChanged is the phrase of a repair refused, or ended without
// clearing its finding, because the catalog no longer names the bytes it
// expects at its key.
const RepairCatalogChanged = "the catalog no longer names the bytes the repair expects at the object's key"

// RepairCatalog is what the catalog holds now of the object a repair names,
// read in the transaction that checks it.
type RepairCatalog struct {
	// ScanFound says the catalog holds the scan at the finding's labels;
	// Storage is the storage it is on, ManifestKey the key of its manifest
	// and Manifest the manifest's digest (zero for none).
	ScanFound   bool
	Storage     string
	ManifestKey string
	Manifest    SHA256
	// InstanceFound says the scan holds a live instance of the repair's SOP
	// Instance UID (one a correction hid is not live); Key is its object's
	// key, Offset and Length the instance's place in it, Size and SHA256 the
	// archived file's.
	InstanceFound        bool
	Key                  string
	Offset, Length, Size int64
	SHA256               SHA256
}

// CheckRepairCatalog returns a precondition failure unless the catalog, as
// c holds it, still names at r's key exactly the bytes r expects: for an
// instance, its live row with that key, whole in its object, of that size
// and SHA-256 (FIPS 180-4); for a scan's manifest, that digest. A repair of
// any other object is refused: the catalog names no checksum of it.
func CheckRepairCatalog(r Repair, c RepairCatalog) error {
	switch {
	case !c.ScanFound:
		return Precondition(RepairCatalogChanged + ": the catalog no longer holds the scan")
	case c.Storage != r.Storage:
		return Precondition(RepairCatalogChanged + ": the scan is on another storage now")
	case r.SOPInstanceUID == "":
		if r.Key != c.ManifestKey {
			return Precondition("the catalog names no checksum of the object, which is not its scan's manifest")
		}
		if c.Manifest.IsZero() || c.Manifest != r.ExpectedSHA256 {
			return Precondition(RepairCatalogChanged + ": the scan's manifest digest changed")
		}
		return nil
	case !c.InstanceFound:
		return Precondition(RepairCatalogChanged + ": the catalog no longer holds the instance")
	case c.Key != r.Key:
		return Precondition(RepairCatalogChanged + ": the catalog names another key for the instance now")
	case c.Offset != 0 || c.Length != c.Size:
		return Precondition("the object holds more than the instance's file, and the catalog names no checksum of it whole")
	case c.Size != r.ExpectedSize || c.SHA256 != r.ExpectedSHA256:
		return Precondition(RepairCatalogChanged + ": the instance's size or checksum changed")
	}
	return nil
}
