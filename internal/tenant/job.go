package tenant

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
)

// JobKind is what a job does, the contract's oneof member of Job's kind.
// Its text is what a store keeps.
type JobKind string

// The kinds.
const (
	// JobSend is archived instances sent to a sender.
	JobSend JobKind = "send"
	// JobDeid is archived instances judged against a de-identification
	// profile version, changing nothing (job_deid.go).
	JobDeid JobKind = "deid"
)

// JobState is where a job is, the contract's JobState in lowercase without
// its prefix. A job goes forward only; done, partial, failed and canceled
// are ends nothing leaves.
type JobState string

// The job states, in the contract's order.
const (
	JobQueued   JobState = "queued"
	JobRunning  JobState = "running"
	JobDone     JobState = "done"
	JobPartial  JobState = "partial"
	JobFailed   JobState = "failed"
	JobCanceled JobState = "canceled"
)

// JobStates returns every job state, in the contract's order.
func JobStates() []JobState {
	return []JobState{JobQueued, JobRunning, JobDone, JobPartial, JobFailed, JobCanceled}
}

// Valid reports whether s is one of JobStates.
func (s JobState) Valid() bool { return slices.Contains(JobStates(), s) }

// Ended reports whether a job in s has ended.
func (s JobState) Ended() bool {
	return s == JobDone || s == JobPartial || s == JobFailed || s == JobCanceled
}

// TakesProgress reports whether a unit's progress write into a job in s is
// recorded: while the job runs, and after a cancel, which keeps what a unit
// in flight answered when the cancel landed (the peer holds those
// instances). A job that ended any other way has final counts: a unit
// still held when its listing gave up (failed) writes nothing more.
func (s JobState) TakesProgress() bool { return !s.Ended() || s == JobCanceled }

// The reasons a store writes on a job, from the contract's fixed set of
// phrases (Job.reason); the server adds those of a job held back (its
// sender down or disabled) when it answers, from its own view of the
// sender.
const (
	JobReasonAttemptsRanOut = "attempts ran out"
	JobReasonCanceled       = "canceled"
	JobReasonSenderRemoved  = "canceled: its sender was removed"
)

// The last errors a store gives a job's queued unit that it ends because
// its job ended.
const (
	WorkReasonJobCanceled = "its job was canceled"
	WorkReasonJobFailed   = "its job failed"
)

// The bounds of jobs.
const (
	// JobUnitAttempts is the attempts a unit of a send job has, about a
	// day at the longest wait between them (RetryDelay).
	JobUnitAttempts = 30
	// MinFailuresKept and MaxFailuresKept bound the setting
	// job.failures-kept; DefaultFailuresKept is its built-in value.
	MinFailuresKept     = 100
	MaxFailuresKept     = 1000000
	DefaultFailuresKept = 10000
	// MaxJobStates is the most states a list's filter names.
	MaxJobStates = 16
	// MaxJobFailureBatch is the most failures one progress write records.
	MaxJobFailureBatch = 1000
	// MaxJobProgressBatch is the most instances one progress write moves
	// a unit's cursor past.
	MaxJobProgressBatch = 10000
	// ProgressEventInterval is the least time between two progress events
	// of one job.
	ProgressEventInterval = 10 * time.Second
	// JobListPage is how many scans one ListJobScans call lists.
	JobListPage = 64
	// MaxJobInstancePage is the most instances one JobScanInstances call
	// answers.
	MaxJobInstancePage = 1000
	// MaxJobOwed is the most instances a scan's unit holds owed at once
	// (JobProgress.Owed), and the most one progress write names as owed or
	// settled.
	MaxJobOwed = 1000
)

// JobTarget is what a send job sends: a subject, a session of it, or a scan
// of that, by labels.
type JobTarget struct {
	SubjectLabel, SessionLabel, ScanLabel string
}

// Whole reports whether t names no label: the whole tenant, which only a
// deid job judges.
func (t JobTarget) Whole() bool { return t == JobTarget{} }

// Validate returns an Invalid error unless t names a subject, a session of
// it or a scan of that, each by a valid label.
func (t JobTarget) Validate() error {
	if err := ValidateLabel("subject", t.SubjectLabel); err != nil {
		return err
	}
	if t.SessionLabel == "" && t.ScanLabel != "" {
		return invalid("a job's scan is named with its session")
	}
	if t.SessionLabel != "" {
		if err := ValidateLabel("session", t.SessionLabel); err != nil {
			return err
		}
	}
	if t.ScanLabel != "" {
		return ValidateLabel("scan", t.ScanLabel)
	}
	return nil
}

