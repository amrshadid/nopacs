package tenant

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/record"
)

// Moves is what the tenant's database answers of a move before it is made:
// a session filed under another subject, or a subject merged into another
// (record.KindMove). The move itself is a correction like the others
// (Corrections): PrepareCorrection with Kind record.KindMove, the target's
// session (a session's move) or subject (a merge), ToLabel the subject it
// goes under and Confirm the preview's token; the caller copies every
// object to its key under the new subject (CorrectionScans' NewPath) and
// CommitCorrection publishes it.
//
// # What a move does
//
// Its publishing transaction files each session it moves under the
// destination subject at once: the session's row and the copies of the
// subject's label in its scans, the keys of the instances whose revision is
// past the first, each scan's manifest digest, the two subjects' counts.
// A merge's emptied subject is hidden: its row leaves the catalog, and its
// label is held for arrivals until the merge ends; a lookup by its labels
// answers where the data is now (RelabeledTo). It writes an
// EventCatalogChanged and an EventSessionMoved (or EventSubjectMerged).
// Its purge removes the old tree, as a relabel's does.
//
// UndoCorrection takes a committed move back before its purge: every
// session filed under its subject again, the merged subject shown again,
// the manifests and metadata digests it had; the copies under the
// destination are the caller's to sweep (MarkSwept), as an abandoned
// relabel's are. An undo is refused while an active hold covers what it
// moved, and once anything changed in what it moved since its publish (an
// instance or a scan arrived, another correction touched a scan): the old
// tree would no longer be what the catalog names.
//
// # What a move refuses
//
// The prepare refuses (precondition, nothing written) a move whose token
// does not match the preview made now, a destination that is the source,
// a source or destination an active hold covers (HELD), a source or
// destination a correction not ended covers, a reception of the source
// not ended; and as a label conflict a session label the destination
// holds already, compared case folded (relabel it first). A session's
// move of its subject's last session is refused: that is a merge.
//
// While a move is committed an edit may be made of what it moved (the
// identity rewrite a move asks for is one): it is the one correction
// allowed to overlap a move not ended, and the move's undo is refused
// while it, or any later correction of its scans, exists.
type Moves interface {
	// PreviewMove answers what a move of target (a subject, or a session
	// of it) under the subject labeled to would do, as the catalog holds it
	// now, and the token a move must carry (MoveConfirm): a digest of the
	// answer and of now. A target or destination the tenant does not hold
	// is NotFound.
	PreviewMove(ctx context.Context, target CorrectionTarget, to string, now time.Time) (MovePreview, error)
	// IdentityRewritten records that the identity rewrite a move asked
	// for (CorrectionPrepare.RewriteIdentity) was made, whatever became of
	// the edits it made: it is not made again. A correction that asked none
	// is left as it is; one that is not a move is a precondition failure.
	IdentityRewritten(ctx context.Context, id string) error
	// IdentityRefused records that the identity rewrite a move asked for
	// is refused for good, for one of the fixed phrases (IdentityRefusedProfile,
	// IdentityRefusedCharset, IdentityRefusedNoID, IdentityRefusedTooLarge): the rewrite is owed no
	// more, and the phrase is answered with the correction. Any other
	// phrase is Invalid; a correction that is not a move is a precondition
	// failure.
	IdentityRefused(ctx context.Context, id, why string) error
	// OwedIdentityRewrites lists, in id order after the id given, up to
	// limit committed moves whose identity rewrite is asked and not yet
	// made: what the pass tries again.
	OwedIdentityRewrites(ctx context.Context, after string, limit int) ([]Correction, error)
}

