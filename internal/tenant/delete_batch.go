package tenant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// JobDeleteBatch is the kind of a batch delete job: many subjects, sessions
// or scans deleted one at a time, each by the ordinary delete
// (DeleteBatches).
const JobDeleteBatch JobKind = "delete_batch"

// DeleteBatches is what the tenant's database keeps of batch deletes: the
// preview of many targets under one token, the job a confirmed preview
// makes, and what became of each target. A target is deleted through the
// ordinary delete (Corrections), never a second path: the job's runner
// previews and prepares a delete of the target with its
// CorrectionPrepare.Batch set, which the prepare checks (the job neither
// ended nor paused, the target not yet met) and records on the target in
// its own transaction, so a restart finds the delete a target was given
// and never makes a second one. Holds, retention's keep-at-least, undo,
// grace and purge on every copy then hold as for any delete.
type DeleteBatches interface {
	// PreviewDeleteBatch answers what a batch delete of targets with reason
	// would delete at now: each target's delete preview (its counts,
	// storages, replicas, holds and what retention keeps), the totals, and
	// the token (BatchConfirm), a digest of the reason, the targets in
	// order and what each preview answered. A target the tenant does not
	// hold is NotFound, naming its place in the list.
	PreviewDeleteBatch(ctx context.Context, targets []CorrectionTarget, reason string, now time.Time) (DeleteBatchPreview, error)
	// QueueDeleteBatch makes a batch delete job of q.ID (kind
	// JobDeleteBatch) listing q's targets in order, each with the digest
	// of its preview, in one transaction with its audit entry, and answers
	// it. The token must be one of a preview made within MaxConfirmAge of
	// the same targets and reason that a preview made again, at the
	// token's time from the catalog as it stands, still matches: a target
	// changed since refuses it. A token used already answers the job it
	// made, and false, when the targets and reason are the same, and is a
	// precondition failure otherwise. It is refused while another batch
	// delete job of the tenant has not ended (one at a time) and while a
	// rebuild runs.
	QueueDeleteBatch(ctx context.Context, q DeleteBatchQueue, audit master.AuditEntry, guard Guard) (Job, bool, error)
	// DeleteBatchJob answers the batch delete job of that id: the job, its
	// reason, whether it is paused, and how many targets it lists.
	DeleteBatchJob(ctx context.Context, id string) (DeleteBatchJob, error)
	// DeleteBatchTargets answers, in order, at most limit (1 to
	// MaxBatchTargetPage) targets of the job after the place given (0 for
	// the first), each with what became of it.
	DeleteBatchTargets(ctx context.Context, id string, after, limit int) ([]DeleteBatchTarget, error)
	// RecordBatchTarget records what became of one target: deleted by the
	// delete its prepare recorded on it, or skipped for a reason; the job's
	// counts move with it (a target deleted counts its instances sent, one
	// skipped refused), its tally too, and a job queued starts running,
	// with its event. A target recorded already keeps its first outcome. A
	// job ended refuses it (ErrBatchStopped): its counts are final.
	RecordBatchTarget(ctx context.Context, r BatchRecord) (Job, error)
	// EndDeleteBatchJob ends the job (done when every target was deleted,
	// partial when any was skipped) once every target has its outcome, with
	// its event, and answers it; one ended already is answered as it is. A
	// target whose outcome is not recorded refuses it (precondition).
	EndDeleteBatchJob(ctx context.Context, id string, now time.Time) (Job, error)
	// StopDeleteBatchJob ends the job failed, with reason (1 to
	// MaxFailureLength bytes) and its event, the targets not met left
	// without an outcome, and answers it; one ended already is answered as
	// it is. It is how a batch ends when the key that made it may no longer
	// delete: its deletes stop rather than go on in the name of a key
	// revoked or demoted.
	StopDeleteBatchJob(ctx context.Context, id, reason string, now time.Time) (Job, error)
	// PauseDeleteBatch pauses the job (paused true) or resumes it, with its
	// audit entry, and answers it with whether it is paused now. A paused
	// job stays in its state; the delete of its next target is refused in
	// its prepare (ErrBatchStopped) until it resumes. A job ended is a
	// precondition failure; one already as asked is answered as it is.
	PauseDeleteBatch(ctx context.Context, id string, paused bool, now time.Time, audit master.AuditEntry, guard Guard) (DeleteBatchJob, error)
}

