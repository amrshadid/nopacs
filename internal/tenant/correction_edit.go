package tenant

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/master"
)

// MaxEditInstances is the most instances one edit changes. Every one of
// them is read, changed, checked and written again before the edit is
// published, the line of each as it was is kept in the database until the
// edit ends (a few KiB each: about 20 MiB for 2,000 lines of 10 KiB), and
// the publish moves every instance's row in one transaction. A session of
// a few thousand images is edited a scan at a time.
const MaxEditInstances = 2000

// Edits is what the tenant's database keeps of edits beyond what every
// correction has (Corrections): what an edit of a target would change, the
// instances an edit changed, and its undo. An edit is prepared with
// Corrections.PrepareCorrection (CorrectionPrepare.Edits) and published
// with CommitCorrection (CorrectionCommit.Edits): the publish gives each
// instance its new revision at its revision key, keeps the revision it
// superseded (instance_revisions, owned by the edit's purge), and gives
// each scan the manifest's and metadata's digests and the catalog values
// the edit set. An edit is refused (precondition) while an active hold
// covers its target, as a delete is, and while another correction not
// ended overlaps it.
//
// # Undo
//
// A committed edit is undone in two steps. BeginEditUndo marks it, so a
// stop between the two is taken up at the next start, and while it is
// marked it is never purged; the caller then checks it may be undone
// (CheckEditUndo), writes each scan's manifest and metadata naming the old
// revisions again, holding every scan's lock until the publish, and
// UndoEdit publishes the old revisions in one transaction (an undo that
// may not be published is written back and abandoned, AbandonEditUndo): each
// instance's row back at its old revision, the catalog values the scans
// had before, the edit's new revisions the objects it owns (removed by the
// caller from every copy, then MarkSwept). An undone edit not swept is
// among PendingCorrections.
type Edits interface {
	// EditTargets returns, in order of storage, Series Instance UID and
	// SOP Instance UID, the instances an edit of target would change, at
	// most limit of them; one more than limit tells the caller there are
	// more. NotFound when the catalog holds no such target.
	EditTargets(ctx context.Context, target CorrectionTarget, limit int) ([]EditTarget, error)
	// CorrectionEdits returns the instances of an edit, in the order of
	// EditTargets.
	CorrectionEdits(ctx context.Context, id string) ([]EditedInstance, error)
	// BeginEditUndo marks a committed edit as being undone, with the
	// reason given, and writes the audit entry. A rebuild running, a
	// switch of one of its storages, a repair of one of its scans, or an
	// edit that is not committed is a precondition failure; one marked
	// already is answered as it stands.
	BeginEditUndo(ctx context.Context, id, reason string, now time.Time, audit master.AuditEntry, guard Guard) (Correction, error)
	// UndoEdit publishes the undo of an edit BeginEditUndo marked (the
	// section above), with an EventCatalogChanged; one undone already is
	// answered as it stands.
	UndoEdit(ctx context.Context, u EditUndo) (Correction, error)
	// CheckEditUndo answers, as UndoEdit would, whether the undo of an edit
	// BeginEditUndo marked may be published now (no rebuild, no switch of
	// its storages, no repair of its scans, no active hold over it): a
	// precondition failure when not, wrapping ErrNotBeingUndone for an edit
	// no longer being undone. It changes nothing.
	CheckEditUndo(ctx context.Context, id string, now time.Time) error
	// AbandonEditUndo ends the undo of an edit BeginEditUndo marked without
	// publishing it, when it may not be (CheckEditUndo, UndoEdit refused):
	// the edit stays committed, its mark and undo reason cleared, and each
	// scan keeps, as the catalog's, the digests of the manifest and metadata
	// the caller wrote back naming the edit's new revisions (u.Manifests,
	// u.Sidecars), under each scan's lock; the server's audit entry
	// (u.Audit) and an EventCatalogChanged are written in the same
	// transaction, so an undo the server took back is seen. An edit not
	// being undone is answered as it stands.
	AbandonEditUndo(ctx context.Context, u EditUndo) (Correction, error)
}

// EditTarget is one instance an edit would change, as the catalog holds it.
type EditTarget struct {
	// Storage is its scan's storage; Path its scan's labels.
	Storage string
	Path    label.Path
	// StudyUID and SeriesUID name its session and scan.
	StudyUID, SeriesUID string
	// UID is its SOP Instance UID; Revision its revision now.
	UID      string
	Revision int
	// Key is its object's whole key; Size and SHA256 its archived bytes'.
	Key    string
	Size   int64
	SHA256 SHA256
	// Profile is the profile version that de-identified it; zero for an
	// instance archived as received.
	Profile ProfileRef
	// Receiver is the receiver its scan was received through.
	Receiver string
}

