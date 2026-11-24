package secret

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"strconv"

	"github.com/amrshadid/nopacs/internal/fault"
)

// Purpose names what a derived key is for. Each purpose has one fixed label,
// the HKDF info string (RFC 5869 2.3), so two purposes never derive the same
// key from one installation key, and a key derived for one use is refused by
// the code of another. The set is closed: a value outside the constants below
// derives nothing, so no caller can pass a label of its own and collide with
// one of these.
type Purpose uint8

// The purposes. A label is never changed once a key derived under it has
// been used: the audit's signatures and the backups made under it would no
// longer verify or open. A new use, or a new version of one, takes a new
// constant and a new label.
const (
	// AuditSeal derives the Ed25519 seed (RFC 8032 5.1.5) that signs the
	// audit log's checkpoints.
	AuditSeal Purpose = iota + 1
	// Backup derives the key from which each backup file's own key is
	// derived, with the file's random salt (see NewBackupWriter).
	Backup
	// ConfirmToken derives the HMAC-SHA-256 key (RFC 2104) of the token a
	// delete's preview gives (see MAC), so only the server makes one.
	ConfirmToken
	// CorrectionRecord derives the HMAC-SHA-256 key (RFC 2104) that
	// authenticates each correction record a correction writes on a
	// storage (see RecordMAC), so a record written by anyone who can write
	// to the storage but does not hold the key file is told apart.
	CorrectionRecord
	// OwnerRecord derives the HMAC-SHA-256 key (RFC 2104) that
	// authenticates the owner record of a tenant on each of its storages
	// (see OwnerMAC), so a record naming this installation that someone
	// who can write to the storage but does not hold the key file put
	// there is told apart, and treated as another owner's.
	OwnerRecord
)

// labels holds each purpose's HKDF info string, in ASCII.
var labels = map[Purpose]string{ //nolint:gosec // G101: public HKDF info strings (RFC 5869 2.3), not credentials.
	AuditSeal:        "nopacs/audit-seal/v1",
	Backup:           "nopacs/backup/v1",
	ConfirmToken:     "nopacs/confirm-token/v1",
	CorrectionRecord: "nopacs/correction-record/v1",
	OwnerRecord:      "nopacs/owner-record/v1",
}

// derivedSize is the length of every derived key: 32 bytes, which is the
// Ed25519 seed's length and AES-256's key length.
const derivedSize = 32

// Label returns p's HKDF info string, or "" for a value that is not one of
// the purposes.
func (p Purpose) Label() string { return labels[p] }

// extract is HKDF-Extract with SHA-256 and no salt (RFC 5869 2.2): the salt
// is then HashLen zero bytes. A salt adds strength when the input keying
// material is not uniformly random; a key file's key is 32 bytes from the
// system's random source, so the extract only puts it in the form Expand
// takes.
func extract(key []byte) ([]byte, error) {
	return hkdf.Extract(sha256.New, key, nil)
}

// DerivedKey is a key derived from one version of the installation key for
// one purpose. It is made only by Keys.Derive and Keys.DeriveVersion and used
// only through this package's functions for its purpose, so the key's bytes
// never leave the package. Like Keys, it prints a fixed text in every form,
// and its key sits two pointers deep (see Keys for why one is not enough).
type DerivedKey struct {
	d *derived
}

// derived is what DerivedKey points to. The key is behind a pointer of its
// own, so fmt, once it has followed the pointer to derived, prints an
// address for it.
type derived struct {
	purpose Purpose
	version uint32
	key     *[derivedSize]byte
}

// text is the fixed text DerivedKey prints as.
func (d DerivedKey) text() string {
	if d.d == nil {
		return "secret.DerivedKey{}"
	}
	return "secret.DerivedKey{" + d.d.purpose.Label() + ", version " + strconv.FormatUint(uint64(d.d.version), 10) + "}"
}

// Format writes a fixed text in place of the key, for every verb.
func (d DerivedKey) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, d.text())
}

// LogValue gives log/slog the same fixed text.
func (d DerivedKey) LogValue() slog.Value {
	return slog.StringValue(d.text())
}

// Purpose returns what the key was derived for.
func (d *DerivedKey) Purpose() Purpose {
	if d == nil || d.d == nil {
		return 0
	}
	return d.d.purpose
}

// Version returns the version of the installation key it was derived from.
// A record made with a derived key stores this number, so the right key is
// derived again after a newer version has been added to the key file.
func (d *DerivedKey) Version() uint32 {
	if d == nil || d.d == nil {
		return 0
	}
	return d.d.version
}

// Derive returns the key for purpose p derived from the installation key's
// highest version, the one Seal uses:
//
//	HKDF-SHA-256(IKM = the version's 32-byte key, salt = none,
//	             info = p.Label(), L = 32)
//
// as RFC 5869 defines it. The same key file gives the same key on every run,
// so a signature made before a restart verifies after it.
func (k *Keys) Derive(p Purpose) (*DerivedKey, error) {
	return k.DeriveVersion(p, k.SealVersion())
}

// DeriveVersion is Derive from a given version of the installation key, for
// checking or opening what an older version made. A version the key file
// does not hold is NotFound; a value that is not a purpose is Internal, a
// mistake in the calling code.
func (k *Keys) DeriveVersion(p Purpose, version uint32) (*DerivedKey, error) {
	label, ok := labels[p]
	if !ok {
		return nil, fault.New(fault.Internal, "secret: "+strconv.Itoa(int(p))+" is not a purpose a key is derived for")
	}
	r := k.ring()
	if r == nil {
		return nil, fault.New(fault.Internal, "secret: no key is loaded")
	}
	prk, ok := r.prks[version]
	if !ok {
		return nil, fault.New(fault.NotFound, "secret: the key file holds no key version "+strconv.FormatUint(uint64(version), 10))
	}
	out, err := hkdf.Expand(sha256.New, prk[:], label, derivedSize)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "secret: a key cannot be derived")
	}
	d := &derived{purpose: p, version: version, key: new([derivedSize]byte)}
	copy(d.key[:], out)
	clear(out)
	return &DerivedKey{d: d}, nil
}