// The bounds of batch deletes.
const (
	// MaxDeleteBatchTargets is the most targets one batch lists, the
	// contract's; the setting correction.batch-max may lower it.
	MaxDeleteBatchTargets = 10000
	// MaxBatchTargetPage is the most targets one DeleteBatchTargets call
	// answers: a page of the contract's most, and one more, by which a
	// caller tells whether another page follows.
	MaxBatchTargetPage = master.MaxPageLimit + 1
)

// BatchOutcome is what became of one target of a batch delete job.
type BatchOutcome string

// The outcomes, the job's tallies. A target is never forced: every refusal
// of its delete that does not pass by itself is a skip with its reason, and
// only a temporary failure (a database or a storage not answering) leaves
// it for the runner's next pass.
const (
	BatchPending BatchOutcome = ""
	BatchDeleted BatchOutcome = "deleted"
	// BatchSkippedHeld is a target a hold covers.
	BatchSkippedHeld BatchOutcome = "skipped|held"
	// BatchSkippedKept is a target a retention rule keeps at least until a
	// time to come; a batch never overrides it.
	BatchSkippedKept BatchOutcome = "skipped|kept"
	// BatchSkippedChanged is a target whose delete preview answers
	// otherwise than when the job was made: data arrived, a job was made,
	// a hold was placed or released, a replica attached.
	BatchSkippedChanged BatchOutcome = "skipped|changed"
	// BatchSkippedGone is a target the tenant no longer holds: deleted or
	// relabeled since.
	BatchSkippedGone BatchOutcome = "skipped|gone"
	// BatchSkippedOverlap is a target another correction not ended holds
	// (ErrOverlap), or a reception of it in progress.
	BatchSkippedOverlap BatchOutcome = "skipped|overlap"
	// BatchSkippedRefused is a target whose delete was refused for any
	// other reason that does not pass by itself.
	BatchSkippedRefused BatchOutcome = "skipped|refused"
)

// BatchOutcomes returns every outcome a target may be recorded with, in
// the order the contract lists them.
func BatchOutcomes() []BatchOutcome {
	return []BatchOutcome{BatchDeleted, BatchSkippedHeld, BatchSkippedKept, BatchSkippedChanged, BatchSkippedGone, BatchSkippedOverlap,
		BatchSkippedRefused}
}

// Valid reports whether o is an outcome RecordBatchTarget takes.
func (o BatchOutcome) Valid() bool {
	for _, v := range BatchOutcomes() {
		if o == v {
			return true
		}
	}
	return false
}

// ErrBatchStopped is in the chain of a batch delete's step refused because
// its job has ended (done, partial or canceled) or is paused: a cancel or a
// pause stops the job's deletes, even one whose target the runner read
// before it.
var ErrBatchStopped = errors.New("the batch delete job has ended or is paused")

// ErrBatchTargetTaken is in the chain of a batch's step refused because its
// target was met already, or has a delete not abandoned recorded on it:
// another runner of the job got there first. It is never a skip; the
// runner leaves the target for its next pass, which finds what the other
// recorded.
var ErrBatchTargetTaken = errors.New("the batch's target was met or has a delete already")

// BatchTargetTakenError returns the precondition failure of a batch's step
// on a target another runner of the job got to first.
func BatchTargetTakenError(why string) error {
	return fault.Wrap(fault.Precondition, ErrBatchTargetTaken, why)
}

// BatchStoppedError returns the precondition failure of a batch delete's
// step whose job has ended or is paused.
func BatchStoppedError(why string) error {
	return fault.Wrap(fault.Precondition, ErrBatchStopped, "the batch delete job "+why+"; it deletes nothing more until it runs again")
}

