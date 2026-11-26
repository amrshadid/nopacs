package tenant

import (
	"context"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
)

// A storage's coverage is computed from when each scan was last read whole
// on each storage: a scan's unit records it at its end on every copy it
// read (RecordScanVerified), under its lease, keyed by the storage's name
// and the scan's Series Instance UID, never by an integer id, so what was
// recorded survives a rebuild that makes the catalog's ids again. A scrub
// of one storage, a scrub of the whole tenant and a request to scrub one
// instance all read objects, and only the time of the read says whether an
// object is covered, so coverage is never computed from the job counts.

// ScanVerified is what a scan's unit records of one copy of a scan it read
// to its end: the scan on the storage, and when.
type ScanVerified struct {
	// Fence is the unit's lease, so a unit that lost it records nothing.
	Fence WorkFence
	// Storage names the storage read: the scan's own, or one of its
	// replicas.
	Storage string
	// Path names the scan.
	Path label.Path
	// ByPresence says some of its objects were checked by presence only
	// (the archive tier), not read.
	ByPresence bool
	// At is when the unit ended the scan on the storage.
	At time.Time
}

// Validate returns an Invalid error unless v can be recorded.
func (v ScanVerified) Validate() error {
	if err := v.Fence.Validate(); err != nil {
		return err
	}
	if err := ValidateName("storage", v.Storage); err != nil {
		return err
	}
	if err := ValidateScanPath(v.Path); err != nil {
		return err
	}
	return master.ValidateTime("verification time", v.At)
}

// VerificationRecorder records when each scan was last read whole on each
// storage: the ground of coverage.
type VerificationRecorder interface {
	// RecordScanVerified records v, replacing what was recorded of the
	// same scan and storage by the same kind of read (whole, or by
	// presence), under v.Fence; a fence that no longer holds is
	// ErrLeaseLost. A scan the catalog no longer holds records nothing.
	RecordScanVerified(ctx context.Context, v ScanVerified) error
}

// ScanVerification is what is recorded of one scan on one storage, as a
// coverage reads it.
type ScanVerification struct {
	// Objects counts the scan's objects the catalog holds.
	Objects int64
	// Held is when the scan's first object was archived: an object never
	// verified is overdue once one interval has passed since.
	Held time.Time
	// Verified is when the scan was last read whole on the storage; zero
	// for never.
	Verified time.Time
	// Presence is when its objects were last checked by presence only;
	// zero for never.
	Presence time.Time
}

// VerificationSource answers what is recorded of the scans of a storage,
// a page at a time.
type VerificationSource interface {
	// ScanVerifications answers a page of at most limit (1 to
	// MaxVerificationPage) of the scans the storage holds (a replica holds
	// its primary's), after the cursor ("" for the first page), and the
	// cursor of the next page, "" after the last. A storage the tenant
	// does not have is NotFound.
	ScanVerifications(ctx context.Context, storage, after string, limit int) ([]ScanVerification, string, error)
}

// MaxVerificationPage is the most scans one ScanVerifications answers.
const MaxVerificationPage = 10000

// ValidateVerificationPage returns an Invalid error unless a page of
// ScanVerifications can be read with these arguments, and the scan id
// after which the page starts (0 for the first page).
func ValidateVerificationPage(storage, after string, limit int) (int64, error) {
	if err := ValidateName("storage", storage); err != nil {
		return 0, err
	}
	if limit < 1 || limit > MaxVerificationPage {
		return 0, invalid("a page of verifications holds 1 to " + strconv.Itoa(MaxVerificationPage) + " scans")
	}
	if after == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(after, 10, 64)
	if err != nil || n < 1 {
		return 0, invalid("a cursor of verifications is one a page answered")
	}
	return n, nil
}