// Path returns the target as a path of labels, those not named empty.
func (t JobTarget) Path() label.Path {
	return label.Path{Subject: t.SubjectLabel, Session: t.SessionLabel, Scan: t.ScanLabel}
}

// JobCounts are a job's instances and bytes so far, the contract's
// JobCounts: an instance counts in exactly one of sent, warned, refused and
// remaining, so the remaining are what the others leave of the total.
type JobCounts struct {
	Instances, Bytes               int64
	InstancesSent, BytesSent       int64
	InstancesWarned, BytesWarned   int64
	InstancesRefused, BytesRefused int64
	// Scans counts the scans listed, one unit each; ScansDone those whose
	// unit ended.
	Scans, ScansDone int64
	// Listed says every scan of the target is listed, so Scans is final;
	// Instances and Bytes are too, but for an instance held before the
	// job's bound whose commit landed after its scan was listed, which is
	// added when its unit records it (Sending).
	Listed bool
}

// NothingToRetry reports whether a job, or one scan of it, with counts c
// left nothing to send again: everything listed, nothing refused, nothing
// remaining. A retry of it is refused, and a retry of its job skips it.
func (c JobCounts) NothingToRetry() bool {
	return c.Listed && c.InstancesRefused == 0 && c.InstancesRemaining() == 0
}

// RetryStart is where a retry's stream of an ended job's scan starts, the
// SOP Instance UID its instances follow: after the ended scan's cursor,
// what that scan's unit never reached, with the refused and owed
// instances, which lie at or before the cursor, sent again by name. When
// the scan's refused instances were not all listed (past
// job.failures-kept) the store no longer knows which they were, and the
// retry sends the scan's whole stream again from its own start.
func RetryStart(startAfter, cursor string, refusedNotListed int64) string {
	if refusedNotListed > 0 {
		return startAfter
	}
	return max(startAfter, cursor)
}

// ProgressExcess returns how many instances and bytes a progress write
// counts past what its scan's counts c hold. Each instance a unit's
// stream answers is the job's by the job's definition (held at or before
// its bound), but a commit that took its held time before the bound and
// landed after the scan was listed is in the stream without being in the
// count, so the store grows the scan's and the job's totals by the excess
// in the same write rather than refuse it, which would stop the unit for
// good.
func ProgressExcess(c JobCounts, p JobProgress) (instances, bytes int64) {
	instances = c.InstancesSent + c.InstancesWarned + c.InstancesRefused + p.Sent + p.Warned + p.Refused - c.Instances
	bytes = c.BytesSent + c.BytesWarned + c.BytesRefused + p.SentBytes + p.WarnedBytes + p.RefusedBytes - c.Bytes
	return max(instances, 0), max(bytes, 0)
}

// ProgressEventDue reports whether a progress write that leaves job j with
// its counts should write a progress event: the job runs, its instances
// answered (mark) moved since the last event's, and the last event, at
// last (zero for none), is at least ProgressEventInterval before now.
func ProgressEventDue(j Job, lastMark int64, last, now time.Time) bool {
	return j.State == JobRunning && ProgressMark(j.Counts) != lastMark && (last.IsZero() || !now.Before(last.Add(ProgressEventInterval)))
}

// ProgressMark is the instances of a job answered so far, what a progress
// event records it was written at.
func ProgressMark(c JobCounts) int64 { return c.InstancesSent + c.InstancesWarned + c.InstancesRefused }

// InstancesRemaining returns the instances not sent yet; in an ended job,
// those it never sent.
func (c JobCounts) InstancesRemaining() int64 {
	return c.Instances - c.InstancesSent - c.InstancesWarned - c.InstancesRefused
}

// BytesRemaining returns the bytes of the instances not sent.
func (c JobCounts) BytesRemaining() int64 {
	return c.Bytes - c.BytesSent - c.BytesWarned - c.BytesRefused
}