// The fixed phrases a refused identity rewrite is recorded with: never a
// value of the data.
const (
	// IdentityRefusedProfile is a rewrite whose new revisions the
	// profile's hold or its check after refused.
	IdentityRefusedProfile = "the profile or its check after refused the rewritten files"
	// IdentityRefusedCharset is a rewrite of files whose character set
	// would not be kept.
	IdentityRefusedCharset = "a file's character set would not be kept"
	// IdentityRefusedNoID is a rewrite whose destination's files hold no
	// Patient ID to give.
	IdentityRefusedNoID = "the destination's files hold no Patient ID"
	// IdentityRefusedTooLarge is a rewrite of a session that holds more
	// instances than one edit changes (MaxEditInstances).
	IdentityRefusedTooLarge = "a session holds more instances than one edit changes"
)

// MovePreview is what a move would do.
type MovePreview struct {
	// Target is what moves: a subject (a merge) or a session of it; To the
	// label of the subject it goes under.
	Target CorrectionTarget
	To     string
	// Counts are what moves: its sessions, scans, instances and bytes.
	Counts CorrectionCounts
	// Sessions are the labels of the sessions that move, in order.
	Sessions []string
	// Storages are the storages that hold any of it, by name; Replicas
	// their replicas, by primary then name: the copy reaches each.
	Storages []string
	Replicas []ReplicaCopy
	// Holds are the ids of the active holds that cover the target or the
	// destination, in order; a move is refused while any is.
	Holds []string
	// Clashes are the labels of the sessions that move which the
	// destination holds already (case folded), in order; a move is refused
	// while any is.
	Clashes []string
	// Scans are the catalog's id and manifest SHA-256 of every scan that
	// moves, and ToID the destination's id: what names the rows and their
	// content, so a token of one set never matches another with the same
	// labels and counts. In the digest only; never answered.
	Scans []ScanIdentity
	ToID  int64
	// Issued is when the preview was made; Confirm its token.
	Issued  time.Time
	Confirm string
}

