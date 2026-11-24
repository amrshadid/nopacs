package tenant

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// Importing is what the tenant's database keeps for an import job: the job
// and what it reads, its units with their cursors, the files it skipped and
// those it handed in with what became of each, and its counts. Imports and
// ImportWork are its two concerns; PipelineParts embeds it, so a driver
// implements it whole.
//
// # How an import job is carried out
//
// An import job is a row of the jobs (Job, kind JobImport) and the row of
// what it reads (ImportJob). Its work is units of the tenant's leased
// queue, of the kind ImportWorkKind, each fenced by its attempt as a send
// job's are (WorkFence), keyed by the job's and the unit's ids
// (ImportUnitKey), never a path: first the listing unit (ImportUnitID(0)),
// whose folder is the job's path, which queues one unit per folder at the
// layout's unit depth (AddImportUnits) and judges the files above that
// depth; then each folder's unit, which walks its folder.
//
// A unit judges one entry at a time, in importpath.Compare's order, and
// writes its record in one transaction with the unit's cursor (the entry's
// path) and its counts, Found and the skip or HandedIn, fenced: a skip by
// RecordImportSkip, a file handed in by the record of its arrival, which is
// its received file (ReceivedFile.Import, AddReceivedFiles) or, for a file
// held as it arrived, its quarantine record (QuarantinedFile.Import,
// HoldFiles). So after a crash or a lost lease the next attempt resumes
// after the cursor, and no file is handed in twice: HandedAgain stays 0. A
// write naming the path the cursor holds was recorded before and changes
// nothing, so a write sent again after its answer was lost is harmless; a
// path at or before the cursor otherwise, or outside the unit's folder, is
// a precondition failure; a write whose fence no longer holds is
// ErrLeaseLost and records nothing, its batch included.
//
// A unit has one cursor, which its folder pages (AddImportUnits), its
// skips and its arrivals all move. So the listing unit, which both queues
// folders and judges the files above their depth, sends each page of
// folders before it records any skip or arrival of a path after the
// page's first folder: a skip or arrival recorded first would move the
// cursor past folders not yet queued, and the page would be refused as
// before the cursor.
//
// An entry whose path is longer than importpath.MaxPathLength (a file, or
// a directory, which the unit does not enter) is skipped with
// SkipPathTooLong, its path as it is (up to importpath.MaxEntryPathLength,
// the longest a walk that enters only directories that fit can meet): the
// cursor moves to that path, and the row is kept under its deepest
// ancestor that fits (importpath.Fitting), which every entry too long in
// one directory shares, each counted once. A file is never handed in, nor
// a folder queued, with a path that does not fit.
//
// # Outcomes
//
// Each file handed in is an arrival row (the job, the path, the unit, and
// the reception, SOP Instance UID and received checksum of its received
// file), its own row rather than columns of the received file: a reception
// keeps one received file per UID and checksum and removes them when it
// ends, and an import's file must keep its outcome in both cases (one
// instance in two folders is two files, each with an outcome). The
// pipeline's deciding transactions give each arrival its outcome, in the
// transaction that decides it, moving it out of Processing, by the rules
// of import_outcome.go:
//
//   - CommitScan: archived, for the first arrival in path order of each
//     instance it confirms; already held, for any other of the same file
//     (ImportCommitOutcomes);
//   - HoldFiles: quarantined by the record's reason, or lost for
//     QuarantineIncomingLost (ImportHeldOutcome), for the arrivals of the
//     record's reception, UID and checksum; the record names the job and
//     the path (one of them, the first in path order);
//   - a step to an end, for the arrivals still processing
//     (ImportEndOutcome): to done, already held, which the pipeline's own
//     rule makes duplicates (a reception done has every received file
//     archived, ignored as the same file, or quarantined); to failed
//     (StepReception, or a unit of work held as failed), failed;
//     to quarantined, failed: the pipeline holds every file of a reception
//     before it quarantines it, so none is left, and one left anyway is not
//     claimed held;
//   - RetryReception moves the failed arrivals of a failed reception back
//     to processing, into the new reception; DiscardReception makes them
//     discarded.
//
// A quarantined file later released keeps its first outcome; a record a
// release quarantines again keeps the job and the path of the record it
// came from. Outcomes are counted whatever the job's state, so a file
// decided after its job ended (canceled, failed, settling timed out) still
// moves the counts.
//
// # How an import job ends
//
// A job is listed once the listing unit's walk ends. A unit whose walk ends
// is walked, then done once nothing it handed in is processing. Once the
// job is listed and every unit's walk has ended, the job ends, in the
// transaction that makes it so (a walk's end, or the outcome that brings
// Processing to zero), in ImportEndState's state; while files are still
// processing it stays running with the reason ImportReasonSettling. A unit
// that fails (FailImportUnit, or its attempts running out) ends the job
// failed once every walk has ended, without waiting for its files, which
// are still counted. TimeOutImportSettling ends partial a job still
// settling. CancelJob ends an import job canceled with its units that are
// queued; a unit walking learns of it from the job's state its writes
// answer, and ends its walk: a skip or a folder page is answered with the
// job canceled, and a file handed in is refused with ErrImportEnded in
// the chain (ImportEnded), its batch recording nothing, so the worker ends
// its walk and lets the copy it landed in the incoming area go. A cancel
// judges the job's state again once it holds the job's rows, so a job a
// walk's end or an outcome ended meanwhile is not turned canceled.
//
// # Paths
//
// A path can identify a person (a folder named after a patient). The store
// keeps paths only in its own rows (the job's path and map file, a unit's
// folder and cursor, a skip's or an outcome's row, an arrival, a
// quarantine record's ImportPath), and no error it returns holds one: a
// path's fault is named by its segment's number (importpath), a refusal
// says which rule failed, and a list's cursor holds a row's number, never
// its path. Paths are kept as ImportPathKey's bytes where an index orders
// them.
//
// # Several writers
//
// Under several writers (Postgres) the rows are locked in one order: a
// unit of work's row (its fence), a reception's, an import unit's (several
// in order of job, then unit id), the import job's, then the job's; a
// cancel skips a unit of work's row a transaction holds, as a send job's
// cancel does. Every change of an import job's state locks its import
// job's row first, so a transaction holding that row reads the job's
// state as it will stay to its commit. The events of the import jobs a
// transaction ends are written last in it, after its own.
type Importing interface {
	Imports
	ImportWork
}

