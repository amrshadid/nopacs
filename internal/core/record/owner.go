package record

import (
	"crypto/hmac"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
)

// MaxGeneration is the greatest generation an owner record holds: below
// 2^53, so a JSON number reads back exactly in every tool.
const MaxGeneration = 1<<53 - 1

// maxIDLength is the longest installation or tenant id.
const maxIDLength = 64

// Owner is the owner record of a tenant on one storage: which installation,
// at which generation, may write the tenant's objects there.
type Owner struct {
	// Format is Format.
	Format int `json:"format"`
	// Installation is the id of the installation that owns the tenant's
	// objects.
	Installation string `json:"installation"`
	// Tenant is the tenant's id.
	Tenant string `json:"tenant"`
	// Generation counts the hand-offs and restores of the tenant, from 1;
	// the greater generation wins.
	Generation int64 `json:"generation"`
	// Written is when the record was written.
	Written time.Time `json:"written"`
	// KeyVersion is the version of the writer's installation key the MAC's
	// key is derived from; 0 in a record with no MAC.
	KeyVersion uint32 `json:"mac_key_version,omitempty"`
	// MAC is the record's HMAC-SHA-256 in lowercase hex, set by EncodeOwner
	// and checked by VerifyOwner; written last.
	MAC string `json:"mac,omitempty"`
}

// Key is the key of the owner record on a storage.
func (Owner) Key() string { return layout.OwnerKey }

// An owner record is authenticated, as a correction record is (auth.go):
// anyone who can write to a storage can put an object at meta/.owner.json,
// and the record decides which installation may write the tenant's objects
// there, so a record planted there could make an installation that was
// handed the tenant fence itself, or could hide that another installation
// took the storage over. Each record carries an HMAC-SHA-256 (RFC 2104)
// under a key derived from the writer's installation key for this use
// alone:
//
//	message = len(installation) || installation || len(tenant) || tenant ||
//	          generation || body
//	mac     = HMAC-SHA-256(key derived for owner records from installation
//	          key version mac_key_version, message)
//
// where each len is 4 bytes, big-endian; generation is 8 bytes, big-endian;
// and body is the record as EncodeOwner writes it, with mac_key_version set
// and without its "mac" field. Only the installation that wrote a record can
// verify it: one written by another installation (the one a tenant was
// handed to) is told by its installation field, and one naming this
// installation that does not verify was not written by it.

// OwnerKeyring makes the MAC of an owner record: package secret's Keys is
// one.
type OwnerKeyring interface {
	// SealVersion returns the version of the installation key a new record
	// is authenticated under, or 0 when the key file holds none.
	SealVersion() uint32
	// OwnerMAC returns the HMAC-SHA-256 of message under the key derived
	// for owner records from that version of the installation key, or an
	// error for a version the key file does not hold.
	OwnerMAC(version uint32, message []byte) ([]byte, error)
}

// ownerMessage returns the message an owner record's MAC is computed over.
func ownerMessage(o Owner, body []byte) []byte {
	out := make([]byte, 0, 16+len(o.Installation)+len(o.Tenant)+len(body))
	out = binary.BigEndian.AppendUint32(out, uint32(len(o.Installation))) //nolint:gosec // G115: an installation id is at most 64 bytes (validID).
	out = append(out, o.Installation...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(o.Tenant))) //nolint:gosec // G115: a tenant id is at most 64 bytes (validID).
	out = append(out, o.Tenant...)
	out = binary.BigEndian.AppendUint64(out, uint64(o.Generation)) //nolint:gosec // G115: a generation is 1 to MaxGeneration (Validate).
	return append(out, body...)
}

// validID reports whether s is 1 to 64 of A-Z, a-z, 0-9, "_" and "-".
func validID(s string) bool {
	if s == "" || len(s) > maxIDLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// Validate returns an error wrapping ErrFormat unless o is a record this
// package writes, its MAC aside.
func (o Owner) Validate() error {
	switch {
	case o.Format != Format:
		return bad("format %d is not %d", o.Format, Format)
	case !validID(o.Installation):
		return bad("installation is 1 to %d of A-Z, a-z, 0-9, _ and -", maxIDLength)
	case !validID(o.Tenant):
		return bad("tenant is 1 to %d of A-Z, a-z, 0-9, _ and -", maxIDLength)
	case o.Generation < 1 || o.Generation > MaxGeneration:
		return bad("generation is 1 to %d", int64(MaxGeneration))
	case !validTime(o.Written):
		return bad("written is a time in UTC")
	case o.MAC != "" && !lowerHex64(o.MAC):
		return bad("mac is 64 lowercase hex digits")
	}
	return nil
}

// Supersedes reports whether o is a later owner than other: a higher
// generation. Equal generations of different installations are neither
// later; a caller treats them as a conflict.
func (o Owner) Supersedes(other Owner) bool { return o.Generation > other.Generation }

// EncodeOwner returns the record's bytes, authenticated under keys' seal
// version, or an error wrapping ErrFormat if it is not valid. The time is
// written in UTC. Whatever KeyVersion and MAC o holds are replaced. No keys
// is an error: a record is never written unauthenticated.
func EncodeOwner(o Owner, keys OwnerKeyring) ([]byte, error) {
	if keys == nil || keys.SealVersion() == 0 {
		return nil, errors.New("record: no installation key to authenticate an owner record with")
	}
	o.Written = o.Written.UTC()
	o.KeyVersion, o.MAC = keys.SealVersion(), ""
	if err := o.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(o)
	if err != nil {
		return nil, bad("it cannot be encoded")
	}
	mac, err := keys.OwnerMAC(o.KeyVersion, ownerMessage(o, body))
	if err != nil {
		return nil, fmt.Errorf("record: the owner record cannot be authenticated: %w", err)
	}
	o.MAC = hex.EncodeToString(mac)
	b, err := json.Marshal(o)
	if err != nil {
		return nil, bad("it cannot be encoded")
	}
	return append(b, '\n'), nil
}

// DecodeOwner reads one owner record from r, which must hold nothing after
// it, within MaxBytes. It does not verify the MAC: a record another
// installation wrote cannot be verified here and is still read, for whom it
// names. A caller that trusts a record naming this installation calls
// VerifyOwner.
func DecodeOwner(r io.Reader) (Owner, error) {
	var o Owner
	if err := decode(r, &o); err != nil {
		return Owner{}, err
	}
	if err := o.Validate(); err != nil {
		return Owner{}, err
	}
	return o, nil
}

// VerifyOwner returns an error wrapping ErrUnauthenticated unless o's MAC is
// the one keys make for it: a record written by the installation that holds
// keys, unchanged since.
func VerifyOwner(o Owner, keys OwnerKeyring) error {
	if keys == nil || keys.SealVersion() == 0 {
		return errors.New("record: no installation key to authenticate an owner record with")
	}
	if o.KeyVersion == 0 || !lowerHex64(o.MAC) {
		return fmt.Errorf("%w: it carries no MAC", ErrUnauthenticated)
	}
	got, _ := hex.DecodeString(o.MAC)
	o.MAC = ""
	body, err := json.Marshal(o)
	if err != nil {
		return bad("it cannot be encoded")
	}
	want, err := keys.OwnerMAC(o.KeyVersion, ownerMessage(o, body))
	if err != nil {
		return fmt.Errorf("%w: key version %d: %w", ErrUnauthenticated, o.KeyVersion, err)
	}
	if !hmac.Equal(got, want) {
		return fmt.Errorf("%w: its MAC does not match", ErrUnauthenticated)
	}
	return nil
}
