package tenant

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// crockford is the alphabet of an operation id: Crockford's base 32 in
// lowercase (layout.ValidOperationID).
const crockford = "0123456789abcdefghjkmnpqrstvwxyz"

// NewCorrectionID returns a new operation id: a ULID (48 bits of
// milliseconds since 1970, then 80 random bits) in lowercase, so ids sort
// in the order they were made, and a record of one on a storage names it.
func NewCorrectionID(now time.Time) (string, error) {
	var b [16]byte
	ms := uint64(now.UnixMilli()) //nolint:gosec // G115: a time after 1970 is positive.
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	if _, err := rand.Read(b[6:]); err != nil {
		return "", fault.Wrap(fault.Internal, err, "an operation id cannot be made")
	}
	// 128 bits written as 26 digits of 5 bits, the first holding 3.
	out := make([]byte, layout.OperationIDLength)
	var acc uint64
	bits, n := 0, 0
	out[n] = crockford[b[0]>>5]
	n++
	acc, bits = uint64(b[0]&0x1f), 5
	for _, c := range b[1:] {
		acc = acc<<8 | uint64(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[n] = crockford[(acc>>uint(bits))&0x1f] //nolint:gosec // G115: bits is 0 to 12.
			n++
		}
	}
	return string(out), nil
}

// ErrTargetChanged is in the chain of the precondition failure of a
// publish whose target changed after the correction was prepared (an
// instance arrived in a scan a relabel copied, a replaced instance was
// replaced meanwhile): what it wrote would not match, and the caller
// abandons it.
var ErrTargetChanged = errors.New("the correction's target changed after it was prepared")

// StateReasonStopped is the state reason of a prepared relabel an admin
// stopped.
const StateReasonStopped = "stopped by an admin"

// ValidateUndoReason returns an Invalid error unless s is a person's reason
// to undo a correction or let its data arrive again.
func ValidateUndoReason(s string) error { return validateCorrectionReason("reason", s) }

// ValidateCorrectionID returns an Invalid error unless id is an operation id.
func ValidateCorrectionID(id string) error {
	if err := layout.ValidOperationID(id); err != nil {
		return invalid("a correction's id is a ULID in lowercase: 26 of 0-9 and a-z but i, l, o and u")
	}
	return nil
}

// validateCorrectionReason returns an Invalid error unless s is a person's
// reason for a correction: 1 to 1024 bytes of UTF-8, not only spaces, with
// no control character, since a log and a record keep it as it is.
func validateCorrectionReason(what, s string) error {
	if err := validateReason(what, s); err != nil {
		return err
	}
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return invalid("a " + what + " holds no control character")
	}
	return nil
}

// Validate returns an Invalid error unless t names a subject, a session of
// it, a scan of that, or one instance of the scan.
func (t CorrectionTarget) Validate() error {
	if !label.Valid(t.Subject) {
		return invalid("a correction's subject label passes the label rule")
	}
	if t.Session == "" && (t.Scan != "" || t.Instance != "") || t.Scan == "" && t.Instance != "" {
		return invalid("a correction's target names each level above the one it names")
	}
	for _, l := range []string{t.Session, t.Scan} {
		if l != "" && !label.Valid(l) {
			return invalid("a correction's labels pass the label rule")
		}
	}
	if t.Instance != "" {
		return ValidateInstanceUID(t.Instance)
	}
	return nil
}

