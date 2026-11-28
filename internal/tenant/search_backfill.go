package tenant

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/search"
)

// JobSearchBackfill is the kind of a search backfill job: the scans archived
// before a search attribute was added are given its column, read from each
// scan's metadata file, and the attribute is then ready to search
// (SearchBackfills).
const JobSearchBackfill JobKind = "search_backfill"

// SearchBackfills is what the tenant's database keeps of search backfills.
// An attribute added to the search tables after scans were archived starts
// in state building, which every search naming it refuses, since it would
// answer from the scans archived since. The job reads each scan's metadata
// file (outside the database, in the runner), and ApplySearchBackfill writes
// the rows read and the place reached in one transaction, so a restart goes
// on after the last page written and no scan is written twice from a stale
// read: a row is written only where its source is the file the metadata
// holds first or a lower one (search.LowestOrSame), as an archive step
// writes it. The job's end sets every attribute the job covers to ready.
type SearchBackfills interface {
	// QueueSearchBackfill makes a search backfill job of q.ID (kind
	// JobSearchBackfill) covering every attribute in state building, in one
	// transaction with its audit entry and event, and answers it. The
	// attribute q names must be one of them: an attribute already ready is a
	// precondition failure (nothing to fill), one the search tables do not
	// hold is Invalid. It is refused while another search backfill has not
	// ended (one at a time) and while a rebuild runs.
	QueueSearchBackfill(ctx context.Context, q SearchBackfillQueue, audit master.AuditEntry, guard Guard) (Job, error)
	// SearchBackfillJob answers the search backfill job of that id: the job,
	// the attributes it covers and the last scan it wrote; NotFound for none.
	SearchBackfillJob(ctx context.Context, id string) (SearchBackfillJob, error)
	// SearchBackfillScans returns at most limit (1 to MaxSearchBackfillPage)
	// scans with an id after the one given, in order of id: those the
	// catalog holds and those a delete hid and has not purged (their ids are
	// never a held scan's, which are not reused).
	SearchBackfillScans(ctx context.Context, after int64, limit int) ([]SearchBackfillScan, error)
	// ApplySearchBackfill writes the rows of a page of scans and moves the
	// job's place past it, in one transaction, counting each scan filled or
	// skipped; a job queued becomes running, with its event. The page must
	// begin where the job stands (A.After is the job's place), or it is
	// refused as a precondition failure and nothing is written, so a second
	// runner or a restart never writes a page twice. A scan whose manifest is
	// no longer the one its row was read under (a file was added, removed or
	// edited meanwhile) refuses the whole page with ErrSearchBackfillStale,
	// writing nothing: the runner reads the page again. A job ended refuses
	// it.
	ApplySearchBackfill(ctx context.Context, a SearchBackfillApply) (SearchBackfillJob, error)
	// EndSearchBackfill ends the job once no scan lies past its place: every
	// attribute it covers becomes ready, the search tables are analyzed, and
	// the job ends done, or partial when any scan was skipped; one ended
	// already is answered as it is.
	EndSearchBackfill(ctx context.Context, id string, now time.Time) (Job, error)
	// StopSearchBackfill ends the job failed with reason (1 to
	// MaxFailureLength bytes); its attributes stay building. One ended
	// already is answered as it is.
	StopSearchBackfill(ctx context.Context, id, reason string, now time.Time) (Job, error)
}

// ErrSearchBackfillStale is in the chain of a page of a search backfill
// refused because a scan of it changed after its metadata was read: what
// was read may no longer be what the scan holds, and a row written from it
// would undo the change a correction or an archive step wrote.
var ErrSearchBackfillStale = errors.New("a scan changed after its metadata was read")

// SearchBackfillStaleError returns the precondition failure of a page whose
// scan with the id changed after it was read.
func SearchBackfillStaleError(scanID int64) error {
	return fault.Wrap(fault.Precondition, ErrSearchBackfillStale, "scan "+strconv.FormatInt(scanID, 10)+" changed after its metadata was read; the page is read again")
}

