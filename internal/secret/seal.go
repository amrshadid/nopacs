package secret

import (
	"encoding/binary"

	"github.com/amrshadid/nopacs/internal/fault"
)

// formatV1 is the first byte of every sealed value in the format the package
// documentation describes. A later format takes a new byte, so a value always
// says how it is to be opened.
const formatV1 = 0x01

// headerSize is the format byte and the 4-byte key version.
const headerSize = 1 + 4

// overhead is what AES-GCM with a random nonce adds to a plaintext: the
// 12-byte (96-bit) nonce in front and the 16-byte tag behind.
const overhead = 12 + 16

// maxPlaintext bounds what Seal accepts and so what Open has to read. The
// secrets sealed here are database addresses and passwords; anything far
// larger is a mistake, and a bound keeps a hostile value in a row from
// costing more than a few microseconds to refuse.
const maxPlaintext = 64 << 10

// errNotOpened is the one error Open returns. It says nothing about why: a
// reason ("wrong version", "bad tag") would tell whoever forged the value
// which part to fix next.
var errNotOpened = fault.New(fault.Internal, "secret: a sealed value does not open")

// Seal encrypts plaintext with the highest key version, for the given
// context, and returns the sealed value described in the package
// documentation. The context names where the value will be stored, such as
// "tenant/<id>/database"; Open must be given the same context, so a sealed
// value copied into another row does not open there. An empty context is
// refused, since it would bind the value to nothing.
//
// Each call draws a fresh random 96-bit nonce. NIST SP 800-38D requires the
// chance of one nonce being used twice under a key to stay at or below 2^-32
// (section 8), and so limits random nonces to 2^32 seals per key (section
// 8.3): a repeated nonce reveals the XOR of the two plaintexts and lets an
// attacker forge values that open. The archive seals a handful of secrets per
// tenant, many orders of magnitude below that; a key that had to seal more
// would be rotated by adding a version.
func (k *Keys) Seal(context string, plaintext []byte) ([]byte, error) {
	if context == "" {
		return nil, fault.New(fault.Invalid, "secret: a secret must be sealed for a context")
	}
	if len(plaintext) > maxPlaintext {
		return nil, fault.New(fault.Invalid, "secret: a secret is at most 64 KiB")
	}
	// A nil *Keys and the zero Keys hold no key; both are refused here,
	// rather than panicking, like any Keys without the version it needs.
	r := k.ring()
	if r == nil {
		return nil, fault.New(fault.Internal, "secret: no key is loaded")
	}
	aead, ok := r.aeads[r.sealVersion]
	if !ok {
		return nil, fault.New(fault.Internal, "secret: no key is loaded")
	}
	sealed := make([]byte, headerSize, headerSize+overhead+len(plaintext))
	sealed[0] = formatV1
	binary.BigEndian.PutUint32(sealed[1:headerSize], r.sealVersion)
	// The nonce argument is nil because the AEAD from
	// cipher.NewGCMWithRandomNonce draws its own random nonce and writes it
	// in front of the ciphertext; it panics if given one.
	return aead.Seal(sealed, nil, plaintext, additionalData(sealed[:headerSize], context)), nil //nolint:gosec // G407: the nonce is random, made inside Seal.
}

// Open returns the plaintext of a value Seal made for the same context with
// any key version k holds. It fails with kind Internal and one fixed message
// for every reason, and never returns part of a plaintext.
func (k *Keys) Open(context string, sealed []byte) ([]byte, error) {
	if context == "" || len(sealed) < headerSize+overhead || len(sealed) > headerSize+overhead+maxPlaintext {
		return nil, errNotOpened
	}
	if sealed[0] != formatV1 {
		return nil, errNotOpened
	}
	r := k.ring()
	if r == nil {
		return nil, errNotOpened
	}
	aead, ok := r.aeads[binary.BigEndian.Uint32(sealed[1:headerSize])]
	if !ok {
		return nil, errNotOpened
	}
	plaintext, err := aead.Open(nil, nil, sealed[headerSize:], additionalData(sealed[:headerSize], context))
	if err != nil {
		return nil, errNotOpened
	}
	return plaintext, nil
}

// additionalData is what GCM authenticates without encrypting: the header,
// then the context. The header has a fixed length, so the boundary between
// the two is never in doubt.
func additionalData(header []byte, context string) []byte {
	ad := make([]byte, 0, len(header)+len(context))
	ad = append(ad, header...)
	return append(ad, context...)
}
