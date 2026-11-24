package tenant

import (
	"context"
	"slices"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
)

// Sending is what the tenant's database keeps for sending archived data
// out: its senders, its jobs, and the progress of each job's units of work.
// Each concern is a narrow interface; Sending is all of them, which
// PipelineParts embeds, so a driver implements them whole.
//
// # How a send job is carried out
//
// A job is one row with its state and running counts. Its work is units of
// the tenant's leased queue (Work), all of the kind SendKind(sender), so a
// worker claims the units of one sender and a sender that is down or
// disabled holds only its own jobs: first a listing unit, which lists the
// job's scans a page at a time (ListJobScans) and queues one unit per scan,
// so a subject of thousands of scans is never one transaction; then each
// scan's unit, which sends the scan's instances in the catalog's order
// (JobScanInstances) and records how far the peer answered (RecordJobProgress,
// EndJobScan). Every write of a unit is fenced by its attempt (WorkFence): a
// worker whose lease was taken over writes nothing and is answered
// ErrLeaseLost. A unit's cursor is the last instance the peer answered for,
// moved only forward and only from the cursor the write names, so a write
// sent again after its answer was lost changes nothing; after a crash or a
// takeover the next attempt goes on after the cursor, and at most the
// instances answered since the last write are sent again.
//
// What a job sends is fixed when it is made. A job of a subject, a session
// or a scan sends the instances its target's scans held at its create time:
// each instance whose held time (ScanInstance.Held) is at or before it,
// whatever is archived later. A forward sends exactly the instances one
// reception archived. A retry sends the ended job's refused instances and
// those it never sent, of the instances that job sent, and nothing archived
// since. The rule is a bound each read applies, never a copy of the catalog,
// so a job of millions of instances costs one row per scan. The held time
// is the caller's clock at the commit (see Pipeline's clocks), so a commit
// may take its held time before a job's creation and land after the job
// listed the scan: such an instance is in the scan's stream and is sent,
// and the scan's and the job's totals grow by it when its unit records it
// (ProgressExcess), so the job still ends done. A scan whose first commit
// lands after the listing passed it is not listed, and is sent by its own
// forward or a new job; on one server that takes a commit held across a
// whole creation and listing.
//
// A peer may leave an instance unaccounted (a stow answer that lists it
// neither kept nor failed). The unit's progress write moves the cursor past
// it all the same and keeps it owed to the unit (JobProgress.Owed), stored
// with the cursor, so the cursor's promise holds: after a crash at most the
// instances answered since the last write are sent again, and the owed ones
// are tried again by name (StartJobUnit answers them, JobScan.Owed). The
// worker tries an owed instance once more within the unit and records the
// answer (JobProgress.Settled), refusing it as FailurePeerUnaccounted when
// the peer leaves it unaccounted again. An instance still owed when its
// unit ends stays remaining, and a retry sends it.
//
// How a job ends early. CancelJob ends the job canceled and its queued
// units in its transaction; a unit a worker holds learns of it at its next
// write, which is still recorded, since the peer holds what it answered: a
// cancel takes effect within one batch. A listing unit whose attempts run
// out ends its job failed and, in the same transaction, its queued units
// and every scan of it, so the job's counts are final: a unit still held
// then records nothing more, and is answered with the job's state at its
// next write. A retry of that job may send again what such a unit sends
// before its next write, at most one batch per unit held when the listing
// gave up, as after a crash.
//
// # Rules every driver keeps
//
// Store's and Pipeline's rules hold. A change a person or a key asks for
// (a sender added, changed or removed, a job made, canceled or retried)
// takes its audit entry and a Guard; the work of units is the server's and
// takes neither. Events are written in the transaction of their change:
//
//   - CreateJob, RetryJob and a forward: the new job reaching queued;
//   - StartJobUnit: the job reaching running, at its first unit;
//   - CancelJob and RemoveSender with cancel: each job reaching canceled;
//   - EndJobScan, and a scan's unit whose attempts ran out (FailWork,
//     ClaimWork): the job reaching its end when it was its last unit
//     (EndState); a listing's unit whose attempts ran out: the job
//     reaching failed;
//   - RecordJobProgress and EndJobScan: the job's counts, at most one event
//     per job every ProgressEventInterval, and none when its counts did not
//     change since the last.
//
// A forward is made by StepReception itself: a step that reaches done makes,
// in its transaction, one job per sender its receiver's ForwardTo names, or
// its route's when the route chosen names its own (ReceptionRoute.Senders)
// (ForwardJobID, ForwardKey), so a step run again makes nothing twice and a
// caller cannot take the step without it.
//
// Under several writers (Postgres) a driver keeps these, which the suite
// tests: two workers on two units of one job both count, and neither's
// counts are lost; a cancel and a unit's write at once end with the job
// canceled and the unit's counts kept, the unit told at its next write; a
// sender removed while a forward is made either sees the job and refuses or
// cancels it, or the forward sees no sender and makes none (the forward
// holds the sender's row from removal to its commit).
type Sending interface {
	Senders
	Jobs
	JobWork
}