// Job is one piece of work in a tenant and how far it got.
type Job struct {
	// ID names it, given by the caller (master.ValidateID).
	ID string
	// Kind is what it does: JobSend, JobDeid or JobImport.
	Kind JobKind
	// Sender names the sender a send job sends to; kept when the sender
	// is removed. Empty for a deid job.
	Sender string
	// Mode is what a deid job does; empty for a send job.
	Mode DeidMode
	// Profile is the version a deid job's dry run runs; zero for a check,
	// and for a dry run that runs each instance's own version.
	Profile ProfileRef
	// Target is what it sends, or judges; Whole for a deid job of the
	// whole tenant.
	Target JobTarget
	// ReceptionID and Receiver name the reception whose archived
	// instances a forward sends, and its receiver; empty for a job a
	// person made. An import job's Receiver is its import receiver.
	ReceptionID, Receiver string
	// Import is what an import job reads and what became of its files;
	// nil for every other kind. Its Counts give the job's (JobCounts).
	Import *ImportJob
	// Storage is the storage a scrub job reads, empty for every storage of
	// the tenant; empty for every other kind.
	Storage string
	// Scheduled says the server made a scrub job by itself, every
	// scrub.interval, rather than a person.
	Scheduled bool
	// Rebuild is what a rebuild job does; nil for every other kind.
	Rebuild *RebuildJob
	// Repair is what a repair job mends; nil for every other kind. Its
	// Counts read: sent, the objects repaired or found good; refused, those
	// no replica holds good; Instances and Bytes, both together.
	Repair *RepairJob
	// Replicate is what a replicate job copies; nil for every other kind.
	// Its Counts read: Scans, the scans its backfill marked; ScansDone,
	// those the replica no longer owes (BackfillCounts).
	Replicate *ReplicateJob
	// State is where it is.
	State JobState
	// Counts are its instances and bytes so far.
	Counts JobCounts
	// Reason is why it ended as it did, a phrase of the store's
	// (JobReasonAttemptsRanOut and the others); empty while it runs and
	// for a job done.
	Reason string
	// CreatedByKeyID is the key that made it, its audit entry's
	// (master.AuditEntry.KeyID); empty for the local operator and for a
	// forward.
	CreatedByKeyID string
	// Created is when it was made; Started when its first work started,
	// zero until then; StateTime when it reached its state; Ended when it
	// ended, zero while it runs.
	Created, Started, StateTime, Ended time.Time
	// RetryOf is the ended job whose unsent instances it sends; RetryJob
	// the job that sends this one's. Empty when none.
	RetryOf, RetryJob string
	// FailuresNotListed counts the failures counted and not listed, past
	// the setting job.failures-kept.
	FailuresNotListed int64
}

// NewJob is a job a person asks for (Jobs.CreateJob).
type NewJob struct {
	// ID is its id, given by the caller.
	ID string
	// Kind is what it does; empty is JobSend.
	Kind JobKind
	// Sender names the sender a send job sends to; empty for a deid job.
	Sender string
	// Mode is what a deid job does; empty for a send job.
	Mode DeidMode
	// Profile is the version a deid job's dry run runs, which the tenant
	// must have; zero for a check, and for a dry run of each instance's
	// own version.
	Profile ProfileRef
	// Target is what it sends, or judges; a deid job may name the whole
	// tenant (JobTarget.Whole), and a scrub job names it always.
	Target JobTarget
	// Storage is the storage a scrub job reads, which the tenant must
	// have; empty for every storage, and for every other kind.
	Storage string
	// Scheduled says the server makes a scrub job by itself; false for
	// every other kind.
	Scheduled bool
	// Rebuild is what a rebuild job does: its mode and scratch schema; nil
	// for every other kind.
	Rebuild *RebuildJob
	// Repair is what a repair job mends; nil for every other kind.
	Repair *RepairJob
	// Created is when it is made, and the moment whose catalog it sends.
	Created time.Time
}

// Validate returns an Invalid error unless j can be made.
func (j NewJob) Validate() error {
	if err := master.ValidateID("job id", j.ID); err != nil {
		return err
	}
	if j.Kind != JobScrub && j.Kind != JobRebuild && (j.Storage != "" || j.Scheduled) {
		return invalid("only a scrub job names a storage or is scheduled")
	}
	if j.Kind != JobRebuild && j.Rebuild != nil {
		return invalid("only a rebuild job names a rebuild mode")
	}
	if j.Kind != JobRepair && j.Repair != nil {
		return invalid("only a repair job names what it mends")
	}
	switch j.Kind {
	case JobRepair:
		if err := validateRepairJob(j); err != nil {
			return err
		}
	case JobRebuild:
		if err := validateRebuildJob(j); err != nil {
			return err
		}
	case JobScrub:
		if err := validateScrubJob(j); err != nil {
			return err
		}
	case JobSend, "":
		if j.Mode != "" || !j.Profile.IsZero() {
			return invalid("a send job has no mode and no profile")
		}
		if err := ValidateSenderName(j.Sender); err != nil {
			return err
		}
		if err := j.Target.Validate(); err != nil {
			return err
		}
	case JobDeid:
		if err := validateDeidJob(j.Sender, j.Mode, j.Profile); err != nil {
			return err
		}
		if !j.Target.Whole() {
			if err := j.Target.Validate(); err != nil {
				return err
			}
		}
	default:
		return invalid("a job's kind is send, deid, scrub, rebuild or repair")
	}
	return master.ValidateTime("job time", j.Created)
}

