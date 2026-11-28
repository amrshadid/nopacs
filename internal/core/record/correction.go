package record

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/uid"
)

// Format is the version of both records' form.
const Format = 1

// The bounds of a record, which keep what a hostile or damaged storage
// object can make a reader hold.
const (
	// MaxBytes is the most bytes a record is read from.
	MaxBytes = 16 << 20
	// MaxScans is the most scans a correction record names.
	MaxScans = 100000
	// MaxInstances is the most instance UIDs an instance delete's record
	// names.
	MaxInstances = 100000
	// MaxEdits is the most instances an edit's record names: the most one
	// edit changes (tenant.MaxEditInstances), with room to spare.
	MaxEdits = 10000
)

// ErrFormat is returned, wrapped, for a record that is not in the form this
// package writes or that breaks one of its rules.
var ErrFormat = errors.New("record: not a valid record")

// Kind is what a correction does.
type Kind string

// The kinds of correction.
const (
	// KindDelete hides and, after its grace, removes a subject, session,
	// scan or instance.
	KindDelete Kind = "delete"
	// KindRelabel gives a subject, session or scan another label.
	KindRelabel Kind = "relabel"
	// KindReplace swaps one instance's bytes for a later revision.
	KindReplace Kind = "replace"
	// KindEdit gives each instance of a subject, session, scan or instance
	// a later revision whose named attributes were set, changed or removed.
	KindEdit Kind = "edit"
)

// State is how far a correction got. It only moves forward: prepared to
// committed or abandoned, committed to purging or (a delete or an edit)
// undone, purging to purged.
type State string

// The states of a correction.
const (
	// StatePrepared is decided and recorded; nothing is hidden yet.
	StatePrepared State = "prepared"
	// StateCommitted is published: the catalog shows the new state.
	StateCommitted State = "committed"
	// StatePurging has begun removing what the correction replaced.
	StatePurging State = "purging"
	// StatePurged is finished; its record is kept for good.
	StatePurged State = "purged"
	// StateUndone is a delete or an edit taken back before its purge began.
	StateUndone State = "undone"
	// StateAbandoned is a prepared correction recovery rolled back.
	StateAbandoned State = "abandoned"
)

// Valid reports whether s is one of the six states.
func (s State) Valid() bool {
	switch s {
	case StatePrepared, StateCommitted, StatePurging, StatePurged, StateUndone, StateAbandoned:
		return true
	}
	return false
}

// Published reports whether the state is one in which the correction was
// published: committed, purging or purged.
func (s State) Published() bool {
	return s == StateCommitted || s == StatePurging || s == StatePurged
}

// CanFollow reports whether a correction of that kind may go from state s to
// next: the forward moves only, and undone only for a delete, an edit or a
// move, whose superseded revisions or old tree are kept until its purge.
func (s State) CanFollow(next State, kind Kind) bool {
	switch s {
	case StatePrepared:
		return next == StateCommitted || next == StateAbandoned
	case StateCommitted:
		return next == StatePurging || (next == StateUndone && (kind == KindDelete || kind == KindEdit || kind == KindMove))
	case StatePurging:
		return next == StatePurged
	}
	return false
}

// Rank returns how far along the state machine s lies: 0 for prepared, 1
// for committed and abandoned, 2 for purging and undone, 3 for purged; -1
// for a state that is not one of the six. A state is reached only from
// states of a lower rank.
func (s State) Rank() int {
	switch s {
	case StatePrepared:
		return 0
	case StateCommitted, StateAbandoned:
		return 1
	case StatePurging, StateUndone:
		return 2
	case StatePurged:
		return 3
	}
	return -1
}

// After reports whether s is reached from prev by one or more forward moves
// of a correction of that kind (CanFollow): a record on a storage whose
// state is After its database row's is ahead of the row, which a crash
// between writing the record and committing the row leaves, and the row is
// moved forward to it; one that is not After and not equal is behind, and
// is written again from the row.
func (s State) After(prev State, kind Kind) bool {
	if s.Rank() <= prev.Rank() || prev.Rank() < 0 {
		return false
	}
	for _, next := range []State{StateCommitted, StateAbandoned, StatePurging, StateUndone, StatePurged} {
		if prev.CanFollow(next, kind) && (next == s || s.After(next, kind)) {
			return true
		}
	}
	return false
}

