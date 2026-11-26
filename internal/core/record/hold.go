package record

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
)

// HoldKind is the kind every hold record names, the first field after its
// format, so a reader tells a hold record from a correction record by its
// content; their MACs are kept apart by their keys (auth.go).
const HoldKind = "hold"

// LevelTenant is the level of a hold of the whole tenant; a hold of a
// subject, a session or a scan names LevelSubject, LevelSession or
// LevelScan.
const LevelTenant = "tenant"

// The states of a hold record.
const (
	// HoldActive is a hold that stops every delete, purge and disposal of
	// what it covers.
	HoldActive = "active"
	// HoldReleased is a hold a person released, or one that was never
	// placed: its record was written before its row and the row was not
	// made.
	HoldReleased = "released"
)

// Hold is the record of one hold on one storage, so a hold outlives a lost
// database: a rebuild, a restore or a start that finds one active with no
// row in the tenant's database makes it an active hold again. It names what
// the hold covers by the labels the catalog has now (a relabel rewrites
// it), and never carries the reason: only its SHA-256, so the reason stays
// in the tenant's database and its audit entry.
type Hold struct {
	// Format is Format.
	Format int `json:"format"`
	// Kind is HoldKind.
	Kind string `json:"kind"`
	// ID is the hold's id, an operation id (layout.ValidOperationID).
	ID string `json:"id"`
	// Tenant is the tenant's id, which the MAC binds too.
	Tenant string `json:"tenant"`
	// Level is LevelTenant, LevelSubject, LevelSession or LevelScan.
	Level string `json:"level"`
	// Subject, Session and Scan are the labels down to Level; empty below
	// it, and all empty for the whole tenant.
	Subject string `json:"subject,omitempty"`
	Session string `json:"session,omitempty"`
	Scan    string `json:"scan,omitempty"`
	// State is HoldActive or HoldReleased.
	State string `json:"state"`
	// ReasonSHA256 is the SHA-256 of the reason text, in lowercase hex
	// (ReasonDigest).
	ReasonSHA256 string `json:"reason_sha256"`
	// PlacedBy is the id of the key that placed it; empty for the local
	// operator.
	PlacedBy string `json:"placed_by,omitempty"`
	// Placed is when it was placed; Released when it was released, zero
	// while active.
	Placed   time.Time `json:"placed"`
	Released time.Time `json:"released,omitzero"`
	// Seq counts the hold's changes (placed, relabeled, released) as its
	// row does: of two records of a hold, the one of the higher count is
	// the later.
	Seq int64 `json:"seq"`
	// KeyVersion is the version of the installation key the MAC's key is
	// derived from.
	KeyVersion uint32 `json:"mac_key_version,omitempty"`
	// MAC is the record's HMAC-SHA-256 in lowercase hex (auth.go), set by
	// EncodeHold and checked by DecodeHold; the last field.
	MAC string `json:"mac,omitempty"`
}

// Key returns the key of the record on a storage.
func (h Hold) Key() (string, error) { return layout.HoldKey(h.ID) }

// Active reports whether the record is of a hold that holds.
func (h Hold) Active() bool { return h.State == HoldActive }