// Imports keeps a tenant's import jobs and answers what became of them.
type Imports interface {
	// CreateImportJob makes an import job, queued, its listing unit, and
	// its unit of work, and returns the job. A receiver the tenant does not
	// have, and another import job of the same receiver queued or running
	// whose path overlaps this one's (importpath.Overlap), are
	// precondition failures; an id taken is a Conflict. The key that makes
	// it is its audit entry's. That the receiver is an import receiver,
	// and that its root, map and layout allow the job, are the caller's to
	// check.
	CreateImportJob(ctx context.Context, j NewImportJob, audit master.AuditEntry, guard Guard) (Job, error)
	// ImportJob returns the import job of that id with its Import whole,
	// the inline map included; NotFound when the tenant has no job of that
	// id, and a precondition failure for a job of another kind.
	ImportJob(ctx context.Context, id string) (Job, error)
	// ListImportUnits returns a page of an import job's units that the
	// filter keeps, in importpath.Compare's order of their folders (the
	// listing unit first). A job not there is NotFound; one of another
	// kind a precondition failure.
	ListImportUnits(ctx context.Context, jobID string, filter ImportUnitFilter, page master.Page) ([]ImportUnit, string, error)
	// ListImportFiles returns a page of an import job's files that did not
	// reach the archive and that the filter keeps, in importpath.Compare's
	// order of their paths, then by kind; at most the job's FailuresKept
	// are listed (ImportJob.FilesNotListed counts the others). A file
	// failed and then retried is no longer listed. Errors as
	// ListImportUnits.
	ListImportFiles(ctx context.Context, jobID string, filter ImportFileFilter, page master.Page) ([]ImportFile, string, error)
}

