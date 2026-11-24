// Package dirimport carries out import jobs: it walks a directory under one
// of the installation's import roots and hands each DICOM file in it to the
// tenant's import receiver, through the same reception pipeline a DICOM
// listener hands its files to (receive.Sink), one file at a time.
//
// # Units, leases and the supervisor
//
// An import job's work is units of the tenant's leased queue
// (tenant.ImportWorkKind), as a send job's are: the listing unit, whose
// folder is the job's path, queues one unit per folder at the layout's unit
// depth (tenant.ImportWork.AddImportUnits), a page at a time, and judges the
// files above that depth; each folder's unit walks its folder. One
// supervisor runs every tenant's units, at most import.max-units of one
// tenant and import.server-max-units of all tenants at once, both read
// live, so an import never takes the pipeline from live receivers. Each
// unit renews its lease while it works; a unit whose lease is lost stops
// and writes nothing more, and the next attempt resumes after its cursor.
//
// # The walk
//
// A unit opens its root's directory with os.Root and each segment below it
// by its type as read without following a link (Lstat), then opens it and
// checks that what it opened is what it read (os.SameFile): a link, wherever
// it is in the tree, is skipped as not_regular, and nothing outside the
// root is ever opened. A directory is read as names only, at most
// import.max-dir-entries of them, sorted, then visited in
// importpath.Compare's order (a directory's whole subtree before the next
// name beside it), so a unit's cursor is a path in that order and the walk
// resumes after it. A directory with more names is skipped whole as
// directory_too_wide. For each regular file, in order: a path longer than
// importpath.MaxPathLength is skipped as path_too_long; the layout and the
// map give its parts or skip it (outside_layout, unmapped_session); a file
// larger than the receiver's largest file is skipped as too_large; its first
// 132 bytes must be the Part 10 preamble and "DICM" (PS3.10 7.1) or it is
// skipped as not_part10. Every other file is handed in with its arrival
// (tenant.ImportArrival: the unit's fence, its path, its size and parts),
// and the unit goes on only once its record is written, in one transaction
// with the unit's cursor; a skip is recorded the same way
// (tenant.ImportWork.RecordImportSkip). So no file is handed in twice.
//
// # What it must never do
//
// It never writes, moves or deletes anything under a root. A path can
// identify a person: no log line, error or event of this package holds a
// path, a part's value or a map's cell; paths are kept only in the tenant's
// database, by the store.
package dirimport
