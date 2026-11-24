package naming

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"strings"
)

// HashLength is the length of a keyed hash's text: 80 bits in base32, 5 bits
// to a character. 80 bits make a collision between two values of one tenant
// unlikely far past any archive's count of patients, studies or series (the
// birthday bound is about 2^40 values), in 16 characters of the 128 a label
// may hold.
const HashLength = 16

// hashEncoding is RFC 4648 base32 in lower case, without padding. Base32,
// because its characters are all label characters and all one case, so a
// hash never collides with another on a file system that ignores case, and
// it is shorter than hexadecimal (16 characters for 80 bits, not 20).
// Base64 would be shorter still, but holds both cases and "+" and "/".
var hashEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Hasher gives the keyed hash of a value for a {#...} placeholder. The
// caller supplies it, because the key is a secret of the tenant, which the
// core neither holds nor reads. Every Hasher must return the text HashText
// returns for its key, so that every part of the server, and a later
// version of it, makes the same labels from the same key; NewHasher is
// that Hasher.
type Hasher interface {
	// Hash returns the keyed hash of value as HashLength characters of
	// lower case base32.
	Hash(value []byte) string
}

// HashText returns the text a {#...} placeholder puts in a label for value
// under key: the HMAC-SHA-256 of value (RFC 2104, FIPS 180-4) under key,
// its first 80 bits, in lower case base32 without padding (RFC 4648 section
// 6, its alphabet lowercased), HashLength characters. Value is the bytes of
// the attribute's value as Attributes gives it, with nothing trimmed.
func HashText(key, value []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(value)
	return hashEncoding.EncodeToString(m.Sum(nil)[:HashLength*5/8])
}

// TrimValue removes from a value as text the padding PS3.5 6.2 says is not
// part of it: the trailing spaces or NULs that make its length even, and,
// for the representations whose leading spaces are not significant (AE,
// CS, DS, IS, LO, SH, the dates and times, UI), the leading spaces too. It
// is the one trimming of an attribute's value before HashText: the header
// a label is made from and the de-identification engine that hashes or
// keys a value both call it, so one value always gives one code and one
// subject.
func TrimValue(vr, s string) string {
	s = strings.TrimRight(s, " \x00")
	switch vr {
	case "AE", "CS", "DS", "IS", "LO", "SH", "DA", "DT", "TM", "UI":
		s = strings.TrimLeft(s, " ")
	}
	return s
}

// MinKeyLength is the shortest key NewHasher takes: the output size of
// SHA-256, the key length RFC 2104 section 3 recommends as the least for
// HMAC. A shorter key would let anyone who guesses it reverse a label to
// the identifier it hides by trying identifiers.
const MinKeyLength = sha256.Size

// NewHasher returns the Hasher of HashText under key, or an error if key is
// shorter than MinKeyLength. It keeps its own copy of key, so the caller
// may clear its own.
func NewHasher(key []byte) (Hasher, error) {
	if len(key) < MinKeyLength {
		return nil, errors.New("naming: a hashing key is at least 32 bytes")
	}
	return keyHasher(append([]byte(nil), key...)), nil
}

type keyHasher []byte

func (k keyHasher) Hash(value []byte) string { return HashText(k, value) }

// isHashText reports whether s is HashLength characters of the lower case
// base32 alphabet, as a Hasher must return.
func isHashText(s string) bool {
	if len(s) != HashLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}
