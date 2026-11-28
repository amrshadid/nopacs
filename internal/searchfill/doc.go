// Package searchfill is the search backfill job (kind
// tenant.JobSearchBackfill): it fills the search tables' columns of an
// attribute added after scans were archived, for the scans already held.
//
// A scan's search row is read from the first line of its metadata file, the
// instance with the lowest SOP Instance UID, as the archive step reads it
// (search.ExtractJSON), so a backfilled row is the row an archive of the
// same files makes. The job reads a page of scans in order of id, reads each
// scan's metadata outside the database, and writes the page and the place it
// reached in one transaction (tenant.SearchBackfills.ApplySearchBackfill):
// a restart goes on after the last page written, a page written by another
// runner is refused, and a row written by an archive step meanwhile is never
// overwritten by an older read (the write changes a row only when its source
// is the file the metadata holds first, or a higher one). When no scan lies
// past the place, the job ends: the attributes become ready to search and
// the table is analyzed, in one transaction.
//
// A scan whose metadata is missing or not in its form is skipped and
// counted; the job then ends partial, and the scan keeps no value for the
// attribute, as a scan whose file holds none. A storage that does not answer
// is not a skip: the pass stops with the error, and the next pass goes on
// from the same place.
package searchfill