// KindOrSend returns the job's kind, JobSend when it names none.
func (j NewJob) KindOrSend() JobKind {
	if j.Kind == "" {
		return JobSend
	}
	return j.Kind
}

// JobFilter keeps the jobs a list answers; every filter given must hold.
type JobFilter struct {
	// States keeps the jobs in one of these states, at most MaxJobStates.
	States []JobState
	// InProgress keeps the jobs that have not ended.
	InProgress bool
	// Sender keeps the send jobs to the sender of this name.
	Sender string
	// Since keeps the jobs made at or after it; Until those made before
	// it.
	Since, Until time.Time
}

// Validate returns an Invalid error when f cannot be applied.
func (f JobFilter) Validate() error {
	if len(f.States) > MaxJobStates {
		return invalid("a filter names at most " + strconv.Itoa(MaxJobStates) + " job states")
	}
	for _, s := range f.States {
		if !s.Valid() {
			return invalid("a job state is one of queued, running, done, partial, failed, canceled")
		}
	}
	if f.Sender != "" {
		if err := ValidateSenderName(f.Sender); err != nil {
			return err
		}
	}
	return validateRange(f.Since, f.Until)
}

// NarrowStates returns the states a list reads: those the filter names,
// each once, in the contract's order, without the ended ones when only
// jobs in progress are kept; none says nothing can match.
func (f JobFilter) NarrowStates() (states []JobState, none bool) {
	for _, s := range JobStates() {
		if slices.Contains(f.States, s) && (!f.InProgress || !s.Ended()) {
			states = append(states, s)
		}
	}
	return states, len(f.States) > 0 && len(states) == 0
}

// JobFailureCode is why an instance of a job was refused, or that it was
// sent with a warning, the contract's JobFailureCode in lowercase without
// its prefix.
type JobFailureCode string

// The codes.
const (
	FailurePeerRefused       JobFailureCode = "peer_refused"
	FailurePeerWarning       JobFailureCode = "peer_warning"
	FailureClassNotAccepted  JobFailureCode = "class_not_accepted"
	FailureSyntaxNotAccepted JobFailureCode = "syntax_not_accepted"
	FailurePeerUnaccounted   JobFailureCode = "peer_unaccounted"
	FailureDamagedInStorage  JobFailureCode = "damaged_in_storage"
	FailureMetaUnreadable    JobFailureCode = "meta_unreadable"
	// The codes of a deid job (job_deid.go).
	FailureWouldHold       JobFailureCode = "would_hold"
	FailureCheckFailed     JobFailureCode = "check_failed"
	FailureUndecided       JobFailureCode = "undecided"
	FailureNotDeidentified JobFailureCode = "not_deidentified"
	FailureTooLarge        JobFailureCode = "too_large"
	// FailureDeletedInArchive is an instance a correction deleted after
	// the job was made: a job never sends or reads data a person asked to
	// delete, so the unit counts it refused and goes on.
	FailureDeletedInArchive JobFailureCode = "deleted_in_archive"
	// FailureNotOnline is an instance the storage answered as held in a
	// tier it cannot read at once: tried again later, then listed, never
	// reported as data loss.
	FailureNotOnline JobFailureCode = "not_online"
)

// jobFailureCodes is the closed list of codes.
var jobFailureCodes = []JobFailureCode{FailurePeerRefused, FailurePeerWarning, FailureClassNotAccepted,
	FailureSyntaxNotAccepted, FailurePeerUnaccounted, FailureDamagedInStorage, FailureMetaUnreadable,
	FailureWouldHold, FailureCheckFailed, FailureUndecided, FailureNotDeidentified, FailureTooLarge,
	FailureDeletedInArchive, FailureNotOnline, FailureAsReceived, FailureNoProfileProvenance, FailurePixelRiskAccepted}

// Refused reports whether an instance with code c was refused, not sent
// with a warning (for a deid job: did not pass, rather than passed with a
// note; a file too large to read was not judged, so it is a note).
func (c JobFailureCode) Refused() bool {
	return c != FailurePeerWarning && c != FailureUndecided && c != FailureNotDeidentified && c != FailureTooLarge
}

