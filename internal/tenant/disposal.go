package tenant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// Disposal is what the tenant's database keeps of disposals: the due,
// unheld sessions of a rule a person previews, the dispose job a confirmed
// preview or the daily pass queues, and what became of each session. A
// session is deleted through the ordinary delete (Corrections), never a
// second path: the job runner previews and prepares a delete of the
// session with its CorrectionPrepare.Retention set, which the prepare
// checks again (the session due under the rule, not held, not exempt for
// the pass, the clock not paused).
type Disposal interface {
	// PreviewDisposal answers what a disposal of the rule would delete at
	// now: at most limit (1 to MaxDisposeSessions) due sessions not held,
	// oldest due first, the counts of the rest and of the held, and the
	// confirm token. A rule the tenant does not have is NotFound; a paused
	// retention a precondition failure (ErrRetentionPaused).
	PreviewDisposal(ctx context.Context, rule string, limit int, now time.Time) (DisposalPreview, error)
	// QueueDispose makes a dispose job of d.ID (kind JobDispose) listing
	// the sessions to delete, in one transaction with its audit entry, and
	// answers it and the preview it lists. A person's carries the token of
	// a preview made within MaxConfirmAge that the preview made again in
	// this transaction, at the token's time from the holds, rules and
	// sessions as they stand, still matches: a hold placed or a rule
	// changed since refuses it, a session that only became due since does
	// not; a token used already answers the job it made, and false. The
	// daily pass's (Scheduled) takes up to d.Max sessions not exempt, and
	// is refused while the rule has a dispose job not ended. Both are
	// refused while retention is paused, while a rebuild runs, or when
	// nothing is due.
	QueueDispose(ctx context.Context, d DisposeQueue, audit master.AuditEntry, guard Guard) (Job, DisposalPreview, bool, error)
	// DisposeJob answers the dispose job of that id and the sessions it
	// lists, in order, with what became of each.
	DisposeJob(ctx context.Context, id string) (DisposeJob, error)
	// RecordDisposed records what became of one session of a dispose job:
	// deleted by the correction named, or skipped for a reason. A session
	// recorded already keeps its first outcome. A job ended (done or
	// canceled) refuses it (ErrDisposeJobEnded): its tallies are final.
	RecordDisposed(ctx context.Context, jobID, studyUID string, outcome DisposeOutcome, correction string) error
	// EndDisposeJob ends the dispose job of that id (done) once every
	// session it lists has its outcome, deleted or skipped, with its event,
	// and answers it; a job ended already is answered as it is. A session
	// whose outcome is not recorded refuses it (a precondition failure). Until
	// the job ends, the daily pass queues no other of its rule.
	EndDisposeJob(ctx context.Context, id string, now time.Time) (Job, error)
}

// The bounds of disposal.
const (
	// MaxDisposeSessions is the most sessions a preview lists.
	MaxDisposeSessions = 1000
	// DefaultDisposeSessions is a preview's when none is asked.
	DefaultDisposeSessions = 100
	// MaxPerPass bounds retention.max-per-pass.
	MaxPerPass = 100000
)

// DisposalPreview is what a disposal deletes.
type DisposalPreview struct {
	// Rule names the rule.
	Rule string
	// Sessions are those it deletes, oldest due first, none held.
	Sessions []DueSession
	// NotListed counts the due sessions not held past those listed; Held
	// the due sessions a hold keeps.
	NotListed, Held int64
	// Counts are what the listed sessions cover.
	Counts CorrectionCounts
	// Max is the most it lists; Issued when it was made; Confirm its token.
	Max     int
	Issued  time.Time
	Confirm string
}