// Senders keeps a tenant's senders.
type Senders interface {
	// AddSender adds a sender, Normalize'd, with Created and Updated set to
	// now, and returns it as a read would. A name taken by a sender, or
	// still named by a job, is Conflict, so a name in a record always
	// means one sender. A stow sender's sealed credential is kept as given
	// and never answered. The store adds the details DetailSenderType and
	// DetailSenderDestination (Sender.Destination, never the credential)
	// to the audit entry.
	AddSender(ctx context.Context, s Sender, now time.Time, audit master.AuditEntry, guard Guard) (Sender, error)
	// Sender returns the sender of that name, or NotFound; its credential
	// is never answered (STOWSender.CredentialSet says whether one is
	// held).
	Sender(ctx context.Context, name string) (Sender, error)
	// ListSenders returns a page of the senders in order of name.
	ListSenders(ctx context.Context, page master.Page) ([]Sender, string, error)
	// SetSender changes the fields named to their values in s, whose Name
	// names the sender, by ApplySender's rules; Updated becomes now. A
	// credential not named is kept; one named replaces it; an
	// authentication that becomes none removes it.
	SetSender(ctx context.Context, s Sender, fields []SenderField, now time.Time, audit master.AuditEntry, guard Guard) (Sender, error)
	// RemoveSender removes the sender and its credential and returns it as
	// it was, with how many of its jobs it canceled. A receiver that
	// forwards to it, itself or by a route, is a precondition failure; so is a job to it that is
	// queued or running, unless cancelJobs, which cancels them in the same
	// transaction as CancelJob does (with JobReasonSenderRemoved). Its
	// jobs stay, naming it. The store adds the details DetailSenderType
	// and DetailJobsCanceled to the audit entry.
	RemoveSender(ctx context.Context, name string, cancelJobs bool, now time.Time, audit master.AuditEntry, guard Guard) (Sender, int64, error)
	// SenderCredential returns the sealed credential of the stow sender of
	// that name, as AddSender or SetSender was given it, still sealed: the
	// store neither opens nor logs it. A sender not there, or holding
	// none, is NotFound.
	SenderCredential(ctx context.Context, name string) ([]byte, error)
}

// Jobs keeps a tenant's jobs.
type Jobs interface {
	// CreateJob makes a send job, queued, and its listing unit, and
	// returns it. A sender the tenant does not have is a precondition
	// failure; a target the catalog does not hold is NotFound. The key
	// that makes it is its audit entry's.
	CreateJob(ctx context.Context, j NewJob, audit master.AuditEntry, guard Guard) (Job, error)
	// Job returns the job of that id with its counts now, or NotFound.
	Job(ctx context.Context, id string) (Job, error)
	// ListJobs returns a page of the jobs the filter keeps, newest first:
	// by create time, then by id, both descending.
	ListJobs(ctx context.Context, filter JobFilter, page master.Page) ([]Job, string, error)
	// CancelJob ends a queued or running job as canceled, and ends in the
	// same transaction its units that are queued; a unit a worker holds
	// is told at its next write, whose counts are kept, so the cancel
	// takes effect within one batch. A job canceled already is answered as
	// it stands; one ended otherwise is a precondition failure.
	CancelJob(ctx context.Context, id string, now time.Time, audit master.AuditEntry, guard Guard) (Job, error)
	// RetryJob makes a new job, newID, of the same kind, sender and target
	// as the ended job id, with RetryOf set, that sends exactly that job's
	// refused instances and those it never sent, and sets the ended job's
	// RetryJob. A job retried already is answered as it stands with the
	// one made then. A job not ended, one ended done with nothing to send
	// again, and one whose sender was removed, are precondition failures.
	// A scan whose refused instances were not all listed (past
	// job.failures-kept) is sent again whole, since the store no longer
	// knows which of its instances were refused. The store adds the
	// detail DetailRetryJob to the audit entry.
	RetryJob(ctx context.Context, id, newID string, now time.Time, audit master.AuditEntry, guard Guard) (ended, retry Job, err error)
	// ListJobFailures returns a page of a job's failures, those refused
	// and, with includeWarnings, those sent with a warning, in byte order
	// of subject label, session label, scan label, then SOP Instance UID.
	// A job not there is NotFound.
	ListJobFailures(ctx context.Context, id string, includeWarnings bool, page master.Page) ([]JobFailure, string, error)
	// JobTallies returns a deid job's tallies (JobProgress.Tallies), in
	// SortTallies' order: at most MaxJobTallies and TallyOther. A send
	// job has none; a job not there is NotFound.
	JobTallies(ctx context.Context, id string) ([]JobTally, error)
}