// BatchItem is one target of a batch delete's preview: its delete preview,
// and the digest of what it answered (DeleteFingerprint).
type BatchItem struct {
	DeletePreview
	// Fingerprint is DeleteFingerprint of the preview.
	Fingerprint string
}

// DeleteBatchPreview is what a batch delete deletes.
type DeleteBatchPreview struct {
	// Reason is the reason every delete carries.
	Reason string
	// Items are the targets, in the order given.
	Items []BatchItem
	// Counts are what the targets cover together.
	Counts CorrectionCounts
	// Held counts the targets a hold covers, Kept those a retention rule
	// keeps: the job skips both.
	Held, Kept int64
	// Issued is when it was made; Confirm its token (BatchConfirm).
	Issued  time.Time
	Confirm string
}

// DeleteBatchQueue is a batch delete job to make.
type DeleteBatchQueue struct {
	// ID is the job's id, given by the caller.
	ID string
	// Targets are what it deletes, in order; Reason each delete's reason.
	Targets []CorrectionTarget
	Reason  string
	// Confirm is the preview's token (BatchConfirm).
	Confirm string
	// KeyID is the key that asked, empty for the local operator.
	KeyID string
	// Max is the most targets the tenant lets a batch list now
	// (correction.batch-max), 1 to MaxDeleteBatchTargets.
	Max int
	// Now is when.
	Now time.Time
}

// Validate returns an Invalid error unless q is well formed.
func (q DeleteBatchQueue) Validate() error {
	if err := master.ValidateID("job id", q.ID); err != nil {
		return err
	}
	if q.Max < 1 || q.Max > MaxDeleteBatchTargets {
		return invalid("a batch's bound is 1 to " + strconv.Itoa(MaxDeleteBatchTargets) + " targets")
	}
	if err := ValidateBatchTargets(q.Targets, q.Max); err != nil {
		return err
	}
	if err := validateCorrectionReason("batch's reason", q.Reason); err != nil {
		return err
	}
	if q.KeyID != "" {
		if err := master.ValidateID("key id", q.KeyID); err != nil {
			return err
		}
	}
	if _, ok := BatchConfirmIssued(q.Confirm); !ok {
		return invalid("a batch delete carries the confirm token of its preview")
	}
	return master.ValidateTime("time", q.Now)
}

// ValidateBatchTargets returns an Invalid error unless targets are 1 to max
// subjects, sessions or scans, each valid, none named twice and none
// inside another (labels compared case folded, as the catalog holds them
// unique): a target inside another would only ever be met as gone.
func ValidateBatchTargets(targets []CorrectionTarget, max int) error {
	if len(targets) == 0 {
		return invalid("a batch names at least one target")
	}
	if len(targets) > max {
		return invalid("a batch names at most " + strconv.Itoa(max) + " targets")
	}
	fold := func(t CorrectionTarget) label.Path {
		return label.Path{Subject: label.Fold(t.Subject), Session: label.Fold(t.Session), Scan: label.Fold(t.Scan)}
	}
	seen := make(map[label.Path]int, len(targets))
	for i, t := range targets {
		where := "target " + strconv.Itoa(i+1)
		if err := t.Validate(); err != nil {
			return invalid(where + ": " + fault.Message(err))
		}
		if t.Instance != "" {
			return invalid(where + ": a batch deletes subjects, sessions or scans, never one instance")
		}
		f := fold(t)
		if j, ok := seen[f]; ok {
			return invalid(where + " names what target " + strconv.Itoa(j) + " names")
		}
		seen[f] = i + 1
	}
	for i, t := range targets {
		f := fold(t)
		var ups []label.Path
		if f.Session != "" {
			ups = append(ups, label.Path{Subject: f.Subject})
		}
		if f.Scan != "" {
			ups = append(ups, label.Path{Subject: f.Subject, Session: f.Session})
		}
		for _, up := range ups {
			if j, ok := seen[up]; ok {
				return invalid("target " + strconv.Itoa(i+1) + " lies inside target " + strconv.Itoa(j))
			}
		}
	}
	return nil
}