// EditPrepared is one instance of an edit as it is prepared: what the
// catalog held of it when the edit read it, and its line of the scan's
// metadata then, which a stop before the publish, and an undo, write back.
type EditPrepared struct {
	// UID is its SOP Instance UID; SeriesUID its scan's.
	UID, SeriesUID string
	// OldRevision and OldSHA256 are its revision and archived checksum.
	OldRevision int
	OldSHA256   SHA256
	// RestoreLine is its line of the scan's metadata, as JSON.
	RestoreLine []byte
}

// EditWritten is the new revision an edit wrote of one instance.
type EditWritten struct {
	// UID is its SOP Instance UID.
	UID string
	// SHA256 and Size are the new revision's bytes'.
	SHA256 SHA256
	Size   int64
	// Line is the new revision's line of the scan's metadata, as JSON,
	// which an undo that may not be published writes back.
	Line []byte
}

// EditValues are the values the catalog holds of a scan (ScanValues) that
// an edit sets: each applied only when its Set field says the edit names
// it, the others left as they are.
type EditValues struct {
	// SeriesDescription is Series Description (0008,103E).
	SeriesDescription string
	SetDescription    bool
	// Modality is Modality (0008,0060).
	Modality    string
	SetModality bool
	// SeriesNumber is Series Number (0020,0011), when HasSeriesNumber.
	SeriesNumber    int32
	HasSeriesNumber bool
	SetSeriesNumber bool
}

// EditedInstance is one instance of an edit.
type EditedInstance struct {
	EditPrepared
	// NewRevision is the revision the edit gives it; NewSHA256 and NewSize
	// its bytes', zero until the edit is published.
	NewRevision int
	NewSHA256   SHA256
	NewSize     int64
	// OldKey is the old revision's object's whole key, OldSize its size.
	OldKey  string
	OldSize int64
	// Storage and Path are its scan's when the edit was made.
	Storage string
	Path    label.Path
	// Before are the catalog values of its scan before the edit (its study
	// date left empty: an edit never changes it), which an undo, and a
	// stop before the publish, write back into the scan's metadata.
	Before ScanValues
	// NewLine is the new revision's line of the scan's metadata, as JSON,
	// from the edit's publish until it ends; After the catalog values of
	// the scan now. An undo that may not be published writes both back.
	NewLine []byte
	After   ScanValues
}

// EditUndo is the undo of an edit to publish.
type EditUndo struct {
	// ID names the edit; Now is when.
	ID  string
	Now time.Time
	// Manifests and Sidecars are, by Series Instance UID, the SHA-256 of
	// the manifest and metadata file the caller wrote naming the old
	// revisions again, as CorrectionCommit's are (for AbandonEditUndo, the
	// new revisions).
	Manifests, Sidecars map[string]SHA256
	// Audit is, for AbandonEditUndo, the server's entry that says the undo
	// was taken back and why (no value of a file), written with it.
	// Required there; UndoEdit takes none (BeginEditUndo wrote the entry).
	Audit *master.AuditEntry
}

// ErrNotBeingUndone is, wrapped as a precondition failure, the answer of
// CheckEditUndo and UndoEdit for an edit no longer being undone (undone,
// purging, or its undo abandoned) by the time they read it: another run
// of its undo ended it, and the caller writes nothing more.
var ErrNotBeingUndone = errors.New("the edit is no longer being undone")

// StateReasonUndoing is the state reason of a committed edit BeginEditUndo
// marked: its undo is taken up at the next start if it stops.
const StateReasonUndoing = "being undone"

// validateEdits returns an Invalid error unless an edit's instances can be
// prepared: 1 to MaxEditInstances, each named once with its revision,
// checksum and line.
func validateEdits(edits []EditPrepared) error {
	if len(edits) == 0 || len(edits) > MaxEditInstances {
		return invalid("an edit changes 1 to " + strconv.Itoa(MaxEditInstances) + " instances")
	}
	seen := make(map[string]bool, len(edits))
	for _, e := range edits {
		if err := ValidateInstanceUID(e.UID); err != nil {
			return err
		}
		if err := validateUID("Series Instance UID", e.SeriesUID); err != nil {
			return err
		}
		if seen[e.UID] {
			return invalid("an edit names each instance once")
		}
		seen[e.UID] = true
		if e.OldRevision < 1 || e.OldRevision >= layout.MaxRevision {
			return invalid("an edited instance's revision is 1 to " + strconv.Itoa(layout.MaxRevision-1))
		}
		if e.OldSHA256.IsZero() || len(e.RestoreLine) == 0 {
			return invalid("an edited instance states its checksum and the line it can write back")
		}
	}
	return nil
}

// Validate returns an Invalid error unless u can be published.
func (u EditUndo) Validate() error {
	if err := ValidateCorrectionID(u.ID); err != nil {
		return err
	}
	return master.ValidateTime("undo time", u.Now)
}
