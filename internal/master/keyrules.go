package master

import (
	"context"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
)

// The bounds of a key's rules and of what a key's row records of its use.
const (
	// MaxKeyNetworks bounds the networks one key lists: a key is used from
	// a handful of places; a site with more lists them in the installation's
	// or the tenant's networks, which every key shares.
	MaxKeyNetworks = 64
	// ClientCertLength is the length of a client certificate's fingerprint
	// as a key holds it: the SHA-256 of the certificate's DER bytes in
	// lowercase hexadecimal.
	ClientCertLength = 64
	// MaxAddressLength bounds a key's last-used address, as an audit entry
	// bounds its remote address.
	MaxAddressLength = MaxAuditFieldLength
)

// The keys RotateKey adds to its audit entry's details, read inside the
// change.
const (
	// DetailSuccessor is the detail that holds the id of the key a
	// rotation made.
	DetailSuccessor = "successor"
	// DetailOldExpires is the detail that holds when the rotated key stops
	// working after the rotation, in RFC 3339.
	DetailOldExpires = "old_expires"
)

// KeyRules is where a key may be used from: the networks a request carrying
// it may come from, and the client certificate it must come with. The zero
// KeyRules allows every network and asks for no certificate. They are
// checked for every request made with the key off the installation's local
// socket.
type KeyRules struct {
	// Networks is the networks the key may be used from; empty for every
	// network. Each is in the one form settings.ParseNetworks reads.
	Networks settings.Networks
	// ClientCert is the SHA-256 fingerprint of the TLS client certificate a
	// request with the key must present (ClientCertLength lowercase
	// hexadecimal digits), or empty for none.
	ClientCert string
}

// IsZero reports whether r restricts nothing.
func (r KeyRules) IsZero() bool { return len(r.Networks) == 0 && r.ClientCert == "" }

// Validate returns an Invalid error when r cannot be stored: more than
// MaxKeyNetworks networks, a network not in its one written form or listed
// twice, or a fingerprint that is not 64 lowercase hexadecimal digits.
func (r KeyRules) Validate() error {
	if len(r.Networks) > MaxKeyNetworks {
		return invalid("a key lists at most " + strconv.Itoa(MaxKeyNetworks) + " networks")
	}
	if _, err := settings.ParseNetworks(r.Networks.String()); err != nil {
		return invalid("a key's networks " + err.Error())
	}
	if r.ClientCert != "" && !ValidClientCert(r.ClientCert) {
		return invalid("a client certificate's fingerprint is its SHA-256 in 64 lowercase hexadecimal digits")
	}
	return nil
}

// ValidClientCert reports whether fp is a fingerprint as a key holds it: 64
// lowercase hexadecimal digits.
func ValidClientCert(fp string) bool {
	return len(fp) == ClientCertLength && allBytes(fp, func(b byte) bool { return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') })
}

// validateSuccessor returns an Invalid error when k cannot be the key a
// rotation makes: what Key.Validate asks of a new key's id, hash and times.
// Its holder and rules are the rotated key's, read inside the change, so
// whatever k holds there is ignored.
func validateSuccessor(k Key) error {
	k.Holder = "successor"
	k.Rules = KeyRules{}
	return k.Validate()
}

// ValidateRotation returns an Invalid error when a rotation that makes
// successor and ends the rotated key at oldEnds cannot be made, or when its
// audit entry would be past its bounds once the store adds its details, so a
// driver refuses it before it changes anything. oldEnds is not before the
// successor's creation: a rotated key never ends in the past.
func ValidateRotation(successor Key, oldEnds time.Time, audit AuditEntry) error {
	if err := validateSuccessor(successor); err != nil {
		return err
	}
	if err := ValidateTime("end of the rotated key", oldEnds); err != nil {
		return err
	}
	if oldEnds.Before(successor.Created) {
		return invalid("a rotated key ends at or after its successor's creation")
	}
	return audit.WithDetails(DetailSuccessor, successor.ID, DetailOldExpires, maxTime.Format(time.RFC3339Nano)).Validate()
}

// RotatedExpiry returns when a key that expires at expires (the zero time for
// never) stops working once it is rotated to end at oldEnds: oldEnds, or its
// own expiry when that is sooner, so a rotation never makes a key live
// longer.
func RotatedExpiry(expires, oldEnds time.Time) time.Time {
	if !expires.IsZero() && expires.Before(oldEnds) {
		return expires
	}
	return oldEnds
}

// ValidateMark returns an Invalid error unless MarkKeyUsed may record a use of
// the key id at at from address: a valid id, a time in the store's range, and
// an address of UTF-8 of at most MaxAddressLength bytes.
func ValidateMark(id string, at time.Time, address string) error {
	if err := ValidateID("key id", id); err != nil {
		return err
	}
	if err := ValidateTime("time of use", at); err != nil {
		return err
	}
	return validateText("last-used address", address, 0, MaxAddressLength)
}

// MaxRotations bounds how many rotations back FindRotatedFrom follows a
// key's line: a key rotated more often than this since the key a list
// names is no longer taken for it, and a request costs at most this many
// reads however long the line.
const MaxRotations = 16

// RotationReader is the read FindRotatedFrom needs: the key a rotation
// replaced with a given key (Store.RotatedFrom).
type RotationReader interface {
	RotatedFrom(ctx context.Context, id string) (Key, error)
}

// FindRotatedFrom follows the line of keys the key id was rotated from,
// nearest first and at most MaxRotations back, and returns the first
// whose id match accepts, or "" when none does. A list that names a key
// thereby takes every key rotated from it, so a system that rotates its
// key keeps the access the list gave it. The key id itself is not asked.
// A failure to read the master is returned.
func FindRotatedFrom(ctx context.Context, r RotationReader, id string, match func(id string) bool) (string, error) {
	for range MaxRotations {
		k, err := r.RotatedFrom(ctx, id)
		if fault.KindOf(err) == fault.NotFound {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if match(k.ID) {
			return k.ID, nil
		}
		id = k.ID
	}
	return "", nil
}

// RotationConflict returns the Conflict a rotation of old meets at now, or
// nil: a key revoked, expired at now, or already rotated cannot be rotated,
// so a lost or ended key makes no successor, and a key makes at most one.
//
// A suspended key can be rotated, by intent, and its successor is not
// suspended: a restore suspends every key it brings back, and only the
// local operator, on the socket, can rotate one then (a suspended key
// authenticates nothing), which is how a holder gets a working key again.
// Refusing a suspended key here would leave a restored installation with no
// key that works.
func RotationConflict(old Key, now time.Time) error {
	switch {
	case !old.Revoked.IsZero():
		return fault.New(fault.Conflict, "the key is revoked")
	case !old.Expires.IsZero() && !old.Expires.After(now):
		return fault.New(fault.Conflict, "the key has expired")
	case old.Successor != "":
		return fault.New(fault.Conflict, "the key was already rotated; rotate its successor "+old.Successor)
	}
	return nil
}