// need returns d's contents when d was derived for p, and an Internal error
// otherwise: a key derived for one purpose is never used for another.
func (d *DerivedKey) need(p Purpose) (*derived, error) {
	if d == nil || d.d == nil {
		return nil, fault.New(fault.Internal, "secret: no derived key")
	}
	if d.d.purpose != p {
		return nil, fault.New(fault.Internal, "secret: a key derived for "+d.d.purpose.Label()+" is used for "+p.Label())
	}
	return d.d, nil
}

// MAC returns the HMAC-SHA-256 (RFC 2104) of message under d, which must
// have been derived for ConfirmToken.
func (d *DerivedKey) MAC(message []byte) ([]byte, error) {
	dd, err := d.need(ConfirmToken)
	if err != nil {
		return nil, err
	}
	return hmacSHA256(dd.key, message), nil
}

// hmacSHA256 returns the HMAC-SHA-256 (RFC 2104) of message under key.
func hmacSHA256(key *[derivedSize]byte, message []byte) []byte {
	m := hmac.New(sha256.New, key[:])
	m.Write(message)
	return m.Sum(nil)
}

// RecordMAC returns the HMAC-SHA-256 (RFC 2104) of message under the key
// derived for CorrectionRecord from the given version of the installation
// key. A writer passes SealVersion and stores the version beside the MAC; a
// reader passes the stored version, so a record written before a newer key
// version was added still verifies. A version the key file does not hold is
// NotFound. The derived key is made for the call and cleared after it, so
// its bytes never leave the package.
func (k *Keys) RecordMAC(version uint32, message []byte) ([]byte, error) {
	d, err := k.DeriveVersion(CorrectionRecord, version)
	if err != nil {
		return nil, err
	}
	dd, err := d.need(CorrectionRecord)
	if err != nil {
		return nil, err
	}
	defer clear(dd.key[:])
	return hmacSHA256(dd.key, message), nil
}

// OwnerMAC returns the HMAC-SHA-256 (RFC 2104) of message under the key
// derived for OwnerRecord from the given version of the installation key,
// as RecordMAC does for a correction record: a writer passes SealVersion
// and stores it beside the MAC, a reader passes the stored version. A
// version the key file does not hold is NotFound.
func (k *Keys) OwnerMAC(version uint32, message []byte) ([]byte, error) {
	d, err := k.DeriveVersion(OwnerRecord, version)
	if err != nil {
		return nil, err
	}
	dd, err := d.need(OwnerRecord)
	if err != nil {
		return nil, err
	}
	defer clear(dd.key[:])
	return hmacSHA256(dd.key, message), nil
}

// Signer signs with an Ed25519 private key (RFC 8032) whose 32-byte seed is
// a key derived for AuditSeal. It prints a fixed text in every form, and the
// private key never leaves it: Public returns the public half, which is not
// secret and is what a verifier holds.
type Signer struct {
	s *signer
}

// signer is what Signer points to. The private key is behind a pointer of
// its own, as DerivedKey's is, so it sits two pointers deep.
type signer struct {
	version uint32
	private *[ed25519.PrivateKeySize]byte
}

// text is the fixed text Signer prints as.
func (s Signer) text() string {
	if s.s == nil {
		return "secret.Signer{}"
	}
	return "secret.Signer{Ed25519, key version " + strconv.FormatUint(uint64(s.s.version), 10) + "}"
}

// Format writes a fixed text in place of the key, for every verb.
func (s Signer) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, s.text())
}

// LogValue gives log/slog the same fixed text.
func (s Signer) LogValue() slog.Value {
	return slog.StringValue(s.text())
}

// Signer returns the Ed25519 signer whose seed is d (RFC 8032 5.1.5). d must
// have been derived for AuditSeal.
func (d *DerivedKey) Signer() (*Signer, error) {
	dd, err := d.need(AuditSeal)
	if err != nil {
		return nil, err
	}
	private := ed25519.NewKeyFromSeed(dd.key[:])
	s := &signer{version: dd.version, private: new([ed25519.PrivateKeySize]byte)}
	copy(s.private[:], private)
	clear(private)
	return &Signer{s: s}, nil
}

// KeyVersion returns the version of the installation key the signer's seed
// was derived from, which a signed record stores so a verifier picks the
// matching public key.
func (s *Signer) KeyVersion() uint32 {
	if s == nil || s.s == nil {
		return 0
	}
	return s.s.version
}

// Public returns the signer's Ed25519 public key, 32 bytes (RFC 8032 5.1.5).
func (s *Signer) Public() ed25519.PublicKey {
	if s == nil || s.s == nil {
		return nil
	}
	// The second half of an Ed25519 private key is its public key (RFC 8032
	// 5.1.5, as crypto/ed25519 lays it out).
	return ed25519.PublicKey(bytes.Clone(s.s.private[ed25519.SeedSize:]))
}

// Sign returns the 64-byte pure Ed25519 signature of message (RFC 8032
// 5.1.6; not Ed25519ph, so a verifier such as openssl pkeyutl -rawin checks
// it over the message itself).
func (s *Signer) Sign(message []byte) ([]byte, error) {
	if s == nil || s.s == nil {
		return nil, fault.New(fault.Internal, "secret: no signer")
	}
	return ed25519.Sign(s.s.private[:], message), nil
}