// JobFailure is one instance of a job that was refused, or sent with a
// warning.
type JobFailure struct {
	// SOPInstanceUID names the instance, as the tenant holds it.
	SOPInstanceUID string
	// Path is its scan's labels. Answered by a store; ignored in a write.
	Path label.Path
	// Code is why it was refused, or that it was sent with a warning.
	Code JobFailureCode
	// PeerStatus is the peer's code for it, or 0 for none.
	PeerStatus int32
	// SOPClassUID and TransferSyntaxUID are its class and the syntax it is
	// stored and was offered in.
	SOPClassUID, TransferSyntaxUID string
	// Detail is a phrase of the server's, at most MaxFailureLength bytes.
	Detail string
	// Time is when it was refused, or sent.
	Time time.Time
}

// FailureDetailWholeRequest is the detail of an instance a stow peer
// refused with the whole request it was in, answering an HTTP status and
// no account of each instance (PS3.18 10.5.3): its PeerStatus is that HTTP
// status, where every other failure's is a DICOM status. The phrase is the
// one place the two are told apart, so a failure keeps no other field for it.
const FailureDetailWholeRequest = "the peer refused the whole request"

// HTTPStatus reports whether f's PeerStatus is an HTTP status (RFC 9110 15)
// rather than a DICOM status (PS3.7 Annex C).
func (f JobFailure) HTTPStatus() bool {
	return f.Code == FailurePeerRefused && f.Detail == FailureDetailWholeRequest && f.PeerStatus != 0
}

// Validate returns an Invalid error unless f can be recorded.
func (f JobFailure) Validate() error {
	if err := ValidateInstanceUID(f.SOPInstanceUID); err != nil {
		return err
	}
	if !slices.Contains(jobFailureCodes, f.Code) {
		return invalid("a job failure's code is one of peer_refused, peer_warning, class_not_accepted, syntax_not_accepted, peer_unaccounted, damaged_in_storage, meta_unreadable, would_hold, check_failed, undecided, not_deidentified, too_large, deleted_in_archive, not_online, as_received, no_profile_provenance, pixel_risk_accepted")
	}
	if f.SOPClassUID != "" {
		if err := validateUID("SOP Class UID", f.SOPClassUID); err != nil {
			return err
		}
	}
	if f.TransferSyntaxUID != "" {
		if err := validateUID("Transfer Syntax UID", f.TransferSyntaxUID); err != nil {
			return err
		}
	}
	if err := validateText("job failure detail", f.Detail, 0, MaxFailureLength); err != nil {
		return err
	}
	return master.ValidateTime("failure time", f.Time)
}

// JobStateCount is how many jobs are in one state.
type JobStateCount struct {
	// State is the state.
	State JobState
	// Jobs counts the tenant's jobs in it.
	Jobs int64
}

// InProgressJobs returns counts as a status lists them: one entry per state
// that has not ended and has any, in the contract's order.
func InProgressJobs(counts map[JobState]int64) []JobStateCount {
	var out []JobStateCount
	for _, s := range JobStates() {
		if !s.Ended() && counts[s] > 0 {
			out = append(out, JobStateCount{State: s, Jobs: counts[s]})
		}
	}
	return out
}

// EndState is the state a job ends in once every unit of it has ended:
// failed when some instances were never sent (a unit's attempts ran out),
// partial when some were refused and every other one sent, done when every
// one was sent. The reason goes with failed.
func EndState(c JobCounts) (JobState, string) {
	switch {
	case c.InstancesRemaining() > 0:
		return JobFailed, JobReasonAttemptsRanOut
	case c.InstancesRefused > 0:
		return JobPartial, ""
	}
	return JobDone, ""
}

// ForwardKey is the unique key of the job a receiver's forward makes for a
// reception and a sender, so one is made once whatever runs the step again.
func ForwardKey(receptionID, sender string) string { return "fwd:" + receptionID + ":" + sender }

// ForwardJobID is the id of the job a receiver's forward makes for a
// reception and a sender: made from both, so the store, which makes it in
// the step's transaction, needs no id from a caller and a step run again
// names the same job.
func ForwardJobID(receptionID, sender string) string {
	return "fwd-" + digest(ForwardKey(receptionID, sender))
}

// JobListKey is the unique key of a job's listing unit, so a job is listed
// by one unit at a time.
func JobListKey(jobID string) string { return "list:" + jobID }

// JobScanKey is the unique key of the unit that sends one scan of a job
// (part the store's key of the scan), so a scan is queued once.
func JobScanKey(jobID, part string) string { return "send:" + jobID + ":" + part }

// JobUnitID is the id of a unit of work of a job: its listing (part empty)
// or one of its scans (part the store's key of the scan), made from both so
// a unit queued again is the same unit.
func JobUnitID(jobID, part string) string { return "job-" + digest(jobID+"\x00"+part) }

// digest returns 40 hexadecimal digits of the SHA-256 of s: within the 64
// bytes of an id with its prefix, and far from any collision.
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:20])
}