// The levels a correction targets: Correction.Level for a delete,
// Relabel.Level for a relabel.
const (
	LevelSubject  = "subject"
	LevelSession  = "session"
	LevelScan     = "scan"
	LevelInstance = "instance"
)

// Scan names a scan a correction touches, by the labels it had when the
// correction was made, its Series Instance UID and its session's Study
// Instance UID, which never change: a restore that applies the record
// knows by them which arrivals are of what the correction removed.
type Scan struct {
	// Subject, Session and Scan are the labels.
	Subject string `json:"subject"`
	Session string `json:"session"`
	Scan    string `json:"scan"`
	// StudyUID is the Study Instance UID of the scan's session.
	StudyUID string `json:"study_uid"`
	// SeriesUID is the scan's Series Instance UID.
	SeriesUID string `json:"series_uid"`
}

// Relabel says which label a relabel changes.
type Relabel struct {
	// Level is LevelSubject, LevelSession or LevelScan.
	Level string `json:"level"`
	// From and To are the label before and after at that level.
	From string `json:"from"`
	To   string `json:"to"`
}

// Replacement says which instance a replace changes and how.
type Replacement struct {
	// SOPInstanceUID names the instance.
	SOPInstanceUID string `json:"sop_instance_uid"`
	// OldRevision and NewRevision are the revisions before and after.
	OldRevision int `json:"old_revision"`
	NewRevision int `json:"new_revision"`
	// OldSHA256 and NewSHA256 are the checksums of the two revisions'
	// bytes, in lowercase hex. An edit's NewSHA256 is empty until it is
	// published: its new revisions are written after its record.
	OldSHA256 string `json:"old_sha256"`
	NewSHA256 string `json:"new_sha256"`
	// SeriesUID is, for an edit, the Series Instance UID of the instance's
	// scan, one of the record's; empty for a replace, which is of one scan.
	SeriesUID string `json:"series_uid,omitempty"`
}

// Purge sums what a purge did on this storage, by what the storage said of
// each object it was asked to remove.
type Purge struct {
	// Removed counts objects whose bytes are gone from this storage.
	Removed int64 `json:"removed"`
	// Hidden counts objects a versioned bucket no longer lists while it
	// keeps older versions.
	Hidden int64 `json:"hidden"`
	// Retained counts objects the storage's own retention refused to remove.
	Retained int64 `json:"retained"`
	// Unknown counts objects whose outcome the storage did not say.
	Unknown int64 `json:"unknown"`
	// RetainUntil is when the storage's retention ends, when it said.
	RetainUntil time.Time `json:"retain_until,omitzero"`
}