// ImportWork is the work of an import job's units, each write fenced by
// the unit's lease. A unit is one the store queued for an import job; any
// other is a precondition failure.
type ImportWork interface {
	// StartImportUnit says what the leased unit is and how far it got
	// (ImportUnit.Cursor), moves it from queued to walking, and moves its
	// job from queued to running (its event) at its first unit. A unit of
	// a job that has ended is answered with the job's state: the worker
	// ends its walk at once (EndImportWalk).
	StartImportUnit(ctx context.Context, fence WorkFence, now time.Time) (ImportUnit, Job, error)
	// AddImportUnits queues a unit for each folder (ValidateImportFolders),
	// which lie under the job's path, after the listing unit's cursor, and
	// moves the cursor to the last; it is the listing unit's alone. A
	// folder queued before is not queued again, so a page written again
	// after a crash changes nothing; a folder at the cursor was. A job
	// that has ended (canceled) queues nothing and is answered with its
	// state.
	AddImportUnits(ctx context.Context, fence WorkFence, folders []string, now time.Time) (Job, error)
	// RecordImportSkip records an entry the unit judged and did not hand
	// in: its row (listed while the job lists fewer than its
	// FailuresKept), the unit's cursor, and Found and the skip on the unit
	// and the job, in one transaction. It returns the job, whose state
	// tells a walking unit of a cancel.
	RecordImportSkip(ctx context.Context, s ImportSkip) (Job, error)
	// EndImportWalk ends the unit's walk and completes its unit of work:
	// walked, or done when nothing it handed in is processing, or canceled
	// when its job was canceled; for the listing unit, the job is listed.
	// It ends the job when it was the last walk and nothing is processing
	// (ImportEndState), and sets its reason ImportReasonSettling while
	// files are. A walk ended already is answered as it stands.
	EndImportWalk(ctx context.Context, fence WorkFence, now time.Time) (Job, error)
	// FailImportUnit ends the unit's walk failed with reason (one of the
	// fixed phrases: ImportReasonRootUnavailable) and completes its unit of
	// work; what it handed in still goes on to its outcome. The job ends
	// failed once every walk has ended.
	FailImportUnit(ctx context.Context, fence WorkFence, reason string, now time.Time) (Job, error)
	// ReleaseImportUnit gives the leased unit back, due again at due (not
	// before now), with reason as its last error, without spending an
	// attempt, as ReleaseJobUnit does: for a cause not the unit's own (its
	// receiver disabled, the archive refusing files for now). Its cursor
	// and counts are kept. A unit whose job has ended is completed instead,
	// its walk ended canceled.
	ReleaseImportUnit(ctx context.Context, fence WorkFence, due time.Time, reason string, now time.Time) error
	// TimeOutImportSettling ends partial, with ImportReasonSettlingTimedOut,
	// an import job that is listed, every walk ended, and files still
	// processing; any other job is answered as it stands. When to call it
	// (the receiver's longest open time and an hour after SettlingSince)
	// is the caller's to decide. The job's counts go on moving.
	TimeOutImportSettling(ctx context.Context, jobID string, now time.Time) (Job, error)
	// ArrivalParts returns the parts (ImportArrival.Parts) of the first
	// arrival, in path order, of the reception's received file with that
	// SOP Instance UID and received checksum, and true; nil and false when
	// the file has no arrival (it came by another way). The pipeline names
	// an import's files by them. It is a read, fenced by nothing.
	ArrivalParts(ctx context.Context, receptionID, sopInstanceUID string, sum SHA256) (map[string]string, bool, error)
}