// Validate returns an Invalid error unless p can be prepared.
func (p CorrectionPrepare) Validate() error {
	if err := ValidateCorrectionID(p.ID); err != nil {
		return err
	}
	if err := validateCorrectionReason("correction's reason", p.Reason); err != nil {
		return err
	}
	if err := master.ValidateTime("correction time", p.Now); err != nil {
		return err
	}
	if p.KeyID != "" {
		if err := master.ValidateID("key id", p.KeyID); err != nil {
			return err
		}
	}
	switch p.Kind {
	case record.KindDelete:
		if err := p.Target.Validate(); err != nil {
			return err
		}
		if p.ToLabel != "" || p.Quarantine != "" || p.RestoreLine != nil || !p.NewSHA256.IsZero() {
			return invalid("a delete names no new label or quarantined file")
		}
		if p.Confirm == "" || len(p.Confirm) > MaxConfirmLength {
			return invalid("a delete carries the confirm token of its preview")
		}
	case record.KindRelabel:
		if err := p.Target.Validate(); err != nil {
			return err
		}
		if p.Target.Instance != "" {
			return invalid("a relabel names a subject, a session or a scan")
		}
		if !label.Valid(p.ToLabel) {
			return invalid("a relabel's new label passes the label rule")
		}
		if p.Quarantine != "" || p.Confirm != "" || p.RestoreLine != nil || !p.NewSHA256.IsZero() {
			return invalid("a relabel names no quarantined file or confirm token")
		}
	case record.KindReplace:
		if err := master.ValidateID("quarantined file id", p.Quarantine); err != nil {
			return err
		}
		if p.NewSHA256.IsZero() || p.NewSize <= 0 || len(p.RestoreLine) == 0 {
			return invalid("a replace states the new bytes' checksum and size and the line it can write back")
		}
		if p.Target != (CorrectionTarget{}) || p.ToLabel != "" || p.Confirm != "" {
			return invalid("a replace names its quarantined file only")
		}
	case record.KindEdit:
		if err := p.Target.Validate(); err != nil {
			return err
		}
		if p.ToLabel != "" || p.Quarantine != "" || p.RestoreLine != nil || !p.NewSHA256.IsZero() {
			return invalid("an edit names no new label, quarantined file or new bytes of its own")
		}
		if p.Confirm == "" || len(p.Confirm) > MaxConfirmLength {
			return invalid("an edit carries the confirm token of its preview")
		}
		if err := validateEdits(p.Edits); err != nil {
			return err
		}
	case record.KindMove:
		// ToLabel is the subject it goes under.
		if err := ValidateMove(p.Target, p.ToLabel); err != nil {
			return err
		}
		if p.Quarantine != "" || p.RestoreLine != nil || !p.NewSHA256.IsZero() {
			return invalid("a move names no quarantined file or new bytes")
		}
		if p.Confirm == "" || len(p.Confirm) > MaxConfirmLength {
			return invalid("a move carries the confirm token of its preview")
		}
	default:
		return invalid("a correction is a delete, a relabel, a replace, an edit or a move")
	}
	if p.Kind != record.KindEdit && len(p.Edits) > 0 {
		return invalid("only an edit names edited instances")
	}
	if p.Kind != record.KindMove && p.RewriteIdentity {
		return invalid("only a move rewrites the identity of what it moves")
	}
	if b := p.Batch; b != nil {
		if p.Kind != record.KindDelete || p.Retention != nil || p.OverrideRetention || p.Target.Instance != "" {
			return invalid("a batch makes a person's delete of a subject, a session or a scan, never over retention")
		}
		if err := master.ValidateID("job id", b.JobID); err != nil {
			return err
		}
		if b.Ord < 1 || b.Ord > MaxDeleteBatchTargets {
			return invalid("a batch's target is named by its place, 1 to " + strconv.Itoa(MaxDeleteBatchTargets))
		}
	}
	return p.validateRetention()
}

// validateRetention returns an Invalid error unless what p says of
// retention is well formed: an override with its own reason, on a person's
// delete; a disposal's origin on a delete of one session.
func (p CorrectionPrepare) validateRetention() error {
	if p.OverrideRetention != (p.OverrideReason != "") {
		return invalid("override_retention carries its own reason, and a reason only comes with it")
	}
	if p.OverrideRetention {
		if p.Kind != record.KindDelete || p.Retention != nil {
			return invalid("only a person's delete overrides retention")
		}
		if err := validateCorrectionReason("override's reason", p.OverrideReason); err != nil {
			return err
		}
	}
	if r := p.Retention; r != nil {
		if p.Kind != record.KindDelete || p.Target.Level() != record.LevelSession {
			return invalid("a disposal deletes one session")
		}
		if err := ValidateName("rule", r.Rule); err != nil {
			return err
		}
		if err := master.ValidateTime("due time", r.Due); err != nil {
			return err
		}
	}
	return nil
}

// MaxConfirmLength is the longest confirm token, the contract's.
const MaxConfirmLength = 128

// DeleteConfirm returns the confirm token of a preview: the time it was
// issued, then a digest of everything the person saw, so a delete carrying
// it acts on what they saw, and a preview made later that answers
// otherwise does not match.
func DeleteConfirm(p DeletePreview) string {
	issued := strconv.FormatInt(p.Issued.UnixMicro(), 10)
	return "c1." + issued + "." + deleteDigest(p, issued)
}

