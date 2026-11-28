package deidjob

import (
	"context"
	"errors"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A profile re-run runs a newer profile version over archived files that an
// older version de-identified (receive.TightenFile) and gives each file it
// changes a new revision through the corrections' own path (Reviser). The
// job's units, cursors, pauses and cancels are those of every deid job, so
// a restart goes on after the last batch written; an instance revised but
// not yet recorded is met again at its new revision, which already names
// the newer version, and is skipped as unchanged, never revised twice.

// The words of a re-run's tallies and refusals.
const (
	tallyRerun          = "rerun"
	detailHeldByHold    = "held_by_hold"
	detailChangedSince  = "changed_since_judged"
	detailRevisionNoWay = "revision_refused"
	detailKeyRefused    = "key_refused"
	detailPixelRules    = "deid_failed/" + receive.TightenPixelRules
)

// Revision is a new revision of one archived instance a re-run asks to
// publish: made and checked, and about to be written.
type Revision struct {
	// JobID and KeyID name the job and the key that made it, which the
	// publish is audited and guarded by.
	JobID, KeyID string
	// Target names the instance: its scan's labels and its UID.
	Target tenant.CorrectionTarget
	// OldSHA256 is the archived checksum of the revision File was made from.
	OldSHA256 tenant.SHA256
	// File is the new revision's bytes; Profile the version that made it.
	File    []byte
	Profile tenant.ProfileRef
	// Catalog are the scan's catalog values the file changes.
	Catalog []receive.CatalogValue
	// ProfileSHA256 is the checksum of the newer version's expanded text and
	// Base the tenant's base profile in force, "name@version", empty for
	// none; Unnamed and UnnamedOver the elements no rule of the newer
	// version named: what the new revision's line says of how it was made.
	ProfileSHA256, Base string
	Unnamed             []sidecar.UnnamedElement
	UnnamedOver         int
}

// Reviser publishes the new revisions of a re-run: the server's is the
// corrections engine's (correct.Engine.Revise), which refuses an instance a
// hold covers, one another correction is working on and one that changed
// since the file was made.
type Reviser interface {
	// Revise publishes r. A hold is answered with an error that is
	// tenant.ErrHeld, a change since with tenant.ErrTargetChanged or
	// tenant.ErrOverlap, and a key that may no longer ask with
	// ErrKeyRefused; an error that is none of these and a temporary kind
	// (unavailable, exhausted, internal) is tried again later.
	Revise(ctx context.Context, r Revision) error
}

// ErrKeyRefused is a Reviser's answer for the key that made the job when it
// may no longer change the tenant: revoked, expired or no longer an admin.
var ErrKeyRefused = errors.New("deidjob: the key that made the re-run may no longer change the tenant")

// rerunState is what a unit of a re-run holds besides a dry run's.
type rerunState struct {
	reviser Reviser
	jobID   string
	keyID   string
	scan    label.Path
	// loosening caches, by the pair of versions, the problem code that
	// refuses the newer over the older, "" for none.
	loosening map[[2]tenant.ProfileRef]string
}

// loosens returns the code of the problem that refuses stacking the newer
// version over the older as a layer (profile.Stack: a rule that lets
// through more than the older decides, or that cannot be compared with it),
// and "" when the newer is at least as strict everywhere. A looser version
// would need the original, which de-identified data does not keep.
func (j *judge) loosens(older, newer tenant.ProfileRef) string {
	key := [2]tenant.ProfileRef{older, newer}
	if c, ok := j.rr.loosening[key]; ok {
		return c
	}
	code := ""
	_, problems := profile.Stack(
		profile.Layer{Kind: profile.LayerTenant, Name: older.String(), Expanded: j.expanded[older]},
		profile.Layer{Kind: profile.LayerReceiver, Name: newer.String(), Expanded: j.expanded[newer]})
	if len(problems) > 0 {
		code = string(problems[0].Code)
	}
	j.rr.loosening[key] = code
	return code
}

// rerun judges one archived file of a re-run, and for a re-run proper
// publishes its new revision. An error is a cause that is not the file's.
func (j *judge) rerun(ctx context.Context, in tenant.ScanInstance, file []byte, facts receptionFacts, v *verdict) error {
	refuse := func(detail string, tally ...string) {
		v.kind, v.code, v.detail = resultRefused, tenant.FailureWouldHold, detail
		v.tallies[tallyKey(append([]string{tallyHold}, tally...)...)]++
	}
	older, newer := in.Profile, j.profile
	switch {
	case older.IsZero():
		v.kind, v.code, v.detail = resultNoted, tenant.FailureNotDeidentified, detailAsReceived
		v.tallies[tallyAsReceived]++
		return nil
	case older == newer:
		v.kind = resultPassed
		v.tallies[tallyKey(tallyRerun, "unchanged")]++
		return nil
	}
	newEn, err := j.engineFor(ctx, newer)
	if err != nil {
		return err
	}
	oldEn, err := j.engineFor(ctx, older)
	if err != nil {
		return err
	}
	if newEn == nil || oldEn == nil {
		refuse(detailUnusable, "deid_failed", "profile_unusable")
		return nil
	}
	if len(j.expanded[newer].GetPixelRules()) > 0 {
		refuse(detailPixelRules, "deid_failed", receive.TightenPixelRules)
		return nil
	}
	if code := j.loosens(older, newer); code != "" {
		refuse(code, "layer", code)
		return nil
	}
	newCheck := func(maxFileBytes int64) receive.Checker { return check.New(check.Options{MaxFileBytes: maxFileBytes}) }
	t, err := receive.TightenFile(ctx, newEn, oldEn, file, facts.rec, receive.TightenOptions{HeaderLimit: j.headerLimit, Check: newCheck,
		Memory: j.memory})
	if err != nil {
		return fault.New(fault.KindOf(err), fault.Message(err))
	}
	switch {
	case t.Hold != nil:
		cause := holdCause(t.Hold)
		refuse(string(t.Hold.Reason)+"/"+cause, string(t.Hold.Reason), cause)
		return nil
	case len(t.Changes) == 0:
		v.kind = resultPassed
		v.tallies[tallyKey(tallyRerun, "unchanged")]++
		return nil
	}
	for _, c := range t.Changes {
		v.tallies[tallyKey(tallyRerun, "element", tagWord(c.Tag), c.Action)]++
	}
	v.kind = resultPassed
	if j.mode == tenant.DeidRerunPreview {
		v.tallies[tallyKey(tallyRerun, "would_change")]++
		return nil
	}
	err = j.rr.reviser.Revise(ctx, Revision{JobID: j.rr.jobID, KeyID: j.rr.keyID,
		Target:    tenant.CorrectionTarget{Subject: j.rr.scan.Subject, Session: j.rr.scan.Session, Scan: j.rr.scan.Scan, Instance: in.SOPInstanceUID},
		OldSHA256: archivedSum(in), File: t.Written, Profile: newer, Catalog: t.Catalog,
		ProfileSHA256: j.sums[newer], Base: baseText(j.base), Unnamed: t.Unnamed, UnnamedOver: t.UnnamedOver})
	switch {
	case err == nil:
		v.tallies[tallyKey(tallyRerun, "revised")]++
	case errors.Is(err, tenant.ErrHeld):
		refuse(detailHeldByHold, "skipped", "held")
	case errors.Is(err, ErrKeyRefused):
		refuse(detailKeyRefused, "skipped", "key_refused")
	case errors.Is(err, tenant.ErrTargetChanged), errors.Is(err, tenant.ErrOverlap):
		v.kind, v.code, v.detail = resultNoted, tenant.FailureUndecided, detailChangedSince
		v.tallies[tallyKey(tallyRerun, "skipped", "changed")]++
	case ctx.Err() != nil, fault.KindOf(err) == fault.Unavailable, fault.KindOf(err) == fault.Exhausted, fault.KindOf(err) == fault.Internal:
		return err
	default:
		refuse(detailRevisionNoWay, "skipped", "refused")
	}
	return nil
}

// tagWord writes a tag as eight upper-case hexadecimal digits, as a tally
// names an element.
func tagWord(t uint32) string {
	const digits = "0123456789ABCDEF"
	b := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		b[i] = digits[t&0xf]
		t >>= 4
	}
	return string(b)
}

// expandedOf is the expanded text of a version compiled for a unit, kept so
// the layer rule can be run over two versions.
type expandedOf map[tenant.ProfileRef]*v1.ExpandedProfile

// archivedSum is the checksum of the revision an instance holds now: what
// was archived, or what was received when the two are the same file.
func archivedSum(in tenant.ScanInstance) tenant.SHA256 {
	if !in.ArchivedSHA256.IsZero() {
		return in.ArchivedSHA256
	}
	return in.SHA256
}

// baseText writes the tenant's base profile as a line records it: its
// "name@version", or nothing when the tenant has none.
func baseText(b tenant.ProfileRef) string {
	if b.IsZero() {
		return ""
	}
	return b.String()
}
