package record

import (
	"crypto/hmac"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// A correction record is authenticated: anyone who can write to a storage
// can put an object under meta/.corrections/, and a record is what a
// rebuild hides by, so a record planted there could hide a live subject
// for good, or a record changed there could bring withdrawn data back. Each
// record carries an HMAC-SHA-256 (RFC 2104) under a key derived from the
// installation key for this use alone, which only the installation that
// holds the key file can make:
//
//	message = len(tenant) || tenant || len(id) || id || body
//	mac     = HMAC-SHA-256(key derived for correction records from
//	          installation key version mac_key_version, message)
//
// where each len is 4 bytes, big-endian; tenant is the tenant's id; id is
// the correction's operation id; and body is the record encoded as
// EncodeCorrection writes it, with mac_key_version set and without its
// "mac" field, which EncodeCorrection writes last: the record's bytes with
// `,"mac":"<64 hex digits>"` taken out before the closing brace and the
// newline after it removed. The tenant binds a record to its tenant, so a
// record copied to another tenant's storage does not verify there; the
// whole body binds every field, its state above all, so a record moved back
// to an earlier state, or to undone, does not verify either.

// ErrUnauthenticated is returned, wrapped, for a record in the form this
// package writes whose MAC is missing, is not of the tenant and operation it
// is read for, or does not match its content: a record not written by this
// installation, or changed since. A reader never applies such a record.
var ErrUnauthenticated = errors.New("record: not authenticated by the installation key")

// Keyring makes the MAC of a correction record: package secret's Keys is
// one. The core states it as an interface so it holds no key and does no
// key derivation itself.
type Keyring interface {
	// SealVersion returns the version of the installation key a new record
	// is authenticated under: the highest the key file holds, or 0 when it
	// holds none.
	SealVersion() uint32
	// RecordMAC returns the HMAC-SHA-256 of message under the key derived
	// for correction records from that version of the installation key, or
	// an error for a version the key file does not hold.
	RecordMAC(version uint32, message []byte) ([]byte, error)
}

// macMessage returns the message a record's MAC is computed over.
func macMessage(tenantID, id string, body []byte) []byte {
	out := make([]byte, 0, 8+len(tenantID)+len(id)+len(body))
	out = binary.BigEndian.AppendUint32(out, uint32(len(tenantID))) //nolint:gosec // G115: a tenant id is a short validated name.
	out = append(out, tenantID...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(id))) //nolint:gosec // G115: an operation id is a 26-character ULID.
	out = append(out, id...)
	return append(out, body...)
}

// checkAuth refuses a keyring or tenant id that cannot authenticate: no
// keys is a mistake of the caller's, never a reason to skip the check.
func checkAuth(tenantID string, keys Keyring) error {
	if keys == nil || keys.SealVersion() == 0 {
		return errors.New("record: no installation key to authenticate a correction record with")
	}
	if tenantID == "" {
		return errors.New("record: no tenant to authenticate a correction record for")
	}
	return nil
}

// sign returns c with its MAC set, under keys' seal version.
func sign(c Correction, tenantID string, keys Keyring) (Correction, error) {
	c.KeyVersion, c.MAC = keys.SealVersion(), ""
	body, err := json.Marshal(c)
	if err != nil {
		return c, bad("it cannot be encoded")
	}
	mac, err := keys.RecordMAC(c.KeyVersion, macMessage(tenantID, c.ID, body))
	if err != nil {
		return c, fmt.Errorf("record: the record cannot be authenticated: %w", err)
	}
	c.MAC = hex.EncodeToString(mac)
	return c, nil
}

// verify returns an error wrapping ErrUnauthenticated unless c's MAC is the
// one keys make for it, of the tenant named.
func verify(c Correction, tenantID string, keys Keyring) error {
	if c.KeyVersion == 0 || !lowerHex64(c.MAC) {
		return fmt.Errorf("%w: it carries no MAC", ErrUnauthenticated)
	}
	got, _ := hex.DecodeString(c.MAC)
	c.MAC = ""
	body, err := json.Marshal(c)
	if err != nil {
		return bad("it cannot be encoded")
	}
	want, err := keys.RecordMAC(c.KeyVersion, macMessage(tenantID, c.ID, body))
	if err != nil {
		// A version the key file does not hold: a record written under a
		// key this installation never had, or a version made up.
		return fmt.Errorf("%w: key version %d: %w", ErrUnauthenticated, c.KeyVersion, err)
	}
	if !hmac.Equal(got, want) {
		return fmt.Errorf("%w: its MAC does not match", ErrUnauthenticated)
	}
	return nil
}