// deleteDigest returns the SHA-256, in hexadecimal, of everything a delete
// preview answers, with issued as the time it was issued: DeleteConfirm's
// digest, or with "" DeleteFingerprint, which leaves the time out.
func deleteDigest(p DeletePreview, issued string) string {
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
	w("nopacs-delete-preview-4", issued, t.Subject, t.Session, t.Scan, t.Instance,
		n(c.Subjects), n(c.Sessions), n(c.Scans), n(c.Instances), n(c.Bytes), n(p.Quarantined), n(p.Receptions))
	w(n(int64(len(p.Storages))))
	w(p.Storages...)
	w(n(int64(len(p.SentTo))))
	for _, s := range p.SentTo {
		w(s.Sender, n(s.Instances), n(s.LastSend.UnixMicro()))
	}
	w(n(int64(len(p.Jobs))), n(p.JobsNotListed))
	w(p.Jobs...)
	w(n(int64(len(p.HeldAfterPurge))))
	w(p.HeldAfterPurge...)
	// The holds and what retention keeps: a hold placed, or a rule changed,
	// since the preview makes its token stale.
	w(n(int64(len(p.Holds))))
	w(p.Holds...)
	keep := ""
	if !p.KeepUntil.IsZero() {
		keep = n(p.KeepUntil.UnixMicro())
	}
	w(n(p.SessionsUnderMinimum), keep)
	// The replicas the purge removes it from: one attached or detached
	// since the preview makes its token stale. Written only when there are
	// any, so a tenant without replicas keeps the token it had.
	if len(p.Replicas) > 0 {
		w(n(int64(len(p.Replicas))))
		for _, r := range p.Replicas {
			w(r.Primary, r.Replica)
		}
	}
	// Which rows it takes, and what they hold: labels and counts alone
	// match two sessions of one size swapped by relabels, or an instance
	// replaced or edited in place.
	scans := slices.Clone(p.Scans)
	slices.SortFunc(scans, func(a, b ScanIdentity) int { return cmp.Compare(a.ID, b.ID) })
	w(n(int64(len(scans))))
	for _, s := range scans {
		w(n(s.ID), s.Manifest.String())
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// KeyedConfirm returns the token a person is shown for a preview whose
// token (DeleteConfirm) is token: its issue time and mac, a MAC of token
// under a key only the server holds. The digest itself is not shown, so a
// client that knows what a preview counts cannot make a token; the server
// makes the preview again at the issue time, checks the MAC, and prepares
// with the digest token (SplitKeyedConfirm).
func KeyedConfirm(token string, mac []byte) string {
	f := strings.Split(token, ".")
	if len(f) != 3 {
		return ""
	}
	return "c2." + f[1] + "." + hex.EncodeToString(mac)
}

// SplitKeyedConfirm returns the issue time and MAC of a token KeyedConfirm
// made, and false for a string that is not one.
func SplitKeyedConfirm(token string) (time.Time, []byte, bool) {
	f := strings.Split(token, ".")
	if len(f) != 3 || f[0] != "c2" || len(f[2]) != 64 {
		return time.Time{}, nil, false
	}
	at, ok := issuedAt(f[1])
	mac, err := hex.DecodeString(f[2])
	if !ok || err != nil {
		return time.Time{}, nil, false
	}
	return at, mac, true
}

// ConfirmIssued returns when a confirm token says it was issued, and false
// for a string that is not one.
func ConfirmIssued(token string) (time.Time, bool) {
	f := strings.Split(token, ".")
	if len(f) != 3 || f[0] != "c1" || len(f[2]) != 64 {
		return time.Time{}, false
	}
	return issuedAt(f[1])
}

// issuedAt parses a token's issue time, in microseconds since 1970.
func issuedAt(s string) (time.Time, bool) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		return time.Time{}, false
	}
	return time.UnixMicro(v).UTC(), true
}

// ValidateCorrectionFilter returns an Invalid error unless f can be
// applied.
func ValidateCorrectionFilter(f CorrectionFilter) error {
	if len(f.Kinds) > 16 || len(f.States) > 16 {
		return invalid("a filter names at most 16 kinds and 16 states")
	}
	for _, k := range f.Kinds {
		if !knownKind(k) {
			return invalid("a correction's kind is delete, relabel, replace, edit or move")
		}
	}
	for _, s := range f.States {
		if !s.Valid() {
			return invalid("a correction's state is one of prepared, committed, purging, purged, undone, abandoned")
		}
	}
	return nil
}

// knownKind reports whether k is one of the kinds of correction.
func knownKind(k record.Kind) bool {
	switch k {
	case record.KindDelete, record.KindRelabel, record.KindReplace, record.KindEdit, record.KindMove:
		return true
	}
	return false
}

// ValidateStateReason returns an Invalid error unless why can be a state
// reason.
func ValidateStateReason(why string) error {
	return validateText("state reason", why, 1, MaxStateReasonLength)
}

// Validate returns an Invalid error unless o can be recorded.
func (o PurgedObject) Validate() error {
	if err := ValidateCorrectionID(o.ID); err != nil {
		return err
	}
	if err := ValidateName("storage", o.Storage); err != nil {
		return err
	}
	if err := validateText("object key", o.Key, 1, MaxScrubKeyLength); err != nil {
		return err
	}
	if err := validateText("object version", o.Version, 0, 1024); err != nil {
		return err
	}
	if o.Size < 0 {
		return invalid("an object's size is not negative")
	}
	switch o.Disposition {
	case DispositionRemoved, DispositionHidden, DispositionRetained, DispositionUnknown:
	default:
		return invalid("a disposition is removed, hidden, retained or unknown")
	}
	return master.ValidateTime("purge time", o.Now)
}