// DisposeQueue is a dispose job to make.
type DisposeQueue struct {
	// ID is the job's id, given by the caller.
	ID string
	// Rule names the rule.
	Rule string
	// Reason is the admin's, each delete's reason; empty for the pass,
	// whose deletes each carry the server's.
	Reason string
	// Confirm is a person's preview token; empty for the pass.
	Confirm string
	// KeyID is the admin's key, empty for the local operator or the pass.
	KeyID string
	// Scheduled says the daily pass makes it.
	Scheduled bool
	// Max is, for the pass, retention.max-per-pass.
	Max int
	// Now is when.
	Now time.Time
}

// Validate returns an Invalid error unless d is well formed.
func (d DisposeQueue) Validate() error {
	if err := master.ValidateID("job id", d.ID); err != nil {
		return err
	}
	if err := ValidateName("rule", d.Rule); err != nil {
		return err
	}
	if d.KeyID != "" {
		if err := master.ValidateID("key id", d.KeyID); err != nil {
			return err
		}
	}
	if d.Scheduled {
		if d.Confirm != "" || d.Reason != "" {
			return invalid("the daily pass's disposal carries no token and no reason")
		}
		if d.Max < 1 || d.Max > MaxPerPass {
			return invalid("a pass disposes of 1 to " + strconv.Itoa(MaxPerPass) + " sessions")
		}
	} else {
		if err := validateCorrectionReason("disposal's reason", d.Reason); err != nil {
			return err
		}
		if d.Confirm == "" || len(d.Confirm) > MaxConfirmLength {
			return invalid("a disposal carries the confirm token of its preview")
		}
	}
	return master.ValidateTime("time", d.Now)
}

// DisposeOutcome is what became of one session of a dispose job.
type DisposeOutcome string

// The outcomes, the job's tallies. A dispose job is not tried again, so
// every refusal of a session's delete that would not pass by itself is a
// skip with its reason: a session left without an outcome keeps the job
// open, and a job not ended keeps its rule from the daily pass. Only a
// temporary failure (a database or storage not answering, retention paused
// for its clock) leaves a session for the next pass. A session skipped is
// listed again by the rule's next job while it stays due.
const (
	DisposePending        DisposeOutcome = ""
	DisposeDeleted        DisposeOutcome = "deleted"
	DisposeSkippedHeld    DisposeOutcome = "skipped|held"
	DisposeSkippedNotDue  DisposeOutcome = "skipped|not_due"
	DisposeSkippedDeleted DisposeOutcome = "skipped|already_deleted"
	DisposeSkippedUndone  DisposeOutcome = "skipped|undone"
	// DisposeSkippedOverlap is a session another correction not ended
	// holds (ErrOverlap): it may stay prepared or purging for as long as a
	// hold stands.
	DisposeSkippedOverlap DisposeOutcome = "skipped|overlap"
	// DisposeSkippedRefused is a session whose delete was refused for any
	// other reason that does not pass by itself (the request or the
	// session's state).
	DisposeSkippedRefused DisposeOutcome = "skipped|refused"
)

// Valid reports whether o is an outcome RecordDisposed takes.
func (o DisposeOutcome) Valid() bool {
	switch o {
	case DisposeDeleted, DisposeSkippedHeld, DisposeSkippedNotDue, DisposeSkippedDeleted, DisposeSkippedUndone, DisposeSkippedOverlap,
		DisposeSkippedRefused:
		return true
	}
	return false
}

// The refusals of a disposal's steps, each a case inside the precondition
// kind.
var (
	// ErrOverlap is in the chain of a correction refused because another
	// correction not ended (prepared, committed or purging) covers some of
	// its target.
	ErrOverlap = errors.New("another correction holds the target")
	// ErrDisposeJobEnded is in the chain of a disposal's step refused
	// because its dispose job has ended (done, or canceled): a cancel stops
	// the job's deletes, even one whose session was read before the cancel.
	ErrDisposeJobEnded = errors.New("the dispose job has ended")
)

// OverlapError returns the precondition failure of a correction another
// correction not ended overlaps.
func OverlapError() error {
	return fault.Wrap(fault.Precondition, ErrOverlap, "a correction on the target is prepared or not yet purged")
}