// Correction is the record of one correction on one storage.
type Correction struct {
	// Format is Format.
	Format int `json:"format"`
	// ID is the operation id, a ULID in lowercase (layout.ValidOperationID).
	ID string `json:"id"`
	// Kind says what the correction does.
	Kind Kind `json:"kind"`
	// Level is what a delete removes, or what an edit names: LevelSubject,
	// LevelSession, LevelScan or LevelInstance. Empty for a relabel or a
	// move, whose level is its Relabel's or Move's, and for a replace, which
	// is of one instance.
	Level string `json:"level,omitempty"`
	// State says how far it got.
	State State `json:"state"`
	// ReasonSHA256 is the SHA-256 of the reason text, in lowercase hex.
	ReasonSHA256 string `json:"reason_sha256"`
	// Created is when the correction was made.
	Created time.Time `json:"created"`
	// CommittedAt is when it was published; zero until then. The grace runs
	// from it.
	CommittedAt time.Time `json:"committed_at,omitzero"`
	// PurgeAfter is when what it replaced may be removed, fixed when it was
	// published.
	PurgeAfter time.Time `json:"purge_after,omitzero"`
	// Scans are the scans on this storage the correction touches.
	Scans []Scan `json:"scans"`
	// Instances are the SOP Instance UIDs an instance delete removes, in
	// byte order; empty for a delete of a whole scan or more.
	Instances []string `json:"instances,omitempty"`
	// Relabel is set for a relabel.
	Relabel *Relabel `json:"relabel,omitempty"`
	// Replace is set for a replace.
	Replace *Replacement `json:"replace,omitempty"`
	// Move is set for a move.
	Move *Move `json:"move,omitempty"`
	// Edits are, for an edit, the instances of the record's scans it gives a
	// new revision, in byte order of SOP Instance UID.
	Edits []Replacement `json:"edits,omitempty"`
	// Purge is what the purge did here, set once it has begun.
	Purge *Purge `json:"purge,omitempty"`
	// KeyVersion is the version of the installation key the MAC's key is
	// derived from, so a record written before a newer version was added
	// still verifies.
	KeyVersion uint32 `json:"mac_key_version,omitempty"`
	// MAC is the record's HMAC-SHA-256 in lowercase hex (auth.go), set by
	// EncodeCorrection and checked by DecodeCorrection. It is the last
	// field, so the record's bytes without it are the bytes it covers.
	MAC string `json:"mac,omitempty"`
}

// ReasonDigest returns the SHA-256 of a reason, as a record holds it.
func ReasonDigest(reason string) string {
	sum := sha256.Sum256([]byte(reason))
	return hex.EncodeToString(sum[:])
}

// Key returns the key of the record on a storage.
func (c Correction) Key() (string, error) { return layout.CorrectionKey(c.ID) }

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrFormat, fmt.Sprintf(format, a...))
}

// validTime reports whether t is set and in UTC.
func validTime(t time.Time) bool {
	_, off := t.Zone()
	return !t.IsZero() && off == 0
}

// lowerHex64 reports whether s is 64 lowercase hex digits.
func lowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Validate returns an error wrapping ErrFormat unless c is a record this
// package writes.
func (c Correction) Validate() error {
	if c.Format != Format {
		return bad("format %d is not %d", c.Format, Format)
	}
	if err := layout.ValidOperationID(c.ID); err != nil {
		return bad("id: %v", err)
	}
	switch c.Kind {
	case KindDelete, KindRelabel, KindReplace, KindEdit, KindMove:
	default:
		return bad("kind is delete, relabel, replace, edit or move")
	}
	if !c.State.Valid() {
		return bad("state is not one of the six")
	}
	if c.State == StateUndone && c.Kind != KindDelete && c.Kind != KindEdit && c.Kind != KindMove {
		return bad("only a delete, an edit or a move is undone")
	}
	if !lowerHex64(c.ReasonSHA256) {
		return bad("reason_sha256 is 64 lowercase hex digits")
	}
	if !validTime(c.Created) {
		return bad("created is a time in UTC")
	}
	if err := c.validTimes(); err != nil {
		return err
	}
	if len(c.Scans) == 0 || len(c.Scans) > MaxScans {
		return bad("a correction names 1 to %d scans", MaxScans)
	}
	if err := c.validScans(); err != nil {
		return err
	}
	if err := c.validTarget(); err != nil {
		return err
	}
	return c.validPurge()
}

// validTimes checks the times against the state.
func (c Correction) validTimes() error {
	if c.State.Published() || c.State == StateUndone {
		if !validTime(c.CommittedAt) || !validTime(c.PurgeAfter) || c.PurgeAfter.Before(c.CommittedAt) || c.CommittedAt.Before(c.Created) {
			return bad("a published correction has committed_at and purge_after in UTC, in order after created")
		}
		return nil
	}
	if !c.CommittedAt.IsZero() || !c.PurgeAfter.IsZero() {
		return bad("a correction that was not published has no committed_at or purge_after")
	}
	return nil
}