// MaxSearchBackfillPage is the most scans one SearchBackfillScans call
// answers and one ApplySearchBackfill call writes.
const MaxSearchBackfillPage = 500

// SearchBackfillQueue is a search backfill job to make.
type SearchBackfillQueue struct {
	// ID is the job's id, given by the caller.
	ID string
	// Field is the attribute (its keyword) the caller asked to fill.
	Field string
	// KeyID is the key that asked, empty for the local operator.
	KeyID string
	// Now is when.
	Now time.Time
}

// Validate returns an Invalid error unless q is well formed.
func (q SearchBackfillQueue) Validate() error {
	if err := master.ValidateID("job id", q.ID); err != nil {
		return err
	}
	if !search.Known(q.Field) {
		return fault.New(fault.Invalid, "the search tables hold no attribute named "+strconv.Quote(q.Field))
	}
	if q.KeyID != "" {
		if err := master.ValidateID("key id", q.KeyID); err != nil {
			return err
		}
	}
	return master.ValidateTime("time", q.Now)
}

// SearchBackfillJob is a search backfill job's own part.
type SearchBackfillJob struct {
	// Job is the job.
	Job Job
	// Fields are the attributes the job covers, by keyword, in order.
	Fields []string
	// After is the id of the last scan the job wrote: the next page begins
	// after it. Zero before the first.
	After int64
	// Skipped counts the scans whose metadata could not be read, which keep
	// no value for the attributes.
	Skipped int64
}

// SearchBackfillScan is a scan the job reads.
type SearchBackfillScan struct {
	// ID is the scan's id in the catalog.
	ID int64
	// Path is its labels; Storage the storage its files are on.
	Path    label.Path
	Storage string
	// Correction, when not empty, names the delete that hid the scan: the
	// scan is one a delete hid and has not purged, whose row is in the hidden
	// tables, and which an undo brings back. Empty for a scan held now.
	Correction string
	// Manifest is the SHA-256 of its manifest when it was listed, zero when
	// the catalog holds none: it changes with every file the scan gains,
	// loses or has edited, so a row read from the metadata then and written
	// after the manifest changed is a stale one (ApplySearchBackfill).
	Manifest SHA256
}

// SearchBackfillRow is the row read for one scan of a page, or the reason it
// could not be read.
type SearchBackfillRow struct {
	// ID is the scan's id; Manifest the scan's manifest SHA-256 as it was
	// listed (SearchBackfillScan.Manifest), which the metadata was read under.
	ID         int64
	Correction string
	Manifest   SHA256
	// Row is the row read from the scan's metadata file. Ignored when
	// Skipped.
	Row search.Row
	// Skipped says the scan's metadata could not be read (it is missing or
	// not in its form): the scan keeps what its row holds and counts as
	// skipped.
	Skipped bool
}

// SearchBackfillApply is one page of a backfill to write.
type SearchBackfillApply struct {
	// JobID is the job.
	JobID string
	// After is the place the page begins at: the job's After.
	After int64
	// Rows are the scans of the page in order of id, each past After, the
	// last being the place the job reaches.
	Rows []SearchBackfillRow
	// Now is when.
	Now time.Time
}

// Validate returns an Invalid error unless a can be written.
func (a SearchBackfillApply) Validate() error {
	if err := master.ValidateID("job id", a.JobID); err != nil {
		return err
	}
	if len(a.Rows) == 0 || len(a.Rows) > MaxSearchBackfillPage {
		return invalid("a page of a search backfill holds 1 to " + strconv.Itoa(MaxSearchBackfillPage) + " scans")
	}
	last := a.After
	for _, r := range a.Rows {
		if r.ID <= last {
			return invalid("the scans of a page of a search backfill are in order of id, past its start")
		}
		last = r.ID
	}
	return master.ValidateTime("time", a.Now)
}

// Reached returns the id of the last scan of the page: where the job stands
// once it is written.
func (a SearchBackfillApply) Reached() int64 { return a.Rows[len(a.Rows)-1].ID }
