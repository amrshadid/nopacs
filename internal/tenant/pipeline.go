package tenant

import (
	"context"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// Pipeline is a tenant's database with what the reception pipeline keeps in
// it: the tenant's storages and receivers, its receptions and the files they
// received, its quarantine, and the leased units of work that carry a
// reception through its steps. Everything the pipeline must not lose in a
// restart is here, so a server that stops at any moment starts again from
// what was committed.
//
// It is a Store and more. A driver implements it and proves it with the
// contract suite's pipeline groups (tenanttest.Run, its pipeline groups); once every
// driver does, its operations become part of Store itself.
//
// # Rules every driver keeps
//
// Store's rules hold, with these.
//
// A change a person or a key asks for (adding, changing or removing a
// storage or a receiver, retrying or discarding a reception, releasing or
// rejecting a quarantined file) takes its audit entry and a Guard, as
// Store's changes do. The pipeline's own work (opening and stepping a
// reception, recording received files, holding a file in quarantine,
// queuing, claiming and finishing work) is the server's and takes neither:
// it happens once per file or per step, far more often than any person
// acts, and its record is the reception's own row and, once they exist, its
// events.
//
// Times are given by the caller, always: a store never reads a clock. The
// clock that decides when a lease ends, when a unit of work is due, and
// when a reception opened or stepped is the clock of the server process
// that calls, handed over as "now". Correctness never rests on it: a lease
// that ends early or late changes who works a unit and when, but every
// write of a worker is fenced by its attempt number (WorkFence), so a
// worker whose lease was taken over changes nothing, whatever the clocks
// say. Several processes on one tenant (possible on a database that allows
// several writers) must keep their clocks in step to well under a lease,
// as any time service does, or a lease can be taken over early.
//
// A rule that depends on the state of what a request names (a storage that
// holds data, a reception that has not failed, a file already rejected) is
// refused with the kind fault.Precondition (Precondition), so a caller can
// answer it apart from a Conflict, which is a name or a key already taken. A
// sentinel in the chain names a case inside a kind (ErrReceptionClosed,
// ErrLeaseLost, ErrStateChanged), never the kind itself: what a caller
// answers follows from the kind alone.
//
// Every list is paged by a cursor bound to its list, its filter and the
// tenant, and every list, claim and lookup is answered from an index.
// Batches are bounded (MaxReceivedBatch, MaxQuarantineBatch,
// MaxDuplicateBatch, MaxClaimBatch, MaxScanCommit); a larger one is Invalid.
//
// # Instances: claimed, then confirmed
//
// What the tenant holds is its catalog's instance rows, one per SOP Instance
// UID, never a reading of receptions' states. A reception that archives
// first claims its instances (ClaimInstances), before it writes anything to
// a storage; after the files are on the storage and read back, it confirms
// them (CommitScan) in the transaction that writes their scan's catalog
// rows, one scan at a time. A reception that ends, or is discarded or
// retried, releases the claims it did not confirm in the transaction of
// that step; what it confirmed stays held whatever becomes of it. So two
// receptions in flight never both archive one instance: the second is told
// the UID is Claimed, and tries again later, through its leased work. An
// instance row is claimed or held, never both: a held instance is never
// replaced by reception, since replacing an archived file deletes archived
// data, which needs a recorded reason and proof of its replacement.
//
// # A reception is never left without a way to end
//
// A reception that has not ended is receiving (and the server's supervisor
// completes it), or has a unit of work queued or leased that names it
// (Work.ReceptionID). When such a unit is held as failed, its reception
// takes the Fail step in the same transaction, releasing its claims, so no
// claim outlives every worker that could end its reception. A crash between
// two transactions of the caller (a step taken, its next unit not yet
// queued) can still leave a reception without a unit: ReceptionsWithoutWork
// finds them, and the server runs it at start.
//
// # Several writers
//
// A driver on a database with several writers (Postgres at read committed)
// keeps each operation's rules under concurrent calls; the SQLite driver's
// one writer keeps them by running one transaction at a time. What each
// operation must guarantee there:
//
//   - every condition a change depends on (a reception's state and last
//     file time, a unit's attempt, a record's state, a claim's owner) is in
//     the statement that changes the row, or the row is locked by an
//     update before it is read, so a change made meanwhile is seen, never
//     overwritten;
//   - OpenReception: calls at once for one receiver and study return the
//     one reception that won, never a Conflict; arrival numbers stay
//     unique and without gaps under concurrent opens of one study;
//   - AddReceivedFiles: a step and a batch of one reception are ordered
//     (the batch locks the reception's row first), so a quiet step decided
//     before a file is refused (ErrStateChanged), and a batch after the
//     step is refused (ErrReceptionClosed);
//   - ClaimInstances: a claim of one UID by two receptions at once gives it
//     to one; the other waits for that one's commit and is answered
//     Claimed (or a held answer once confirmed). A batch claims its UIDs in
//     sorted order, whatever order the caller gave, so two batches that
//     share UIDs never wait for each other in a cycle;
//   - CommitScan: its subject, session and scan are each found or made by
//     an insert that does nothing when the row is there, then a read, so
//     two commits at once of two studies of one subject share that subject,
//     and a label taken meanwhile is a LabelConflict, never an unclassified
//     failure;
//   - a storage's row is the lock of its use: CommitScan and an Identify
//     step update it (changing nothing) in their transaction, and removing
//     a storage or changing where its files are updates it first and checks
//     whether it is in use after, so neither can pass the other unseen;
//   - HoldFiles locks each reception it names before it counts a file
//     received, as a received batch does;
//   - ClaimWork: two claims at once take different units (on Postgres:
//     FOR UPDATE SKIP LOCKED);
//   - every fenced write locks the unit's row with its attempt in the
//     condition, so a takeover waits for it or it fails;
//   - changes of configuration (storages, receivers) check rules across
//     rows (names, overlaps, ports): a driver serializes them per tenant,
//     as with a lock taken first in each such transaction.
//
// The suite runs these cases on every driver; on SQLite's one writer they
// pass by serialization, and the cases that need true concurrency say so
// where they are.
type Pipeline interface {
	Store
	PipelineParts

	// AddStorage adds a storage, stored as given with Created and Updated
	// set to now, and returns it as a read would. A name taken by another
	// storage is Conflict. An S3 location on the endpoint and bucket of
	// another S3 storage whose prefix starts with this one's, or the other
	// way round, and a directory whose path is, contains or lies inside
	// another directory storage's under the same root (segments compared
	// with ASCII case folded), is a precondition failure.
	AddStorage(ctx context.Context, s Storage, now time.Time, audit master.AuditEntry, guard Guard) (Storage, error)
	// Storage returns the storage of that name, or NotFound. Its secret is
	// never returned: S3.SecretSet says whether one is held.
	Storage(ctx context.Context, name string) (Storage, error)
	// ListStorages returns a page of the storages in order of name.
	ListStorages(ctx context.Context, page master.Page) ([]Storage, string, error)
	// SetStorage changes the fields named in fields to their values in s,
	// whose Name names the storage, and no other; Updated becomes now. The
	// type cannot change, so a field of the other type's configuration is
	// Invalid; the access key id and the secret are named together or not
	// at all, and a secret named is not empty. A change of where its files
	// are is a precondition failure while the storage is in use: it holds
	// an instance, a session names it, or a reception that has not ended
	// names it. The rules of AddStorage
	// apply to the result. An S3 endpoint is kept as NormalizeEndpoint
	// spells it.
	SetStorage(ctx context.Context, s Storage, fields []StorageField, now time.Time, audit master.AuditEntry, guard Guard) (Storage, error)
	// RemoveStorage removes the storage and returns it as it was. A storage
	// a receiver or one of its routes names, or that is in use (it holds an instance, a session
	// names it, so every later arrival of the session goes to it, or a
	// reception that has not ended names it), is a precondition failure
	// whose message says which. A storage not there is NotFound.
	RemoveStorage(ctx context.Context, name string, audit master.AuditEntry, guard Guard) (Storage, error)

	// AddReceiver adds a receiver, with Created and Updated set to now. A
	// name taken by a receiver, or still named by a reception, a
	// quarantined file or an import job (ended or not) of a receiver
	// removed, is Conflict, so a name in a record always means one
	// receiver. A storage the tenant does not have, its own or a route's, a
	// sender a route forwards to that the tenant does not have, and another
	// receiver that would contend for the same port (ReceiversClash), are
	// precondition failures.
	AddReceiver(ctx context.Context, r Receiver, now time.Time, audit master.AuditEntry, guard Guard) (Receiver, error)
	// Receiver returns the receiver of that name, or NotFound.
	Receiver(ctx context.Context, name string) (Receiver, error)
	// ListReceivers returns a page of the receivers in order of name.
	ListReceivers(ctx context.Context, page master.Page) ([]Receiver, string, error)
	// SetReceiver changes the fields named in fields to their values in r,
	// whose Name names the receiver; Updated becomes now. The rules of
	// AddReceiver apply to the result. An r.Updated not zero is the time the
	// caller read the receiver: a stored receiver changed since is a
	// precondition failure (ApplyReceiver).
	SetReceiver(ctx context.Context, r Receiver, fields []ReceiverField, now time.Time, audit master.AuditEntry, guard Guard) (Receiver, error)
	// RemoveReceiver removes the receiver and returns it as it was. Its
	// receptions and quarantined files stay, naming it.
	RemoveReceiver(ctx context.Context, name string, audit master.AuditEntry, guard Guard) (Receiver, error)

	// OpenReception returns the reception of the study at the receiver that
	// is receiving, or makes one with the id given, the next arrival of the
	// study in the tenant, receiving since at; opened says which. A
	// receiver the tenant does not have is a precondition failure, checked
	// first, even while a reception of it is open. Two calls at once for
	// one study and receiver open one reception. A call again, or calls at
	// once, with the id of the reception receiving answer that reception
	// with opened false, never a Conflict; an id taken by a reception that
	// is not the one receiving is a Conflict.
	OpenReception(ctx context.Context, receiver, studyUID, id string, at time.Time) (r Reception, opened bool, err error)
	// Reception returns the reception with its study's arrivals, up to
	// MaxArrivals of the latest, in order of arrival number; or NotFound.
	Reception(ctx context.Context, id string) (Reception, []Arrival, error)
	// ListReceptions returns a page of the receptions the filter keeps,
	// newest first: by start time, then by id, both descending.
	ListReceptions(ctx context.Context, filter ReceptionFilter, page master.Page) ([]Reception, string, error)
	// StepReception moves a reception by one step, as reception.Step allows
	// it from step.From, comparing and setting the state: when the stored
	// state is not step.From, or its last file time not step.LastFile when
	// that is given, nothing changes and the error holds ErrStateChanged; a
	// step the table refuses holds the *reception.Refusal (errors.As),
	// Already among them. Both are Conflict. Identify's storage must be the
	// tenant's (a precondition). A step to an end releases the claims the
	// reception did not confirm, and removes the received files of one it
	// ends done or quarantined (KeepsReceivedFiles), in the same
	// transaction. hook, when not
	// nil, runs inside the step's transaction after the reception is
	// written and before the commit, with the reception as it was and as it
	// is; an error from it undoes the step and is returned as it is. A
	// store that keeps an event feed (Events) writes the step's event itself
	// in the same transaction, before the hook. The hook may run more than once
	// for one step, as a Guard may (Guard): what it writes inside the
	// transaction is undone with it, and it does nothing outside it.
	StepReception(ctx context.Context, step ReceptionStep, hook StepHook) (Reception, error)
	// RetryReception makes a new reception that runs the files of a failed
	// one again: at the receiver named, or the failed one's when receiver
	// is empty; the next arrival of its study, complete since now, with the
	// failed reception's received files and RetryOf set; and sets the
	// failed one's RetryReception. A reception already retried is returned
	// as it stands with the one made then, and nothing more is made, even
	// when its receiver was removed since and none is named. A
	// reception that has not failed or whose files were discarded, a
	// failed one's receiver removed with none named, and a receiver named
	// that does not exist, are precondition failures, which leave the
	// failed reception as it was and write no audit entry. The failed
	// reception's claims not confirmed are released in the same
	// transaction; its received files that are held instances are found
	// SameFile by the new reception's claim and ignored. The store adds the
	// detail DetailRetryReception, the id of the reception that runs the
	// files, to the audit entry.
	RetryReception(ctx context.Context, id, newID, receiver string, now time.Time, audit master.AuditEntry, guard Guard) (failed, retry Reception, err error)
	// DiscardReception records that a failed reception's files left in the
	// incoming area are removed, with the reason and now, releases its
	// claims not confirmed, and returns the reception; the caller then
	// removes the bytes DiscardedFiles lists, and a restart that finds them
	// still there removes them. The quarantine records of the reception
	// stay, with their bytes, until each is released or rejected: a file a
	// record keeps, or whose place another reception still needs, is never
	// in the list (DiscardedFiles). A reception already discarded is
	// returned as it stands with its first reason. A reception that has
	// not failed or that was retried is a precondition failure. The store
	// adds the details DetailFiles and DetailBytes, the count and bytes of
	// what DiscardedFiles lists at the moment of the discard, to the audit
	// entry.
	DiscardReception(ctx context.Context, id, reason string, now time.Time, audit master.AuditEntry, guard Guard) (Reception, error)
	// ReceptionsWithoutWork returns a page of the receptions that have not
	// ended, are past receiving, and have no unit of work queued or leased
	// that names them, oldest first: what a crash between a step and the
	// queuing of the next unit left behind. The server runs it at start and
	// queues work for each, or fails it.
	ReceptionsWithoutWork(ctx context.Context, page master.Page) ([]Reception, string, error)
	// DiscardedFiles returns a page of the received files of a discarded
	// reception whose bytes the caller removes, in order of SOP Instance
	// UID, then checksum: every one whose place in the incoming area
	// nothing else needs. It never lists a place a quarantine record keeps
	// its bytes at, nor a place a received file of another reception
	// occupies while that reception has not ended, or has failed and its
	// files were not discarded (a retry would run them again): a release
	// gives a record's place to the reception it joins, and removing what
	// is listed, which a restart does again, must never delete bytes a
	// reception has yet to archive. The list is read when it is asked for,
	// so a place freed later is listed then. A reception not discarded is
	// a precondition failure.
	DiscardedFiles(ctx context.Context, receptionID string, page master.Page) ([]ReceivedFile, string, error)

	// AddReceivedFiles records files that have landed in the incoming area,
	// up to MaxReceivedBatch, all or none, and returns how many were new.
	// A file already recorded with the same reception, SOP Instance UID and
	// checksum is skipped, so a batch written again after a crash changes
	// nothing. Each new file adds to its reception's received counts and
	// moves its last file time to at when later; its association, the
	// first time the reception sees it, adds to the reception's
	// associations and calling AE titles. A reception not there is
	// NotFound, and one that is not receiving is a precondition failure
	// with ErrReceptionClosed in the chain (ReceptionClosed): its files
	// belong to the next arrival. A batch naming several receptions is
	// checked one reception at a time, in the order the batch first names
	// them, and the first failure met is the answer (a file of another
	// study is Invalid when its reception is reached).
	AddReceivedFiles(ctx context.Context, files []ReceivedFile, at time.Time) (int, error)
	// ReceivedFiles returns a page of a reception's received files in order
	// of SOP Instance UID, then checksum. A reception that ended done or
	// quarantined has none: the step that ends it removes them
	// (KeepsReceivedFiles), as it releases its claims.
	ReceivedFiles(ctx context.Context, receptionID string, page master.Page) ([]ReceivedFile, string, error)
	// CheckDuplicates says for each file, in order, up to
	// MaxDuplicateBatch, what the catalog holds or a reception claims under
	// its SOP Instance UID (JudgeRow): NotHeld, SameFile, DifferentFile,
	// UIDElsewhere or Claimed. It is a read, for the receiving step's early
	// answer (a re-send ignored at once); the claim is the authority.
	CheckDuplicates(ctx context.Context, files []DuplicateQuery) ([]Duplicate, error)

	// ClaimInstances claims, for a reception that has been identified and
	// not ended, the instances of up to MaxClaimBatch of its received files
	// before anything is written to a storage, in one transaction, and says
	// for each file, in the caller's order, what the claim decided
	// (DecideClaim; DuplicateKind says what the caller does with each). A
	// file that is not one of the reception's received files, with the
	// UIDs and checksum it was received with, is a precondition failure.
	// The UIDs are claimed in sorted order (of UID, then checksum), so two
	// files of one batch under one UID with two checksums keep the lower
	// checksum and answer DifferentFile for the other, whatever order they
	// were given in. A claim written again after a crash answers
	// ClaimedBefore, or ArchivedBefore for what the reception's commits
	// confirmed meanwhile, and changes nothing. fence, when not nil, is the
	// lease the claim is made under.
	ClaimInstances(ctx context.Context, receptionID string, files []InstanceClaim, now time.Time, fence *WorkFence) ([]Duplicate, error)
	// CommitScan writes one scan's catalog rows and confirms the
	// instances given (at least one), which the reception must claim, in
	// one transaction, for a reception identified and not ended. The
	// commit's study is the reception's, and each instance's study and
	// series are the commit's, as the claim recorded them; anything else is
	// a precondition failure. The storage must be the tenant's, and its row
	// is held to the commit's end (see Several writers).
	// the subject found or made by its label, the session by its Study
	// Instance UID, the scan by its Series Instance UID under that session,
	// each kept with the label it was first given. A label held at the
	// same level by another (or the same but for case, label.Fold) is a
	// LabelConflict; another subject for an existing session is one
	// (FaultTwoSubjectsForOneStudy); a series of another session holds
	// ErrSeriesElsewhere; labels or storage other than an existing
	// session's or scan's own are a precondition failure. An instance
	// confirmed again (a commit written again after a crash) is counted
	// once. Each confirmed instance adds to the scan's counts, to its
	// session's and subject's running totals (a new scan to the session's,
	// a new session to the subject's), to the reception's archived
	// counts, and to the catalog's totals, in the same transaction, which
	// moves the scan's, session's and subject's update time to the
	// commit's. A scan and a session made take the commit's Values and the
	// receiver of its reception; one already there fills only the values
	// it holds empty (ScanValues). A scan of more than MaxScanCommit
	// instances is committed in several calls.
	CommitScan(ctx context.Context, c ScanCommit) (Scan, error)
	// CatalogLabels returns the labels and storage of the study's session
	// and the labels of those of its series that have scans, as the
	// catalog holds them.
	CatalogLabels(ctx context.Context, studyUID string, seriesUIDs []string) (CatalogLabels, error)
	// CatalogTotals returns the catalog's running totals.
	CatalogTotals(ctx context.Context) (CatalogCounts, error)
	// ReceptionScans returns the scans a reception confirmed instances in,
	// in order of label, up to MaxReceptionScans, and whether there were
	// more.
	ReceptionScans(ctx context.Context, receptionID string) ([]ReceptionScan, bool, error)

	// HoldFiles records files in quarantine, held, up to
	// MaxQuarantineBatch, all or none, and returns how many were new. A
	// record whose id is there already is skipped as it stands, so a batch
	// written again after a crash changes nothing. Each new record adds to
	// the quarantine's totals and, when it names a reception, to that
	// reception's quarantined counts, and also to its received counts when
	// the reception has no received file of that UID and checksum (a file
	// that arrived and could not be recorded as one), in the same
	// transaction. A reception named that is not there is a precondition
	// failure.
	HoldFiles(ctx context.Context, files []QuarantinedFile) (int, error)
	// QuarantinedFile returns the record of that id, or NotFound.
	QuarantinedFile(ctx context.Context, id string) (QuarantinedFile, error)
	// ListQuarantined returns a page of the records the filter keeps,
	// newest first: by quarantine time, then by id, both descending.
	ListQuarantined(ctx context.Context, filter QuarantineFilter, page master.Page) ([]QuarantinedFile, string, error)
	// ReleaseQuarantined releases a held file as it is, through
	// the receiver named, or the one it first came to when receiver is
	// empty. A file with a valid study, series and instance UID and a
	// checksum joins that receiver's reception of its study that is
	// receiving, or a new one with receptionID opened at now, as a received
	// file counted released; the record names that reception. A different
	// file (QuarantineDifferentFile) is released only when the tenant no
	// longer holds an instance under its UID, which nothing in this version
	// causes: in this version such a file can only be rejected. Back into
	// the reception it was quarantined from, it is counted released there
	// and no longer quarantined, and not received twice. A file
	// already released is returned as it stands. A mode other than
	// ReleaseAsIs is Invalid. A different file whose UID the tenant still
	// holds, a rejected file, one whose bytes were never kept, a removed
	// receiver with none named, and a receiver named that does not exist,
	// are precondition failures.
	ReleaseQuarantined(ctx context.Context, id string, mode ReleaseMode, receiver, receptionID string, now time.Time, audit master.AuditEntry, guard Guard) (QuarantinedFile, error)
	// RejectQuarantined rejects a held file with the reason given: its
	// bytes are no longer kept. A file already rejected is returned as it
	// stands with its first reason; a released file is a precondition
	// failure. The store adds the details DetailReason, DetailSize and
	// DetailSHA256 to the audit entry.
	RejectQuarantined(ctx context.Context, id, reason string, now time.Time, audit master.AuditEntry, guard Guard) (QuarantinedFile, error)
	// QuarantineTotals returns the files held now and the bytes kept for
	// them, running totals kept in the transaction of every change, so
	// they equal a count over the records after every commit.
	QuarantineTotals(ctx context.Context) (files, bytes int64, err error)

	// EnqueueWork queues a unit of work, due at w.Due, and returns it with
	// created true; or, when w.UniqueKey is set and a unit with that key is
	// queued or leased, returns that one with created false and queues
	// nothing, so the same work is queued once. The key is looked at first:
	// a held unit is the answer whatever reception w names. A new unit
	// naming a reception the tenant does not have is a precondition
	// failure.
	EnqueueWork(ctx context.Context, w Work, now time.Time) (unit Work, created bool, err error)
	// Work returns the unit of that id, or NotFound.
	Work(ctx context.Context, id string) (Work, error)
	// ClaimWork leases the next unit of the kind that is due at now, the
	// earliest due first, for lease, to owner, and returns it with its new
	// attempt number. A unit whose lease ended without being renewed is
	// due again; one whose last attempt was the last allowed is held as
	// failed instead. No unit due is NotFound. Two claims at once get
	// different units.
	ClaimWork(ctx context.Context, kind, owner string, now time.Time, lease time.Duration) (Work, error)
	// RenewWork extends a lease to now + lease. A unit whose attempt is no
	// longer the fence's, or that is not leased, changes nothing and is
	// ErrLeaseLost.
	RenewWork(ctx context.Context, fence WorkFence, now time.Time, lease time.Duration) (Work, error)
	// CompleteWork marks a leased unit done and, when next is not nil,
	// queues next in the same transaction, so a chain of steps never loses
	// its next one. Fenced as RenewWork.
	CompleteWork(ctx context.Context, fence WorkFence, now time.Time, next *Work) error
	// FailWork records a failed attempt with its reason: the unit is due
	// again after RetryDelay(attempt), or, after its last allowed attempt,
	// held as failed, and then its reception, when it names one that has
	// not ended, takes the Fail step (FailureWorkGaveUp) in the same
	// transaction, releasing its claims. ClaimWork does the same for a unit
	// it holds as failed. Fenced as RenewWork. It returns the unit as
	// written.
	FailWork(ctx context.Context, fence WorkFence, reason string, now time.Time) (Work, error)
}

// StepHook runs inside a reception step's transaction (Pipeline.StepReception)
// with the reception as it was and as it is now; an error undoes the step.
type StepHook func(ctx context.Context, before, after Reception) error

// Precondition returns a refusal of the kind fault.Precondition with the
// message, which says which rule failed and is safe to show: the request is
// well formed, and the state of what it names forbids it.
func Precondition(message string) error {
	return fault.New(fault.Precondition, message)
}

// ReceptionClosed returns the refusal of received files whose reception is
// no longer receiving: a precondition, with ErrReceptionClosed in the chain
// for the receiver, which then opens the next arrival.
func ReceptionClosed() error {
	return fault.Wrap(fault.Precondition, ErrReceptionClosed,
		"the reception is no longer receiving; its files belong to the next arrival")
}

// StateChanged returns the refusal of a reception step whose reception is
// no longer in the state the step started from: a Conflict, with
// ErrStateChanged in the chain, since another step came first.
func StateChanged() error {
	return fault.Wrap(fault.Conflict, ErrStateChanged, "the reception is no longer in the state the step started from")
}

// ErrStateChanged is in the chain of a reception step whose reception is no
// longer in the state the step started from: another step came first.
var ErrStateChanged = errors.New("the reception is no longer in the state the step started from")

// ErrReceptionClosed is in the chain of received files refused because their
// reception is no longer receiving (ReceptionClosed): the one precondition a
// receiver acts on by itself, opening the next arrival.
var ErrReceptionClosed = errors.New("the reception is no longer receiving")

// ErrLeaseLost is in the chain of a worker's write refused because the
// worker's lease was taken over or the unit is no longer leased. Its kind is
// Conflict. Nothing was changed.
var ErrLeaseLost = errors.New("the lease on this unit of work is lost")

// LeaseLost returns the refusal of a fenced write whose fence no longer
// holds.
func LeaseLost() error {
	return fault.Wrap(fault.Conflict, ErrLeaseLost, "the lease on this unit of work was lost; nothing was changed")
}

// The details a store adds to the audit entry of a change whose outcome only
// the store knows.
const (
	// DetailRetryReception is the id of the reception that runs a failed
	// reception's files.
	DetailRetryReception = "retry_reception"
	// DetailFiles is a count of files.
	DetailFiles = "files"
	// DetailBytes is a count of bytes.
	DetailBytes = "bytes"
	// DetailReason is a quarantined file's reason code.
	DetailReason = "reason"
	// DetailSize is a quarantined file's size in bytes.
	DetailSize = "size"
	// DetailSHA256 is a quarantined file's checksum in lowercase
	// hexadecimal, empty when it was not kept whole.
	DetailSHA256 = "sha256"
	// DetailReleaseReception is the id of the reception a released file
	// joined.
	DetailReleaseReception = "release_reception"
)