// ValidateGrace returns an Invalid error unless g is a grace period: 0 to
// MaxGrace.
func ValidateGrace(g time.Duration) error {
	if g < 0 || g > MaxGrace {
		return invalid("a grace period is 0 to 90 days")
	}
	return nil
}

// ValidateResolve returns an Invalid error unless refs can be resolved.
func ValidateResolve(refs []InstanceRef) error {
	if len(refs) == 0 || len(refs) > MaxResolve {
		return invalid("a resolve names 1 to " + strconv.Itoa(MaxResolve) + " pairs")
	}
	for _, r := range refs {
		if err := ValidateInstanceUID(r.UID); err != nil {
			return err
		}
		if r.Revision < 1 || r.Revision > layout.MaxRevision {
			return invalid("a revision is 1 to " + strconv.Itoa(layout.MaxRevision))
		}
	}
	return nil
}

// Validate returns an Invalid error unless p can be looked up.
func (p DeletedProbe) Validate() error {
	if p.Subject != "" && !label.Valid(p.Subject) {
		return invalid("a subject label passes the label rule")
	}
	if p.StudyUID != "" {
		if err := validateUID("Study Instance UID", p.StudyUID); err != nil {
			return err
		}
	}
	if len(p.Series) > MaxDeletedProbe || len(p.Instances) > MaxDeletedProbe {
		return invalid("a probe names at most " + strconv.Itoa(MaxDeletedProbe) + " series and instances")
	}
	for _, u := range slices.Concat(p.Series, p.Instances) {
		if err := validateUID("UID", u); err != nil {
			return err
		}
	}
	return nil
}

// RevisionOfKey returns the revision a catalog's storage key names: 1 for
// the layout's own key (an empty storage key), else the key's revision.
func RevisionOfKey(storageKey string) int {
	if storageKey == "" {
		return 1
	}
	k, err := layout.Parse(storageKey)
	if err != nil {
		return 1
	}
	return k.Rev()
}

// CatalogChanged is a correction changing what the catalog shows, the
// contract's message of that name. It names its target by labels, never by
// an instance's UID.
type CatalogChanged struct {
	// Correction is the correction's id.
	Correction string `json:"correction"`
	// Kind is what it does.
	Kind record.Kind `json:"kind"`
	// State is the state it reached.
	State record.State `json:"state"`
	// SubjectLabel, SessionLabel and ScanLabel place its target.
	SubjectLabel string `json:"subject_label"`
	// SessionLabel is the session's label, empty for a whole subject.
	SessionLabel string `json:"session_label"`
	// ScanLabel is the scan's label, empty for a whole session.
	ScanLabel string `json:"scan_label"`
	// ToLabel is a relabel's new label, or the subject a move files its
	// sessions under.
	ToLabel string `json:"to_label"`
	// Instances counts the instances the change covers.
	Instances int64 `json:"instances"`
	// Revision is a replace's new revision.
	Revision int32 `json:"revision"`
	// Origin is who asked.
	Origin string `json:"origin"`
}

// EventType implements EventPayload.
func (*CatalogChanged) EventType() EventType { return EventCatalogChanged }

// validate implements EventPayload.
func (p *CatalogChanged) validate() error {
	if err := ValidateCorrectionID(p.Correction); err != nil {
		return err
	}
	if !knownKind(p.Kind) {
		return invalid("a correction's kind is delete, relabel, replace, edit or move")
	}
	if !p.State.Valid() {
		return invalid("a correction's state is one of the six")
	}
	if !label.Valid(p.SubjectLabel) {
		return invalid("a changed subject's label passes the label rule")
	}
	if p.Instances < 0 || p.Revision < 0 {
		return invalid("a change's counts are not negative")
	}
	return nil
}

// CatalogChangedEvent returns the event of c reaching its state at the time
// given.
func CatalogChangedEvent(c Correction, at time.Time) (EventRecord, error) {
	rev := int32(0)
	if c.Kind == record.KindReplace {
		rev = int32(c.NewRevision) //nolint:gosec // G115: a revision is at most layout.MaxRevision.
	}
	return NewEvent(&CatalogChanged{Correction: c.ID, Kind: c.Kind, State: c.State, SubjectLabel: c.Target.Subject,
		SessionLabel: c.Target.Session, ScanLabel: c.Target.Scan, ToLabel: c.ToLabel, Instances: c.Counts.Instances,
		Revision: rev, Origin: c.Origin}, at)
}
