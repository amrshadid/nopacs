package auditchain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"
)

// entryName is the first field of every encoded entry, so an entry's bytes
// can never be taken for a checkpoint's or for a later encoding's.
const entryName = "nopacs/audit-entry/v1"

// Entry is one audit entry as the chain sees it: every column of the audit
// log, and the position it was given when it was sealed.
type Entry struct {
	// Seq is the entry's position in its log, from 1, given at sealing.
	Seq uint64
	// ID is the entry's id.
	ID string
	// Time is when the request was made; encoded in microseconds, which is
	// what the databases store.
	Time time.Time
	// KeyID is the key that made the request, or empty.
	KeyID string
	// LocalUser is the operating system user on the local socket, or
	// empty.
	LocalUser string
	// RemoteAddress is where the request came from.
	RemoteAddress string
	// Method is the API method or command.
	Method string
	// TenantName is the tenant the request named.
	TenantName string
	// Object is what the request was about.
	Object string
	// Outcome is "done", "refused" or "failed".
	Outcome string
	// Reason is the cause of a refusal or failure, or empty.
	Reason string
	// Details are the entry's other named values.
	Details map[string]string
	// RequestID is the request's id.
	RequestID string
}

// Digest is a SHA-256 value: a position's head in the chain, or a
// checkpoint's digest.
type Digest [sha256.Size]byte

// String returns d in lowercase hex.
func (d Digest) String() string { return hex.EncodeToString(d[:]) }

// ParseDigest reads a digest written by String.
func ParseDigest(s string) (Digest, error) {
	var d Digest
	if len(s) != 2*len(d) {
		return d, errors.New("auditchain: a digest is 64 hex digits")
	}
	if _, err := hex.Decode(d[:], []byte(s)); err != nil {
		return d, errors.New("auditchain: a digest is 64 hex digits")
	}
	return d, nil
}

// appendField appends b as a field: its length as 4 bytes big-endian, then
// b.
func appendField(dst, b []byte) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(b))) //nolint:gosec // G115: every caller checks the length with checkLength first.
	return append(dst, b...)
}

// checkLength refuses a field too long for its 4-byte length.
func checkLength(name string, n int) error {
	if int64(n) > math.MaxUint32 {
		return errors.New("auditchain: the " + name + " is longer than a field can hold")
	}
	return nil
}

// textField appends s as a field, after checking it is valid UTF-8, so that
// what is encoded is exactly what an export in JSON can carry.
func textField(dst []byte, name, s string) ([]byte, error) {
	if !utf8.ValidString(s) {
		return nil, errors.New("auditchain: the " + name + " is not valid UTF-8")
	}
	if err := checkLength(name, len(s)); err != nil {
		return nil, err
	}
	return appendField(dst, []byte(s)), nil
}

// uint64Field appends v as an 8-byte field.
func uint64Field(dst []byte, v uint64) []byte {
	return appendField(dst, binary.BigEndian.AppendUint64(nil, v))
}

// Encode returns e's canonical encoding, the bytes the chain folds in, as
// the package documentation lays it out. It refuses seq 0, which no sealed
// entry has, and text that is not valid UTF-8.
func Encode(e Entry) ([]byte, error) {
	if e.Seq == 0 {
		return nil, errors.New("auditchain: an entry is encoded once it has a seq, from 1")
	}
	b := appendField(nil, []byte(entryName))
	b = uint64Field(b, e.Seq)
	var err error
	if b, err = textField(b, "id", e.ID); err != nil {
		return nil, err
	}
	b = uint64Field(b, uint64(e.Time.UnixMicro())) //nolint:gosec // G115: the bits of a signed time, read back as signed; documented as such.
	for _, f := range []struct{ name, value string }{
		{"key id", e.KeyID},
		{"local user", e.LocalUser},
		{"remote address", e.RemoteAddress},
		{"method", e.Method},
		{"tenant name", e.TenantName},
		{"object", e.Object},
		{"outcome", e.Outcome},
		{"reason", e.Reason},
	} {
		if b, err = textField(b, f.name, f.value); err != nil {
			return nil, err
		}
	}
	if err := checkLength("details", len(e.Details)); err != nil {
		return nil, err
	}
	b = binary.BigEndian.AppendUint32(b, uint32(len(e.Details))) //nolint:gosec // G115: checked just above.
	// Go compares strings byte by byte, which is the order the encoding
	// names, and what Python's sorted() gives over bytes.
	names := make([]string, 0, len(e.Details))
	for name := range e.Details {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if b, err = textField(b, "detail name", name); err != nil {
			return nil, err
		}
		if b, err = textField(b, "detail "+strconv.Quote(name), e.Details[name]); err != nil {
			return nil, err
		}
	}
	return textField(b, "request id", e.RequestID)
}

// Next folds e into the chain after prev: SHA-256(prev || Encode(e)).
func Next(prev Digest, e Entry) (Digest, error) {
	enc, err := Encode(e)
	if err != nil {
		return Digest{}, err
	}
	h := sha256.New()
	h.Write(prev[:])
	h.Write(enc)
	var d Digest
	h.Sum(d[:0])
	return d, nil
}
