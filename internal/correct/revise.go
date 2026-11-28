package correct

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ReviseRequest is a new revision of one archived instance, made and
// checked by the caller, which a profile re-run asks the engine to publish.
type ReviseRequest struct {
	// Target names the instance by its labels and SOP Instance UID.
	Target tenant.CorrectionTarget
	// OldSHA256 is the archived checksum of the revision File was made
	// from: the revision changed since is answered tenant.ErrTargetChanged.
	OldSHA256 tenant.SHA256
	// File is the new revision's bytes, already de-identified and checked.
	File []byte
	// Profile is the profile version that de-identified File: the
	// instance's line names it from the publish on.
	Profile tenant.ProfileRef
	// ProfileSHA256 is the checksum of Profile's expanded text, Base the
	// tenant's base profile in force when File was made ("name@version", empty
	// for none), Unnamed and UnnamedOver the elements no rule of Profile named
	// in that run: the new revision's line records them, so it describes how
	// File was made and not how the revision it replaces was.
	ProfileSHA256, Base string
	Unnamed             []sidecar.UnnamedElement
	UnnamedOver         int
	// Values are the catalog values of the scan File changes, as an edit
	// would set them (Series Description, Modality, Series Number); a
	// removed or emptied one is given with an empty Value.
	Values []EditChange
	// Reason is why; KeyID the key that asked.
	Reason, KeyID string
	// Audit is the request's entry; Guard the check of who asks.
	Audit master.AuditEntry
	Guard tenant.Guard
}

// Revise publishes File as the next revision of one instance, through the
// edit's own path: its row and records on every storage it touches, the
// bytes read back, the scan's manifest and metadata naming the new
// revision, the superseded revision kept for the purge and for an undo. It
// is an edit by its kind (an undo, a purge, a hold and a rebuild treat it
// as one) whose new file was made by a profile instead of by named values,
// so no judge runs: the caller ran the check after. The instance's profile
// version is the one that made File from the publish on, and the version it
// had before again after an undo. A hold over the instance, another
// correction not ended over it, or a change since File was made refuses it.
func (e *Engine) Revise(ctx context.Context, r ReviseRequest) (tenant.Correction, error) {
	if err := e.fence(ctx); err != nil {
		return tenant.Correction{}, err
	}
	if r.Target.Instance == "" || len(r.File) == 0 || r.Profile.IsZero() {
		return tenant.Correction{}, fault.New(fault.Invalid, "a revision names an instance, its bytes and the profile version that made them")
	}
	if err := r.Target.Validate(); err != nil {
		return tenant.Correction{}, err
	}
	if len(r.Values) > 0 {
		for _, c := range r.Values {
			if !scanValueTags[c.Tag] {
				return tenant.Correction{}, fault.New(fault.Invalid, "a revision names catalog values of the scan only")
			}
		}
	}
	targets, err := e.editTargets(ctx, r.Target)
	if err != nil {
		return tenant.Correction{}, err
	}
	if len(targets) != 1 || targets[0].SHA256 != r.OldSHA256 {
		return tenant.Correction{}, fault.Wrap(fault.Precondition, tenant.ErrTargetChanged, "the instance changed since its new revision was made")
	}
	lines, err := e.scanLines(ctx, targets)
	if err != nil {
		return tenant.Correction{}, err
	}
	t := targets[0]
	edits := []tenant.EditPrepared{{UID: t.UID, SeriesUID: t.SeriesUID, OldRevision: t.Revision, OldSHA256: t.SHA256, RestoreLine: lines[t.UID]}}
	now := e.now()
	id, err := tenant.NewCorrectionID(now)
	if err != nil {
		return tenant.Correction{}, err
	}
	issued := strconv.FormatInt(now.UnixMicro(), 10)
	sum := sha256.Sum256([]byte("nopacs-revise-1\x00" + t.UID + "\x00" + strconv.Itoa(t.Revision) + "\x00" + t.SHA256.String()))
	c, made, err := e.cfg.Store.PrepareCorrection(ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindEdit, Target: r.Target,
		Confirm: "c1." + issued + "." + hex.EncodeToString(sum[:]), Reason: r.Reason, KeyID: r.KeyID, Edits: edits, Now: now}, r.Audit, r.Guard)
	if err != nil || !made {
		return c, err
	}
	ctx, cancel := detached(ctx)
	defer cancel()
	if err := e.reached(c, "prepared"); err != nil {
		return c, err
	}
	if err := e.writeRecords(ctx, c); err != nil {
		return e.abandon(ctx, c, reasonStorage, err)
	}
	if err := e.reached(c, "records"); err != nil {
		return c, err
	}
	return e.writeEdit(ctx, c, targets, editWith{changes: r.Values, file: r.File, profile: r.Profile.String(), profileSHA256: r.ProfileSHA256, base: r.Base,
		unnamed: r.Unnamed, unnamedOver: r.UnnamedOver})
}