// JobWork is the work of a job's units, each write fenced by the unit's
// lease (JobScanInstances is a read, not fenced: what it reads changes
// nothing, and a worker whose lease was taken learns of it at its next
// write). A unit is one the store queued for a job (a listing or a scan);
// any other is a precondition failure.
type JobWork interface {
	// StartJobUnit says what the leased unit is and how far it got, and
	// moves its job from queued to running (its event) at its first unit.
	// A unit whose job has ended (canceled) is answered with the job's
	// state: the worker ends it at once with EndJobScan, or, for a
	// listing, CompleteWork.
	StartJobUnit(ctx context.Context, fence WorkFence, now time.Time) (JobUnit, error)
	// ListJobScans lists the next page of a listing unit's job's scans,
	// up to JobListPage, queues a unit for each that holds an instance to
	// send, and adds them to the job's counts; more says whether to call
	// it again. The page that lists the last scan marks the job listed
	// and completes the listing unit in its transaction, and ends a job
	// whose every unit has ended (EndState). A job canceled meanwhile
	// lists nothing more and completes the unit.
	ListJobScans(ctx context.Context, fence WorkFence, now time.Time) (job Job, more bool, err error)
	// JobScanInstances returns up to limit (1 to MaxJobInstancePage) of
	// the instances of a scan unit's stream after the SOP Instance UID
	// after, in its order, each placed as ScanInstances places them. It
	// reads; it is not fenced.
	JobScanInstances(ctx context.Context, fence WorkFence, after string, limit int) ([]ScanInstance, error)
	// DeletedJobInstances returns up to limit (1 to MaxJobInstancePage) of
	// the instances of a scan unit's stream that a committed delete hid,
	// after the SOP Instance UID after, in its order: those the stream
	// showed while they were held, by the same bound (its job's create
	// time, its forward's reception, its retry's resends), which the job
	// counted when it listed the scan. A unit merges them into its stream
	// in their places and refuses each with FailureDeletedInArchive, so
	// its counts add up to what its job counted and its cursor never
	// passes an instance it has not answered. A scan the delete hid whole
	// is still its unit's: StartJobUnit and JobScanInstances answer it
	// (its labels as they were, its stream empty), and this lists its
	// instances. Only SOPInstanceUID, ReceptionID, Size and Held are set;
	// an instance hidden twice under one scan is listed once. A delete
	// undone shows its instances again in the stream and lists none here;
	// one purged lists none either, and its instances not yet answered
	// stay remaining. It reads; it is not fenced.
	DeletedJobInstances(ctx context.Context, fence WorkFence, after string, limit int) ([]ScanInstance, error)
	// RecordJobProgress records what a scan unit's peer answered since
	// the unit's cursor and returns the job as it is after: the counts
	// added to the scan's and the job's (their totals grown by
	// ProgressExcess), the failures listed while the job lists fewer than
	// p.FailuresKept and counted past it (an instance's failure listed
	// twice is a bug of the caller, and fails the write), p.Owed kept owed
	// and p.Settled no longer owed, the cursor moved to p.To. A write
	// whose p.From is not the unit's cursor changes nothing: when the
	// cursor is already p.To it was recorded before and is answered as it
	// stands, and otherwise it is a precondition failure; so is a write
	// that settles an instance not owed, but for one that moves no cursor
	// and whose every settled instance is settled already, which was
	// recorded before. A job canceled is answered with its state, after
	// recording, so the worker stops; a job that ended otherwise records
	// nothing and is answered with its state (JobState.TakesProgress).
	RecordJobProgress(ctx context.Context, p JobProgress) (Job, error)
	// EndJobScan records p as RecordJobProgress does, ends the scan,
	// completes its unit, and ends the job when it was its last unit and
	// the job is listed (EndState). Its instances not answered, and those
	// still owed, stay remaining.
	EndJobScan(ctx context.Context, p JobProgress) (Job, error)
	// ReleaseJobUnit gives the leased unit back, due again at due (not
	// before now), with reason as its last error, without spending an
	// attempt: for a cause that is not the unit's own (its sender down or
	// disabled, a read slot refused), so a peer down for a day fails no
	// job. Its lease ends, and a later write of that lease is
	// ErrLeaseLost; its cursor and counts are kept. FailWork is the
	// release that spends one. A unit whose job has ended (canceled while
	// it sent) is not queued again: it is completed and its scan ended,
	// counted in the job's scans done, in the same transaction.
	ReleaseJobUnit(ctx context.Context, fence WorkFence, due time.Time, reason string, now time.Time) error
	// SeenUIDs looks up UIDs among the catalog's Study Instance UIDs of
	// sessions, Series Instance UIDs of scans and SOP Instance UIDs of
	// instances, at most MaxSeenUIDs of each list, each answered once: which
	// of keyed it holds at all, and which of plain it holds for an instance
	// archived as received (one that records no profile version) and for
	// an instance archived de-identified (UIDsSeen). A deid job's check
	// passes in keyed the tenant's keyed remapping of an archived file's
	// UIDs: the catalog holds a de-identified file's UIDs remapped, so one
	// found is a UID some received file carried, unless the UID is itself
	// one the remapping made (plain, found de-identified), which a file
	// sent back in carries. It reads; it is not fenced.
	SeenUIDs(ctx context.Context, keyed, plain []string) (UIDsSeen, error)
}