// DeleteBatchJob is a batch delete job's own part.
type DeleteBatchJob struct {
	// Job is the job.
	Job Job
	// Reason is each delete's reason.
	Reason string
	// Paused says a person paused it.
	Paused bool
	// Targets counts the targets it lists.
	Targets int64
}

// DeleteBatchTarget is one target a batch delete job lists.
type DeleteBatchTarget struct {
	// Ord is its place in the list, from 1.
	Ord int
	// Target is what it names.
	Target CorrectionTarget
	// Fingerprint is the digest of its delete preview when the job was
	// made (DeleteFingerprint).
	Fingerprint string
	// Instances and Bytes are what it held then.
	Instances, Bytes int64
	// Outcome is what became of it; Correction the delete its prepare
	// recorded (empty for none), which a skip may leave naming a delete
	// that was abandoned.
	Outcome    BatchOutcome
	Correction string
}

// BatchOrigin is what a delete a batch delete job makes carries
// (CorrectionPrepare.Batch).
type BatchOrigin struct {
	// JobID is the job; Ord the target's place in it.
	JobID string
	Ord   int
}

// BatchRecord is one target's outcome to record.
type BatchRecord struct {
	// JobID is the job; Ord the target's place in it.
	JobID string
	Ord   int
	// Outcome is what became of it; Correction, for BatchDeleted, the
	// delete its prepare recorded, empty otherwise.
	Outcome    BatchOutcome
	Correction string
	// Now is when.
	Now time.Time
}

// Validate returns an Invalid error unless r can be recorded.
func (r BatchRecord) Validate() error {
	if err := master.ValidateID("job id", r.JobID); err != nil {
		return err
	}
	if r.Ord < 1 || r.Ord > MaxDeleteBatchTargets {
		return invalid("a batch's target is named by its place, 1 to " + strconv.Itoa(MaxDeleteBatchTargets))
	}
	if !r.Outcome.Valid() {
		return invalid("a batch target's outcome is deleted or one of the skips")
	}
	if (r.Outcome == BatchDeleted) != (r.Correction != "") {
		return invalid("a target deleted names its delete, and only then")
	}
	if r.Correction != "" {
		if err := ValidateCorrectionID(r.Correction); err != nil {
			return err
		}
	}
	return master.ValidateTime("time", r.Now)
}

// ScanIdentity is one scan a delete would take, as its preview names it:
// the catalog's id of its row and the SHA-256 of its manifest now (zero
// when the catalog holds none).
type ScanIdentity struct {
	ID       int64
	Manifest SHA256
}

// DeleteFingerprint returns the digest of what a delete preview answered,
// without the time it was issued: two previews of one target fingerprint
// the same exactly when a delete's token of the one would match the other
// (DeleteConfirm), so a batch's runner tells a target changed since the
// job was made from one preview made when the target's turn comes.
func DeleteFingerprint(p DeletePreview) string { return deleteDigest(p, "") }

// BatchConfirm returns a batch delete preview's token: the time it was
// issued, then a digest of the reason, the targets in order and each
// one's fingerprint, so a batch carrying it acts on the list, the reason
// and the data the person saw.
func BatchConfirm(p DeleteBatchPreview) string {
	var b strings.Builder
	w := func(parts ...string) {
		for _, s := range parts {
			b.WriteString(strconv.Itoa(len(s)))
			b.WriteByte(':')
			b.WriteString(s)
		}
	}
	issued := strconv.FormatInt(p.Issued.UnixMicro(), 10)
	w("nopacs-delete-batch-1", issued, p.Reason, strconv.Itoa(len(p.Items)))
	for _, it := range p.Items {
		t := it.Target
		w(t.Subject, t.Session, t.Scan, it.Fingerprint)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "b1." + issued + "." + hex.EncodeToString(sum[:])
}

// BatchConfirmIssued returns when a batch token was issued, and false for a
// string that is not one.
func BatchConfirmIssued(token string) (time.Time, bool) {
	f := strings.Split(token, ".")
	if len(f) != 3 || f[0] != "b1" || len(f[2]) != 64 {
		return time.Time{}, false
	}
	return issuedAt(f[1])
}
