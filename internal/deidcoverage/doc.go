// Package deidcoverage is the coverage job (kind tenant.JobDeidCoverage): it
// builds the tenant's report of the elements that files held as received and
// no rule of their profile named, from what each scan's metadata file records
// of each instance (sidecar.Instance.Unnamed).
//
// The job reads a page of scans in order of id, reads each scan's metadata
// file once, line by line within the reader's bounds and outside the
// database, adds up the elements the lines record, and writes the page's
// elements and the place it reached in one transaction
// (tenant.DeidCoverages.ApplyDeidCoverage): a restart goes on after the last
// page written, and a page written by another runner is refused, so no scan
// is counted twice. When no scan lies past the place, the job ends and its
// elements are the report, those of the job before dropped in the same
// transaction.
//
// It reads no instance's file and holds no value: a line records a tag, a VR,
// a count and, for a private element, a creator the engine judged to be a
// vendor's name and not a value (a creator that held one is withheld in the
// line itself). A scan whose metadata is missing or not in its form is
// skipped and counted; the job then ends partial. A line written before the
// field existed, or of a file archived as received, is counted as read but not
// as recorded. A storage that does not answer is not a skip: the pass stops
// with the error, and the next pass goes on from the same place.
package deidcoverage
