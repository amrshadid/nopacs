package tenant

import (
	"context"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
)

// JobDeidCoverage is the kind of a coverage job: the report of the elements
// archived files held that no rule of their profile named is built again,
// from what each scan's metadata file records of each instance
// (DeidCoverages).
const JobDeidCoverage JobKind = "deid_coverage"

// The bounds of a coverage report.
const (
	// MaxDeidCoveragePage is the most scans one DeidCoverageScans call
	// answers and one ApplyDeidCoverage call writes.
	MaxDeidCoveragePage = 500
	// MaxCoverageElements is the most distinct elements a report holds, and a
	// page of a job's write lists; occurrences past it are counted in the
	// report's NotListed, never dropped silently.
	MaxCoverageElements = 100000
	// MaxCoverageCreator is the longest private creator a report names: the
	// length of an LO value (PS3.5 6.2).
	MaxCoverageCreator = 64
	// MaxCoverageElementsPage is the most elements one DeidCoverageElements
	// call answers: one more than the largest page of a list, which a caller
	// reads to know whether another page follows.
	MaxCoverageElementsPage = master.MaxPageLimit + 1
)

// DeidCoverages is what the tenant's database keeps of coverage reports. A
// report is the elements, by tag (or private creator and element), VR and
// counts, that files held as received and no rule named; it holds no value,
// and a private creator only where it is printable ASCII a vendor wrote and
// held no value the profile removed. The job reads each scan's metadata file
// (outside the database, in the runner), and ApplyDeidCoverage adds the
// counts of a page of scans and moves the job's place past it in one
// transaction, so a restart goes on after the last page written and no scan
// is counted twice. The report is the elements of the latest job that ended
// done or partial; the end of a job drops the elements of the ones before.
type DeidCoverages interface {
	// QueueDeidCoverage makes a coverage job of q.ID (kind JobDeidCoverage) in
	// one transaction with its audit entry and event, and answers it. It is
	// refused while another coverage job has not ended (one at a time) and
	// while a rebuild runs.
	QueueDeidCoverage(ctx context.Context, q DeidCoverageQueue, audit master.AuditEntry, guard Guard) (Job, error)
	// DeidCoverageJob answers the coverage job of that id: the job, the last
	// scan it counted and its counts; NotFound for none.
	DeidCoverageJob(ctx context.Context, id string) (DeidCoverageJob, error)
	// DeidCoverageInProgress answers the coverage job that has not ended, and
	// whether there is one.
	DeidCoverageInProgress(ctx context.Context) (Job, bool, error)
	// DeidCoverageScans returns at most limit (1 to MaxDeidCoveragePage) scans
	// with an id after the one given, in order of id.
	DeidCoverageScans(ctx context.Context, after int64, limit int) ([]DeidCoverageScan, error)
	// ApplyDeidCoverage adds the elements of a page of scans to the job's and
	// moves its place past the page, in one transaction, counting each scan
	// read or skipped; a job queued becomes running, with its event. The page
	// must begin where the job stands (A.After is the job's place), or it is
	// refused as a precondition failure and nothing is written, so a second
	// runner or a restart never counts a page twice. A job ended refuses it.
	ApplyDeidCoverage(ctx context.Context, a DeidCoverageApply) (DeidCoverageJob, error)
	// EndDeidCoverage ends the job once no scan lies past its place: the
	// elements of the jobs before are dropped, so its are the report, and the
	// job ends done, or partial when any scan was skipped; one ended already
	// is answered as it is.
	EndDeidCoverage(ctx context.Context, id string, now time.Time) (Job, error)
	// StopDeidCoverage ends the job failed with reason (1 to
	// MaxFailureLength bytes); its elements are dropped and the report before
	// it stays. One ended already is answered as it is.
	StopDeidCoverage(ctx context.Context, id, reason string, now time.Time) (Job, error)
	// DeidCoverageReport answers the report of the latest job that ended done
	// or partial, and whether there is one.
	DeidCoverageReport(ctx context.Context) (DeidCoverageReport, bool, error)
	// DeidCoverageElements returns at most limit (1 to MaxCoverageElementsPage)
	// elements of the report of the job with that id, in order of tag, then
	// creator, then withheld, then VR, after the element given (nil for the
	// first page).
	DeidCoverageElements(ctx context.Context, jobID string, after *CoverageKey, limit int) ([]CoverageElement, error)
}

// DeidCoverageQueue is a coverage job to make.
type DeidCoverageQueue struct {
	// ID is the job's id, given by the caller.
	ID string
	// KeyID is the key that asked, empty for the local operator.
	KeyID string
	// Now is when.
	Now time.Time
}

// Validate returns an Invalid error unless q is well formed.
func (q DeidCoverageQueue) Validate() error {
	if err := master.ValidateID("job id", q.ID); err != nil {
		return err
	}
	if q.KeyID != "" {
		if err := master.ValidateID("key id", q.KeyID); err != nil {
			return err
		}
	}
	return master.ValidateTime("time", q.Now)
}

// DeidCoverageJob is a coverage job's own part.
type DeidCoverageJob struct {
	// Job is the job.
	Job Job
	// After is the id of the last scan the job counted: the next page begins
	// after it. Zero before the first.
	After int64
	// Skipped counts the scans whose metadata could not be read.
	Skipped int64
	// Instances counts the instances' lines read, Recorded those that recorded
	// their unnamed elements, Capped those that recorded only the first of
	// their distinct elements.
	Instances, Recorded, Capped int64
	// NotListed counts the occurrences of elements the report does not list:
	// those a line listed past its bound, and those of an element past
	// MaxCoverageElements.
	NotListed int64
	// Elements is how many distinct elements the job's report holds so far.
	Elements int64
}