// MoveConfirm returns the confirm token of a move's preview: the time it
// was issued, then a digest of everything the person saw, so a move
// carrying it moves what they saw. The digest's form is the move's own,
// so a delete's or an edit's token never confirms a move.
func MoveConfirm(p MovePreview) string {
	issued := strconv.FormatInt(p.Issued.UnixMicro(), 10)
	var b strings.Builder
	w := func(parts ...string) {
		for _, s := range parts {
			b.WriteString(strconv.Itoa(len(s)))
			b.WriteByte(':')
			b.WriteString(s)
		}
	}
	n := func(v int64) string { return strconv.FormatInt(v, 10) }
	t, c := p.Target, p.Counts
	w("nopacs-move-preview-1", issued, t.Subject, t.Session, p.To, n(p.ToID), n(c.Sessions), n(c.Scans), n(c.Instances), n(c.Bytes))
	for _, list := range [][]string{p.Sessions, p.Storages, p.Holds, p.Clashes} {
		w(n(int64(len(list))))
		w(list...)
	}
	w(n(int64(len(p.Replicas))))
	for _, r := range p.Replicas {
		w(r.Primary, r.Replica)
	}
	scans := slices.Clone(p.Scans)
	slices.SortFunc(scans, func(a, b ScanIdentity) int { return cmp.Compare(a.ID, b.ID) })
	w(n(int64(len(scans))))
	for _, s := range scans {
		w(n(s.ID), s.Manifest.String())
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "c1." + issued + "." + hex.EncodeToString(sum[:])
}

// ValidateMove returns an Invalid error unless target and to can name a
// move: a subject, or a session of it, by valid labels, and another
// subject's valid label (case folded).
func ValidateMove(target CorrectionTarget, to string) error {
	if err := target.Validate(); err != nil {
		return err
	}
	if target.Scan != "" || target.Instance != "" {
		return invalid("a move names a session, or a subject to merge")
	}
	if !label.Valid(to) {
		return invalid("a move's destination subject label passes the label rule")
	}
	if label.Fold(to) == label.Fold(target.Subject) {
		return invalid("a move files its sessions under another subject")
	}
	return nil
}

// The events of a move's publish and of its undo.
const (
	// EventSessionMoved is a session filed under another subject, or taken
	// back (SessionMoved).
	EventSessionMoved EventType = "session_moved"
	// EventSubjectMerged is a subject merged into another, or taken back
	// (SubjectMerged).
	EventSubjectMerged EventType = "subject_merged"
)

// SessionMoved is a session's move published or undone, the contract's
// message of that name. It names labels and counts only.
type SessionMoved struct {
	// Correction is the move's id; State committed or undone.
	Correction string       `json:"correction"`
	State      record.State `json:"state"`
	// FromSubject and ToSubject are the subjects' labels; SessionLabel the
	// session's.
	FromSubject  string `json:"from_subject"`
	ToSubject    string `json:"to_subject"`
	SessionLabel string `json:"session_label"`
	// Scans and Instances count what moved.
	Scans     int64 `json:"scans"`
	Instances int64 `json:"instances"`
}

// EventType implements EventPayload.
func (*SessionMoved) EventType() EventType { return EventSessionMoved }

// validate implements EventPayload.
func (p *SessionMoved) validate() error {
	if err := validateMoveEvent(p.Correction, p.State, p.FromSubject, p.ToSubject, p.Scans, p.Instances); err != nil {
		return err
	}
	if !label.Valid(p.SessionLabel) {
		return invalid("a moved session's label passes the label rule")
	}
	return nil
}

// SubjectMerged is a merge published or undone, the contract's message of
// that name. It names labels and counts only.
type SubjectMerged struct {
	// Correction is the merge's id; State committed or undone.
	Correction string       `json:"correction"`
	State      record.State `json:"state"`
	// FromSubject is the subject merged; ToSubject the one it merged into.
	FromSubject string `json:"from_subject"`
	ToSubject   string `json:"to_subject"`
	// Sessions, Scans and Instances count what moved.
	Sessions  int64 `json:"sessions"`
	Scans     int64 `json:"scans"`
	Instances int64 `json:"instances"`
}

// EventType implements EventPayload.
func (*SubjectMerged) EventType() EventType { return EventSubjectMerged }

// validate implements EventPayload.
func (p *SubjectMerged) validate() error {
	if p.Sessions < 0 {
		return invalid("a merge's counts are not negative")
	}
	return validateMoveEvent(p.Correction, p.State, p.FromSubject, p.ToSubject, p.Scans, p.Instances)
}

// validateMoveEvent checks what both events of a move carry.
func validateMoveEvent(id string, state record.State, from, to string, scans, instances int64) error {
	if err := ValidateCorrectionID(id); err != nil {
		return err
	}
	if state != record.StateCommitted && state != record.StateUndone {
		return invalid("a move's event is of its publish or its undo")
	}
	if !label.Valid(from) || !label.Valid(to) {
		return invalid("a move's subject labels pass the label rule")
	}
	if scans < 0 || instances < 0 {
		return invalid("a move's counts are not negative")
	}
	return nil
}

// MoveEvent returns the event of a move c reaching its state (committed or
// undone) at the time given: SessionMoved for a session's move,
// SubjectMerged for a merge.
func MoveEvent(c Correction, at time.Time) (EventRecord, error) {
	if c.Level == record.LevelSession {
		return NewEvent(&SessionMoved{Correction: c.ID, State: c.State, FromSubject: c.Target.Subject, ToSubject: c.ToLabel,
			SessionLabel: c.Target.Session, Scans: c.Counts.Scans, Instances: c.Counts.Instances}, at)
	}
	return NewEvent(&SubjectMerged{Correction: c.ID, State: c.State, FromSubject: c.Target.Subject, ToSubject: c.ToLabel,
		Sessions: c.Counts.Sessions, Scans: c.Counts.Scans, Instances: c.Counts.Instances}, at)
}

// MovedPath returns p as a move from the subject labeled from to the one
// labeled to leaves it: the subject's label replaced, the session's and
// scan's kept.
func MovedPath(p label.Path, to string) label.Path {
	p.Subject = to
	return p
}