// validScans checks each scan's labels and UIDs, and that they name one
// archive: each series once, each scan label once in its session, and one
// study per session and one session per study.
func (c Correction) validScans() error {
	series := make(map[string]bool, len(c.Scans))
	scans := make(map[label.Path]bool, len(c.Scans))
	studyOf := map[[2]string]string{}
	sessionOf := map[string][2]string{}
	for _, s := range c.Scans {
		p := label.Path{Subject: s.Subject, Session: s.Session, Scan: s.Scan}
		if err := p.Check(); err != nil {
			return bad("a scan's labels: %v", err)
		}
		if err := uid.Check(s.StudyUID); err != nil {
			return bad("study_uid: %v", err)
		}
		if err := uid.Check(s.SeriesUID); err != nil {
			return bad("series_uid: %v", err)
		}
		if series[s.SeriesUID] || scans[p] {
			return bad("a correction names each scan once")
		}
		series[s.SeriesUID], scans[p] = true, true
		session := [2]string{s.Subject, s.Session}
		if study, ok := studyOf[session]; ok && study != s.StudyUID {
			return bad("a session has one study_uid")
		}
		if other, ok := sessionOf[s.StudyUID]; ok && other != session {
			return bad("a study is one session")
		}
		studyOf[session], sessionOf[s.StudyUID] = s.StudyUID, session
	}
	return nil
}

// within checks that the scans lie within one target at level: one subject,
// one session, or one scan (for LevelScan and LevelInstance).
func (c Correction) within(level string) error {
	first := c.Scans[0]
	for _, s := range c.Scans[1:] {
		switch {
		case s.Subject != first.Subject:
			return bad("a correction at the %s level is of one subject", level)
		case level != LevelSubject && s.Session != first.Session:
			return bad("a correction at the %s level is of one session", level)
		case level == LevelScan || level == LevelInstance:
			return bad("a correction at the %s level is of one scan", level)
		}
	}
	return nil
}

// labelAt returns the scan's label at level.
func (s Scan) labelAt(level string) string {
	switch level {
	case LevelSubject:
		return s.Subject
	case LevelSession:
		return s.Session
	}
	return s.Scan
}

// validTarget checks what the kind needs and refuses what it does not.
func (c Correction) validTarget() error {
	switch c.Kind {
	case KindDelete:
		if c.Relabel != nil || c.Replace != nil || c.Move != nil || len(c.Edits) > 0 {
			return bad("a delete has no relabel, replace, move or edits")
		}
		switch c.Level {
		case LevelSubject, LevelSession, LevelScan, LevelInstance:
		default:
			return bad("a delete's level is subject, session, scan or instance")
		}
		if (c.Level == LevelInstance) != (len(c.Instances) > 0) {
			return bad("an instance delete, and only one, names its instances")
		}
		if err := c.within(c.Level); err != nil {
			return err
		}
		if len(c.Instances) > MaxInstances {
			return bad("an instance delete names at most %d instances", MaxInstances)
		}
		if len(c.Instances) > 0 && len(c.Scans) != 1 {
			return bad("an instance delete is in one scan")
		}
		for i, u := range c.Instances {
			if err := uid.Check(u); err != nil {
				return bad("an instance's UID: %v", err)
			}
			if i > 0 && u <= c.Instances[i-1] {
				return bad("instances are in byte order, each once")
			}
		}
	case KindRelabel:
		r := c.Relabel
		if r == nil || c.Replace != nil || c.Move != nil || len(c.Edits) > 0 || len(c.Instances) > 0 || c.Level != "" {
			return bad("a relabel has its relabel and nothing of a replace, an edit or a delete")
		}
		if r.Level != LevelSubject && r.Level != LevelSession && r.Level != LevelScan {
			return bad("a relabel's level is subject, session or scan")
		}
		if !label.Valid(r.From) || !label.Valid(r.To) || r.From == r.To {
			return bad("a relabel changes one label to another valid one")
		}
		if err := c.within(r.Level); err != nil {
			return err
		}
		if c.Scans[0].labelAt(r.Level) != r.From {
			return bad("a relabel's from is its scans' label at its level")
		}
	case KindReplace:
		r := c.Replace
		if r == nil || c.Relabel != nil || c.Move != nil || len(c.Edits) > 0 || len(c.Instances) > 0 || len(c.Scans) != 1 || c.Level != "" ||
			r.SeriesUID != "" {
			return bad("a replace has its replace, one scan, and nothing else")
		}
		if err := uid.Check(r.SOPInstanceUID); err != nil {
			return bad("replace: %v", err)
		}
		// The new revision is above the old one, not always the next: a
		// revision an undone edit numbered is never numbered again, so its
		// key never names two files.
		if r.OldRevision < 1 || r.NewRevision <= r.OldRevision || r.NewRevision > layout.MaxRevision {
			return bad("a replace adds a later revision, within 1 to %d", layout.MaxRevision)
		}
		// The two checksums may be equal: a replace that repairs an
		// instance a scrub found damaged puts the bytes it was archived
		// with back, at a key of their own.
		if !lowerHex64(r.OldSHA256) || !lowerHex64(r.NewSHA256) {
			return bad("a replace's two checksums are 64 lowercase hex digits")
		}
	case KindEdit:
		return c.validEdits()
	case KindMove:
		return c.validMove()
	}
	return nil
}