// Validate returns an error wrapping ErrFormat unless h is a record this
// package writes, its MAC aside.
func (h Hold) Validate() error {
	if h.Format != Format {
		return bad("format %d is not %d", h.Format, Format)
	}
	if h.Kind != HoldKind {
		return bad("kind is %s", HoldKind)
	}
	if err := layout.ValidOperationID(h.ID); err != nil {
		return bad("id: %v", err)
	}
	if !validID(h.Tenant) {
		return bad("tenant is 1 to %d of A-Z, a-z, 0-9, _ and -", maxIDLength)
	}
	if err := h.validTarget(); err != nil {
		return err
	}
	if !lowerHex64(h.ReasonSHA256) {
		return bad("reason_sha256 is 64 lowercase hex digits")
	}
	if h.PlacedBy != "" && !validID(h.PlacedBy) {
		return bad("placed_by is 1 to %d of A-Z, a-z, 0-9, _ and -", maxIDLength)
	}
	if !validTime(h.Placed) {
		return bad("placed is a time in UTC")
	}
	switch h.State {
	case HoldActive:
		if !h.Released.IsZero() {
			return bad("an active hold has no released time")
		}
	case HoldReleased:
		if !validTime(h.Released) || h.Released.Before(h.Placed) {
			return bad("a released hold has a released time in UTC, not before placed")
		}
	default:
		return bad("state is %s or %s", HoldActive, HoldReleased)
	}
	if h.Seq < 1 || h.Seq > MaxGeneration {
		return bad("seq is 1 to %d", int64(MaxGeneration))
	}
	if h.MAC != "" && !lowerHex64(h.MAC) {
		return bad("mac is 64 lowercase hex digits")
	}
	return nil
}

// validTarget checks the level and the labels it names.
func (h Hold) validTarget() error {
	want := 0
	switch h.Level {
	case LevelTenant:
	case LevelSubject:
		want = 1
	case LevelSession:
		want = 2
	case LevelScan:
		want = 3
	default:
		return bad("level is tenant, subject, session or scan")
	}
	for i, l := range []string{h.Subject, h.Session, h.Scan} {
		switch {
		case i < want && !label.Valid(l):
			return bad("a hold names a valid label at each level down to its own")
		case i >= want && l != "":
			return bad("a hold names no label below its level")
		}
	}
	return nil
}

// EncodeHold returns the record's bytes, its Tenant set to tenantID and
// authenticated for that tenant under keys' seal version (auth.go), or an
// error wrapping ErrFormat if it is not valid. Times are written in UTC.
// Whatever KeyVersion and MAC h holds are replaced. No keys is an error: a
// record is never written unauthenticated.
func EncodeHold(h Hold, tenantID string, keys Keyring) ([]byte, error) {
	if err := checkAuth(tenantID, keys); err != nil {
		return nil, err
	}
	h.Tenant, h.Placed, h.Released = tenantID, h.Placed.UTC(), h.Released.UTC()
	h.KeyVersion, h.MAC = keys.SealVersion(), ""
	if err := h.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(h)
	if err != nil {
		return nil, bad("it cannot be encoded")
	}
	if h.MAC, err = sealBody(keys.HoldMAC, h.KeyVersion, tenantID, h.ID, body); err != nil {
		return nil, err
	}
	b, err := json.Marshal(h)
	if err != nil {
		return nil, bad("it cannot be encoded")
	}
	return append(b, '\n'), nil
}

// DecodeHold reads one hold record from r, which must hold nothing after
// it, within MaxBytes, and checks that keys authenticate it for the tenant
// whose id is tenantID. A record not in the form is an error wrapping
// ErrFormat; one in the form that does not verify, or names another tenant,
// is an error wrapping ErrUnauthenticated, and its content is not returned,
// so no caller adopts it by mistake. No keys, or no tenant id, is an error
// of neither kind: verification is never skipped.
func DecodeHold(r io.Reader, tenantID string, keys Keyring) (Hold, error) {
	if err := checkAuth(tenantID, keys); err != nil {
		return Hold{}, err
	}
	var h Hold
	if err := decode(r, &h); err != nil {
		return Hold{}, err
	}
	if err := h.Validate(); err != nil {
		return Hold{}, err
	}
	if h.Tenant != tenantID {
		return Hold{}, fmt.Errorf("%w: it is of another tenant", ErrUnauthenticated)
	}
	mac := h.MAC
	h.MAC = ""
	body, err := json.Marshal(h)
	if err != nil {
		return Hold{}, bad("it cannot be encoded")
	}
	if err := checkSeal(keys.HoldMAC, h.KeyVersion, mac, tenantID, h.ID, body); err != nil {
		return Hold{}, err
	}
	h.MAC = mac
	return h, nil
}
