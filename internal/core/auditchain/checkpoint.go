package auditchain

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"strings"
	"time"
)

// checkpointName is the first field of every checkpoint's signed text.
const checkpointName = "nopacs/audit-checkpoint/v1"

// MasterLog is the log name of the master database's audit; a tenant's log
// is named by the tenant's id.
const MasterLog = "master"

// Checkpoint is a signed record of a log's chain head after a range of
// entries.
type Checkpoint struct {
	// Installation is the id of the installation whose key signed it, so a
	// checkpoint copied to another installation does not verify there.
	Installation string
	// Log is MasterLog or a tenant's id.
	Log string
	// From and To are the first and last seq covered, both included.
	From, To uint64
	// Head is the chain's value after entry To.
	Head Digest
	// Previous is the digest of the checkpoint before, zero for the first.
	Previous Digest
	// Time is when the checkpoint was made; encoded in microseconds.
	Time time.Time
	// KeyVersion is the installation key version the signing seed was
	// derived from.
	KeyVersion uint32
	// Signature is the 64-byte Ed25519 signature of SignedText.
	Signature []byte
}

// SignedText returns the bytes the signature covers, as the package
// documentation lays them out.
func (c Checkpoint) SignedText() ([]byte, error) {
	if c.From == 0 || c.To < c.From {
		return nil, errors.New("auditchain: a checkpoint covers seq From to To, with 1 <= From <= To")
	}
	b := appendField(nil, []byte(checkpointName))
	var err error
	if b, err = textField(b, "installation", c.Installation); err != nil {
		return nil, err
	}
	if b, err = textField(b, "log", c.Log); err != nil {
		return nil, err
	}
	b = uint64Field(b, c.From)
	b = uint64Field(b, c.To)
	b = appendField(b, c.Head[:])
	b = appendField(b, c.Previous[:])
	b = uint64Field(b, uint64(c.Time.UnixMicro())) //nolint:gosec // G115: the bits of a signed time, documented as such.
	return appendField(b, binary.BigEndian.AppendUint32(nil, c.KeyVersion)), nil
}

// Digest returns the SHA-256 of c's signed text, which the next checkpoint
// names as its Previous.
func (c Checkpoint) Digest() (Digest, error) {
	text, err := c.SignedText()
	if err != nil {
		return Digest{}, err
	}
	return sha256.Sum256(text), nil
}

// Signer signs with the private key derived for the audit seal. The private
// key stays with the implementation.
type Signer interface {
	// KeyVersion returns the installation key version the key was derived
	// from.
	KeyVersion() uint32
	// Sign returns the pure Ed25519 signature of message (RFC 8032 5.1.6).
	Sign(message []byte) ([]byte, error)
}

// Sign returns c with KeyVersion set to s's and Signature over the result.
func Sign(s Signer, c Checkpoint) (Checkpoint, error) {
	c.KeyVersion = s.KeyVersion()
	text, err := c.SignedText()
	if err != nil {
		return Checkpoint{}, err
	}
	sig, err := s.Sign(text)
	if err != nil {
		return Checkpoint{}, err
	}
	c.Signature = sig
	return c, nil
}

// VerifySignature reports whether c's signature is valid for its signed text
// under pub (RFC 8032 5.1.7).
func (c Checkpoint) VerifySignature(pub ed25519.PublicKey) bool {
	if len(pub) != ed25519.PublicKeySize || len(c.Signature) != ed25519.SignatureSize {
		return false
	}
	text, err := c.SignedText()
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, text, c.Signature)
}

// MarshalPublicKey returns pub as a PEM "PUBLIC KEY" block holding its PKIX
// SubjectPublicKeyInfo (RFC 8410 4), the form openssl and most tools read.
func MarshalPublicKey(pub ed25519.PublicKey) ([]byte, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("auditchain: an Ed25519 public key is 32 bytes")
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// ParsePublicKey reads what MarshalPublicKey writes: exactly one PEM
// "PUBLIC KEY" block holding an Ed25519 key, with nothing but white space
// around it, so a file with a second key in it is not read as the first.
func ParsePublicKey(text []byte) (ed25519.PublicKey, error) {
	block, rest := pem.Decode(text)
	if block == nil || block.Type != "PUBLIC KEY" || len(block.Headers) != 0 {
		return nil, errors.New("auditchain: not a PEM PUBLIC KEY block")
	}
	if strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("auditchain: more than one PEM block")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New("auditchain: the PEM block is not a public key: " + err.Error())
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("auditchain: the public key is not Ed25519")
	}
	return pub, nil
}