// JobUnit is what a leased unit of a job is.
type JobUnit struct {
	// Job is its job, as it is after the unit started.
	Job Job
	// Listing says the unit lists the job's scans; otherwise it sends
	// Scan.
	Listing bool
	// Scan is the scan a scan unit sends, and how far it got.
	Scan JobScan
}

// JobScan is one scan of a job and how far its unit got.
type JobScan struct {
	// Path is the scan's labels; Storage its storage.
	Path    label.Path
	Storage string
	// Cursor is the SOP Instance UID of the last instance the peer
	// answered for, empty before the first.
	Cursor string
	// Counts are the scan's instances and bytes so far; Scans and
	// ScansDone are 1 and 0 or 1.
	Counts JobCounts
	// Ended says its unit ended.
	Ended bool
	// Owed are the instances of the scan the peer left unaccounted, owed
	// to the unit: at or before the cursor, to be tried again by name, at
	// most MaxJobOwed, in order, each placed as ScanInstances places them.
	// StartJobUnit answers them.
	Owed []ScanInstance
}

// JobProgress is what a scan unit's peer answered since its cursor.
type JobProgress struct {
	// Fence is the unit's lease.
	Fence WorkFence
	// From is the unit's cursor the instances counted follow; To the last
	// of them, the new cursor. Equal, with nothing counted, for no
	// progress (an EndJobScan of a unit that sends nothing more).
	From, To string
	// Sent, Warned and Refused count the instances answered, each with
	// its bytes.
	Sent, SentBytes       int64
	Warned, WarnedBytes   int64
	Refused, RefusedBytes int64
	// Failures are the instances refused and those sent with a warning,
	// one each: as many refused as Refused and as many warned as Warned.
	// Each is between the cursors or settled.
	Failures []JobFailure
	// Owed are the instances between the cursors the peer left
	// unaccounted: counted in none of Sent, Warned and Refused, moved past
	// by the cursor all the same, and kept owed to the unit until settled.
	Owed []string
	// Settled are owed instances of the unit, at or before From, whose
	// second try this write records: each is counted once in Sent, Warned
	// or Refused, and is no longer owed. A write that only settles names
	// From equal to To.
	Settled []string
	// FailuresKept is the setting job.failures-kept, the most failures
	// the job lists.
	FailuresKept int
	// Tallies are what a deid job's unit found among the instances the
	// write counts, added to the job's (JobTally), at most
	// MaxJobTallyBatch keys; empty for a send job. They are added only
	// when the write moves the cursor or settles, so a write sent again
	// counts nothing twice.
	Tallies map[string]int64
	// Now is when they were recorded.
	Now time.Time
}