// DisposeJobEndedError returns the precondition failure of a disposal's
// step whose job has ended.
func DisposeJobEndedError() error {
	return fault.Wrap(fault.Precondition, ErrDisposeJobEnded, "the dispose job has ended; it deletes and records nothing more")
}

// DisposeJob is a dispose job's own part.
type DisposeJob struct {
	// Job is the job.
	Job Job
	// Rule, Reason and Scheduled are what it was made with.
	Rule, Reason string
	Scheduled    bool
	// Sessions are those it lists, in order.
	Sessions []DisposeSession
}

// DisposeSession is one session a dispose job lists.
type DisposeSession struct {
	// StudyUID names it; SubjectLabel and SessionLabel are its labels when
	// listed.
	StudyUID, SubjectLabel, SessionLabel string
	// Due is the due time listed.
	Due time.Time
	// Instances and Bytes are what it held when listed.
	Instances, Bytes int64
	// Outcome is what became of it; Correction the delete that took it.
	Outcome    DisposeOutcome
	Correction string
}

// RetentionOrigin is what a delete a disposal makes carries.
type RetentionOrigin struct {
	// Rule names the rule the session is due under.
	Rule string
	// StudyUID is the Study Instance UID of the session the dispose job
	// listed: the prepare refuses the delete (NotDue) when the target's
	// labels no longer name that session.
	StudyUID string
	// Due is the due time listed for it.
	Due time.Time
	// Scheduled says the daily pass made it, whose deletes leave alone a
	// session a person's undo exempts.
	Scheduled bool
	// JobID is the dispose job the delete is made for: the prepare refuses
	// it (ErrDisposeJobEnded) in its transaction once that job has ended,
	// so a cancel that lands after the runner read the job still stops the
	// delete. Empty for a delete bound to no job.
	JobID string
}

// DisposeConfirm returns a disposal preview's token: the time it was
// issued and its bound, then a digest of everything listed, so a disposal
// carrying it acts on what the person saw.
func DisposeConfirm(p DisposalPreview) string {
	var b strings.Builder
	w := func(parts ...string) {
		for _, s := range parts {
			b.WriteString(strconv.Itoa(len(s)))
			b.WriteByte(':')
			b.WriteString(s)
		}
	}
	n := func(v int64) string { return strconv.FormatInt(v, 10) }
	issued := strconv.FormatInt(p.Issued.UnixMicro(), 10)
	c := p.Counts
	w("nopacs-dispose-preview-1", issued, p.Rule, strconv.Itoa(p.Max), n(p.NotListed), n(p.Held), n(c.Sessions), n(c.Scans),
		n(c.Instances), n(c.Bytes), n(int64(len(p.Sessions))))
	for _, s := range p.Sessions {
		w(s.StudyUID, s.SubjectLabel, s.SessionLabel, n(s.Due.UnixMicro()), n(s.Instances), n(s.Bytes))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "d1." + issued + "." + strconv.Itoa(p.Max) + "." + hex.EncodeToString(sum[:])
}

// DisposeConfirmParts returns when a disposal token was issued and the
// bound its preview listed, and false for a string that is not one.
func DisposeConfirmParts(token string) (time.Time, int, bool) {
	f := strings.Split(token, ".")
	if len(f) != 4 || f[0] != "d1" || len(f[3]) != 64 {
		return time.Time{}, 0, false
	}
	at, ok := issuedAt(f[1])
	limit, err := strconv.Atoi(f[2])
	if !ok || err != nil || limit < 1 || limit > MaxDisposeSessions {
		return time.Time{}, 0, false
	}
	return at, limit, true
}

// ValidateDisposeMax returns an Invalid error unless limit is a preview's
// bound.
func ValidateDisposeMax(limit int) error {
	if limit < 1 || limit > MaxDisposeSessions {
		return invalid("a disposal lists 1 to " + strconv.Itoa(MaxDisposeSessions) + " sessions")
	}
	return nil
}