// validPurge checks the purge summary against the state.
func (c Correction) validPurge() error {
	p := c.Purge
	if p == nil {
		if c.State == StatePurged {
			return bad("a purged correction says what the purge did")
		}
		return nil
	}
	if c.State != StatePurging && c.State != StatePurged {
		return bad("only a purging or purged correction has a purge summary")
	}
	if p.Removed < 0 || p.Hidden < 0 || p.Retained < 0 || p.Unknown < 0 {
		return bad("a purge's counts are not negative")
	}
	if !p.RetainUntil.IsZero() && !validTime(p.RetainUntil) {
		return bad("retain_until is a time in UTC")
	}
	return nil
}

// EncodeCorrection returns the record's bytes, authenticated for the tenant
// whose id is tenantID under keys' seal version (auth.go), or an error
// wrapping ErrFormat if it is not valid. Times are written in UTC. Whatever
// KeyVersion and MAC c holds are replaced.
func EncodeCorrection(c Correction, tenantID string, keys Keyring) ([]byte, error) {
	if err := checkAuth(tenantID, keys); err != nil {
		return nil, err
	}
	c.Created, c.CommittedAt, c.PurgeAfter = c.Created.UTC(), c.CommittedAt.UTC(), c.PurgeAfter.UTC()
	if c.Purge != nil {
		p := *c.Purge
		p.RetainUntil = p.RetainUntil.UTC()
		c.Purge = &p
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	c, err := sign(c, tenantID, keys)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, bad("it cannot be encoded")
	}
	if len(b) > MaxBytes {
		return nil, bad("it is longer than %d bytes", MaxBytes)
	}
	return append(b, '\n'), nil
}

// DecodeCorrection reads one correction record from r, which must hold
// nothing after it, within MaxBytes, and checks that keys authenticate it
// for the tenant whose id is tenantID. A record not in the form is an error
// wrapping ErrFormat; one in the form that does not verify (auth.go) is an
// error wrapping ErrUnauthenticated, and its content is not returned, so no
// caller can apply it by mistake. No keys, or no tenant id, is an error of
// neither kind: verification is never skipped.
func DecodeCorrection(r io.Reader, tenantID string, keys Keyring) (Correction, error) {
	if err := checkAuth(tenantID, keys); err != nil {
		return Correction{}, err
	}
	var c Correction
	if err := decode(r, &c); err != nil {
		return Correction{}, err
	}
	if err := c.Validate(); err != nil {
		return Correction{}, err
	}
	if err := verify(c, tenantID, keys); err != nil {
		return Correction{}, err
	}
	return c, nil
}

// decode reads exactly one JSON value into v, refusing unknown fields and
// anything after it.
func decode(r io.Reader, v any) error {
	b, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return fmt.Errorf("record: read: %w", err)
	}
	if len(b) > MaxBytes {
		return bad("it is longer than %d bytes", MaxBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return bad("it is not JSON of the record's form")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return bad("bytes after the record")
	}
	return nil
}