// Validate returns an Invalid error unless p can be recorded.
func (p JobProgress) Validate() error {
	if err := p.Fence.Validate(); err != nil {
		return err
	}
	for _, u := range []string{p.From, p.To} {
		if u != "" {
			if err := ValidateInstanceUID(u); err != nil {
				return err
			}
		}
	}
	answered := p.Sent + p.Warned + p.Refused
	for _, n := range []int64{p.Sent, p.SentBytes, p.Warned, p.WarnedBytes, p.Refused, p.RefusedBytes} {
		if n < 0 {
			return invalid("a job's progress counts are not negative")
		}
	}
	if answered > MaxJobProgressBatch || len(p.Failures) > MaxJobFailureBatch || len(p.Owed) > MaxJobOwed ||
		len(p.Settled) > MaxJobOwed {
		return invalid("a progress write records at most 10000 instances, 1000 failures, and 1000 owed and 1000 settled")
	}
	seen := make(map[string]bool, len(p.Failures)+len(p.Owed)+len(p.Settled))
	once := func(u string) error {
		if err := ValidateInstanceUID(u); err != nil {
			return err
		}
		if seen[u] {
			return invalid("a progress write names an instance once among its owed and settled")
		}
		seen[u] = true
		return nil
	}
	settled := make(map[string]bool, len(p.Settled))
	for _, u := range p.Settled {
		if err := once(u); err != nil {
			return err
		}
		if u > p.From {
			return invalid("a settled instance is an owed one, at or before the cursor the write follows")
		}
		settled[u] = true
	}
	for _, u := range p.Owed {
		if err := once(u); err != nil {
			return err
		}
		if u <= p.From || u > p.To {
			return invalid("an owed instance is one the progress write moves the cursor past")
		}
	}
	inRange := answered - int64(len(p.Settled))
	if inRange < 0 {
		return invalid("a progress write counts each instance it settles")
	}
	if (inRange+int64(len(p.Owed)) == 0) != (p.From == p.To) || (p.From != p.To && p.To <= p.From) {
		return invalid("a progress write moves the cursor forward past the instances it counts, and only then")
	}
	var refused, warned int64
	failed := make(map[string]bool, len(p.Failures))
	for _, f := range p.Failures {
		if err := f.Validate(); err != nil {
			return err
		}
		if failed[f.SOPInstanceUID] {
			return invalid("a progress write lists an instance's failure once")
		}
		failed[f.SOPInstanceUID] = true
		if !settled[f.SOPInstanceUID] && (f.SOPInstanceUID <= p.From || f.SOPInstanceUID > p.To) {
			return invalid("a failure is of an instance the progress write moves the cursor past, or settles")
		}
		if slices.Contains(p.Owed, f.SOPInstanceUID) {
			return invalid("an owed instance is neither refused nor sent with a warning")
		}
		if f.Code.Refused() {
			refused++
		} else {
			warned++
		}
	}
	if refused != p.Refused || warned != p.Warned {
		return invalid("a progress write lists each instance refused or sent with a warning, once")
	}
	if p.FailuresKept < MinFailuresKept || p.FailuresKept > MaxFailuresKept {
		return invalid("a job keeps 100 to 1000000 failures")
	}
	if err := validateTallies(p.Tallies); err != nil {
		return err
	}
	if len(p.Tallies) > 0 && p.From == p.To && len(p.Settled) == 0 {
		return invalid("a progress write that counts no instance adds to no tally")
	}
	return master.ValidateTime("progress time", p.Now)
}

// The details a store adds to the audit entry of a change of senders or
// jobs.
const (
	// DetailJobsCanceled is the number of jobs a sender's removal
	// canceled.
	DetailJobsCanceled = "jobs_canceled"
	// DetailRetryJob is the id of the job that sends an ended job's unsent
	// instances.
	DetailRetryJob = "retry_job"
	// DetailSenderType is a sender's type, dimse or stow.
	DetailSenderType = "type"
	// DetailSenderDestination is where a sender sends
	// (Sender.Destination).
	DetailSenderDestination = "destination"
)