// DeidCoverageScan is a scan the job reads.
type DeidCoverageScan struct {
	// ID is the scan's id in the catalog.
	ID int64
	// Path is its labels; Storage the storage its files are on.
	Path    label.Path
	Storage string
}

// CoverageKey names one element of a report: a standard element by its tag,
// a private one by its tag with the block number written 00, its creator,
// and whether the creator is withheld; with its VR.
type CoverageKey struct {
	// Tag is the tag, gggg00ee for a private element.
	Tag uint32
	// Creator is the private creator, printable ASCII of at most
	// MaxCoverageCreator bytes; empty for a standard element, an element of a
	// block with no creator, and when Withheld.
	Creator string
	// Withheld says the creator is not given.
	Withheld bool
	// VR is two upper case letters.
	VR string
}

// Private reports whether the key is of a private element.
func (k CoverageKey) Private() bool { return (k.Tag>>16)%2 == 1 }

// Validate returns an Invalid error unless k can be stored.
func (k CoverageKey) Validate() error {
	switch {
	case k.Private() && k.Tag&0xFF00 != 0:
		return invalid("a private element's block number is written 00")
	case len(k.VR) != 2 || k.VR[0] < 'A' || k.VR[0] > 'Z' || k.VR[1] < 'A' || k.VR[1] > 'Z':
		return invalid("a coverage element's VR is two upper case letters")
	case !k.Private() && (k.Creator != "" || k.Withheld):
		return invalid("only a private element has a creator")
	case k.Withheld && k.Creator != "":
		return invalid("a withheld creator is not given")
	case len(k.Creator) > MaxCoverageCreator:
		return invalid("a private creator is at most " + strconv.Itoa(MaxCoverageCreator) + " bytes")
	}
	for i := range len(k.Creator) {
		if c := k.Creator[i]; c < 0x20 || c > 0x7E {
			return invalid("a private creator is printable ASCII")
		}
	}
	return nil
}

// CoverageElement is one element of a report with its counts.
type CoverageElement struct {
	CoverageKey
	// Instances is how many instances held it, Occurrences how many times in
	// all.
	Instances, Occurrences int64
}

// DeidCoverageScanRow is what the runner read of one scan of a page.
type DeidCoverageScanRow struct {
	// ID is the scan's id.
	ID int64
	// Skipped says the scan's metadata could not be read (it is missing or not
	// in its form); the other fields are then zero.
	Skipped bool
	// Instances, Recorded, Capped and NotListed are the scan's counts, as
	// DeidCoverageJob names them.
	Instances, Recorded, Capped, NotListed int64
}

// DeidCoverageApply is one page of a coverage job to write.
type DeidCoverageApply struct {
	// JobID is the job.
	JobID string
	// After is the place the page begins at: the job's After.
	After int64
	// Scans are the scans of the page in order of id, each past After, the
	// last being the place the job reaches.
	Scans []DeidCoverageScanRow
	// Elements are the page's elements, each once, with the counts of every
	// scan of the page together. At most MaxCoverageElements.
	Elements []CoverageElement
	// Now is when.
	Now time.Time
}

// Validate returns an Invalid error unless a can be written.
func (a DeidCoverageApply) Validate() error {
	if err := master.ValidateID("job id", a.JobID); err != nil {
		return err
	}
	if len(a.Scans) == 0 || len(a.Scans) > MaxDeidCoveragePage {
		return invalid("a page of a coverage job holds 1 to " + strconv.Itoa(MaxDeidCoveragePage) + " scans")
	}
	last := a.After
	for _, s := range a.Scans {
		switch {
		case s.ID <= last:
			return invalid("the scans of a page of a coverage job are in order of id, past its start")
		case s.Instances < 0 || s.Recorded < 0 || s.Capped < 0 || s.NotListed < 0 || s.Recorded > s.Instances || s.Capped > s.Recorded:
			return invalid("a scan's coverage counts are not negative, and recorded and capped are within instances")
		case s.Skipped && (s.Instances != 0 || s.Recorded != 0 || s.Capped != 0 || s.NotListed != 0):
			return invalid("a scan skipped has no counts")
		}
		last = s.ID
	}
	if len(a.Elements) > MaxCoverageElements {
		return invalid("a page of a coverage job lists at most " + strconv.Itoa(MaxCoverageElements) + " elements")
	}
	seen := make(map[CoverageKey]bool, len(a.Elements))
	for _, e := range a.Elements {
		if err := e.Validate(); err != nil {
			return err
		}
		if e.Instances < 1 || e.Occurrences < e.Instances || seen[e.CoverageKey] {
			return invalid("an element of a coverage page is listed once, held by at least one instance and at least as many times")
		}
		seen[e.CoverageKey] = true
	}
	return master.ValidateTime("time", a.Now)
}

// Reached returns the id of the last scan of the page: where the job stands
// once it is written.
func (a DeidCoverageApply) Reached() int64 { return a.Scans[len(a.Scans)-1].ID }

// DeidCoverageReport says what the stored report was made from.
type DeidCoverageReport struct {
	// JobID is the job that made it and Ended when it ended.
	JobID string
	Ended time.Time
	// Partial says some scan could not be read.
	Partial bool
	// Scans is how many scans the job read, ScansSkipped how many it could not.
	Scans, ScansSkipped int64
	// Instances, Recorded, Capped and NotListed are the report's counts, as
	// DeidCoverageJob names them.
	Instances, Recorded, Capped, NotListed int64
	// Elements is how many distinct elements the report lists.
	Elements int64
}
