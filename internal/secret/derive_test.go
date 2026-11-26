package secret

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// knownDerived holds keys derived from testKey(1) (version 1) and testKey(2)
// (version 2), and the Ed25519 public keys of the audit seeds. They were
// computed outside Go, with Python's hmac and hashlib following RFC 5869 by
// hand and checked equal to the cryptography package's HKDF, and the public
// keys with cryptography's Ed25519PrivateKey.from_private_bytes:
//
//	prk = hmac.new(b"\x00" * 32, key, hashlib.sha256).digest()
//	okm = hmac.new(prk, label + b"\x01", hashlib.sha256).digest()
var knownDerived = []struct {
	version uint32
	purpose Purpose
	key     string
	public  string
}{
	{1, AuditSeal, "f8b4d8032826456c59bdd98973bec302f52c5ae6efbe6003c996278ab464af35",
		"4943de520a6c6feb0da7cc596f55c5d11397ad3cc8340791cdb14feeeb82b649"},
	{1, Backup, "99399095e5aac5712bcd0caf8df7ec4457657fbf646005e3b207c1ce0c46d5a0", ""},
	{2, AuditSeal, "b70d66ca70d450463d6514932256732eab714cd5fe87dfb1b2f1912dea51bd22",
		"0bd0d8556ce2b456633321a0dbfae20f10167b292a6ece520b603b2bb5a1bcf4"},
	{2, Backup, "3b8e602fe83c178b31032009337c3ab2a57ba45b6a3bc6c5ad53bfd5b104db55", ""},
	{1, ConfirmToken, "f8546609d73a5ba04b2d3328638614568d5b9440f8517435f643933d754487b1", ""},
	{1, CorrectionRecord, "2129e4ad1940fceeceba068a4b55b8972ad9cf92535ecd4c9c06d253725f2c05", ""},
	{2, CorrectionRecord, "811c154fcf6ee89f6529a94e484b12af4cf925149f33c7c6018dc70a4eac2d48", ""},
	{1, OwnerRecord, "d0d7c08a405ec222dc509792a4264f5abbdcdabc0fe7341e64a9ac79b50b5f89", ""},
	{2, OwnerRecord, "92a81033ff033b7c658f3aa6f6e22c56f5107f594f66e9f208809380d75a0e92", ""},
	{1, HoldRecord, "773d6eeaa6933c7bca02c4a8237a5436f838899a81e65ff643024f3b3b61b8bd", ""},
	{2, HoldRecord, "8c10bed8c300b071a2955c64e2c27f4bdb5f7603b4f07d259bfff9caa930a297", ""},
}

// TestHoldMACKnownAnswer guards that HoldMAC is HMAC-SHA-256 under the key
// derived for HoldRecord from the version named: each value equals
// Python's hmac.new(okm, message, hashlib.sha256) with okm computed as
// above, and differs from a correction record's MAC of the same message.
func TestHoldMACKnownAnswer(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1)+keyLine(2, 2))
	for v, want := range map[uint32]string{
		1: "2362152720130136bc13c30f52b9de7d521db8e639fc70aa193b3a8e2a1635e7",
		2: "8f7c735ca7c218d28e44d772218efdb60d4ebf3563c98532a20cfd423d7e211f",
	} {
		got, err := keys.HoldMAC(v, []byte("hold message"))
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(got) != want {
			t.Errorf("version %d: HoldMAC = %x, want %s", v, got, want)
		}
		other, _ := keys.RecordMAC(v, []byte("hold message"))
		if hex.EncodeToString(other) == want {
			t.Errorf("version %d: a hold record's MAC equals a correction record's", v)
		}
	}
	if _, err := keys.HoldMAC(3, []byte("x")); fault.KindOf(err) != fault.NotFound {
		t.Errorf("a version the file does not hold: err = %v, want NotFound", err)
	}
}

// TestOwnerMACKnownAnswer guards that OwnerMAC is HMAC-SHA-256 under the
// key derived for OwnerRecord from the version named: each value equals
// Python's hmac.new(okm, message, hashlib.sha256) with okm computed as
// above, and differs from a correction record's MAC of the same message.
func TestOwnerMACKnownAnswer(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1)+keyLine(2, 2))
	for v, want := range map[uint32]string{
		1: "0196651c6c9330642c0d375be1193bfd64d14d3bd3e762817b5937dbaf071654",
		2: "b681c3aec8054fc66427f18c75cd13065a7208a5f956ca8f3939e3ae8256c1b4",
	} {
		got, err := keys.OwnerMAC(v, []byte("owner message"))
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(got) != want {
			t.Errorf("version %d: OwnerMAC = %x, want %s", v, got, want)
		}
		other, _ := keys.RecordMAC(v, []byte("owner message"))
		if hex.EncodeToString(other) == want {
			t.Errorf("version %d: an owner record's MAC equals a correction record's", v)
		}
	}
	if _, err := keys.OwnerMAC(3, []byte("x")); fault.KindOf(err) != fault.NotFound {
		t.Errorf("a version the file does not hold: err = %v, want NotFound", err)
	}
}

// TestRecordMACKnownAnswer guards that RecordMAC is HMAC-SHA-256 under the
// key derived for CorrectionRecord from the version named, not the key
// file's highest: each value equals Python's
// hmac.new(okm, message, hashlib.sha256) with okm computed as above. A
// version the key file does not hold is NotFound, and no key makes no MAC.
func TestRecordMACKnownAnswer(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1)+keyLine(2, 2))
	for v, want := range map[uint32]string{
		1: "4c1c58fbb9614a46d89622172628ba447c82549d7193f8ec2621320496108481",
		2: "40ab478d943d4577857f1285c4ec62ded603ef4bf98d1c18c30f916f0132e4f6",
	} {
		got, err := keys.RecordMAC(v, []byte("record message"))
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(got) != want {
			t.Errorf("version %d: RecordMAC = %x, want %s", v, got, want)
		}
	}
	if _, err := keys.RecordMAC(3, []byte("x")); fault.KindOf(err) != fault.NotFound {
		t.Errorf("a version the file does not hold: err = %v, want NotFound", err)
	}
	var none *Keys
	if _, err := none.RecordMAC(1, []byte("x")); err == nil {
		t.Error("a nil Keys made a record MAC")
	}
	if _, err := mustDerive(t, keys, CorrectionRecord, 1).MAC([]byte("x")); err == nil {
		t.Error("a CorrectionRecord key made a confirm token's MAC")
	}
}

// TestMACKnownAnswer guards that MAC is HMAC-SHA-256 under the key derived
// for ConfirmToken: the value equals Python's
// hmac.new(okm, message, hashlib.sha256) with okm computed as above. A key
// derived for another purpose makes no MAC.
func TestMACKnownAnswer(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1))
	got, err := mustDerive(t, keys, ConfirmToken, 1).MAC([]byte("c1.1700000000000000.abc"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "8060f746f63964d885bab751b9de6cb904d16f7347249e8783acfad96a015fde"; hex.EncodeToString(got) != want {
		t.Fatalf("MAC = %x, want %s", got, want)
	}
	if _, err := mustDerive(t, keys, Backup, 1).MAC([]byte("x")); err == nil {
		t.Fatal("a Backup key made a MAC")
	}
}

// mustDerive derives p at version from keys and fails the test on an error.
func mustDerive(t *testing.T, keys *Keys, p Purpose, version uint32) *DerivedKey {
	t.Helper()
	d, err := keys.DeriveVersion(p, version)
	if err != nil {
		t.Fatalf("DeriveVersion(%d, %d): %v", p, version, err)
	}
	return d
}

// TestDeriveKnownAnswers guards that Derive is HKDF-SHA-256 as RFC 5869
// defines it, with no salt, the documented labels and 32 bytes of output,
// and that the audit signer's seed gives the Ed25519 key RFC 8032 gives it:
// each value equals one computed independently in Python. A change of label,
// salt, hash or length, which would make every signature and backup made so
// far unverifiable, fails here.
func TestDeriveKnownAnswers(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1)+keyLine(2, 2))
	for _, k := range knownDerived {
		d := mustDerive(t, keys, k.purpose, k.version)
		if got := hex.EncodeToString(d.d.key[:]); got != k.key {
			t.Errorf("version %d %s: derived %s, want %s", k.version, k.purpose.Label(), got, k.key)
		}
		if d.Version() != k.version || d.Purpose() != k.purpose {
			t.Errorf("version %d %s: the key says version %d, purpose %d", k.version, k.purpose.Label(), d.Version(), d.Purpose())
		}
		if k.public == "" {
			continue
		}
		s, err := d.Signer()
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(s.Public()); got != k.public {
			t.Errorf("version %d: public key %s, want %s", k.version, got, k.public)
		}
		if s.KeyVersion() != k.version {
			t.Errorf("version %d: the signer says key version %d", k.version, s.KeyVersion())
		}
	}
}

// TestDeriveIsStableAcrossLoads guards that the same key file loaded twice,
// as by two runs of the server, derives the same keys, so a checkpoint
// signed before a restart verifies after it and a backup opens.
func TestDeriveIsStableAcrossLoads(t *testing.T) {
	path := filepath.Join(privateDir(t), "nopacs.key")
	writePrivate(t, path, []byte(keyLine(1, 7)+keyLine(3, 8)))
	first, err := LoadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []Purpose{AuditSeal, Backup} {
		for _, v := range []uint32{1, 3} {
			a, b := mustDerive(t, first, p, v), mustDerive(t, second, p, v)
			if *a.d.key != *b.d.key {
				t.Errorf("%s version %d differs between two loads of one file", p.Label(), v)
			}
		}
	}
}

// TestDeriveFollowsSealVersion guards that Derive uses the key file's
// highest version, the one a new record is made with, and DeriveVersion the
// one named.
func TestDeriveFollowsSealVersion(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1)+keyLine(2, 2))
	d, err := keys.Derive(AuditSeal)
	if err != nil {
		t.Fatal(err)
	}
	if d.Version() != 2 || *d.d.key != *mustDerive(t, keys, AuditSeal, 2).d.key {
		t.Errorf("Derive used version %d, want 2", d.Version())
	}
}

// TestPurposesNeverShareAKey guards that the purposes' labels are distinct
// and non-empty, and that no two (purpose, version) pairs of a key file give
// the same key, so a backup key can never sign an audit checkpoint or the
// other way round.
func TestPurposesNeverShareAKey(t *testing.T) {
	seen := map[string]Purpose{}
	for p, label := range labels {
		if label == "" {
			t.Errorf("purpose %d has an empty label", p)
		}
		if q, ok := seen[label]; ok {
			t.Errorf("purposes %d and %d share the label %q", p, q, label)
		}
		seen[label] = p
	}
	keys := mustKeys(t, keyLine(1, 1)+keyLine(2, 2)+keyLine(3, 3))
	derivedKeys := map[[derivedSize]byte]string{}
	for p := range labels {
		for _, v := range []uint32{1, 2, 3} {
			d := mustDerive(t, keys, p, v)
			name := fmt.Sprintf("%s version %d", p.Label(), v)
			if other, ok := derivedKeys[*d.d.key]; ok {
				t.Errorf("%s and %s derive the same key", name, other)
			}
			derivedKeys[*d.d.key] = name
			for _, seed := range []byte{1, 2, 3} {
				if bytes.Equal(d.d.key[:], testKey(seed)) {
					t.Errorf("%s equals an installation key", name)
				}
			}
		}
	}
}

// TestDeriveRefuses guards the refusals: a value that is not a purpose, a
// key version the file does not hold (NotFound, so a caller can say which
// version is missing), and no key at all.
func TestDeriveRefuses(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1))
	for _, p := range []Purpose{0, HoldRecord + 1, 255} {
		if _, err := keys.DeriveVersion(p, 1); fault.KindOf(err) != fault.Internal {
			t.Errorf("purpose %d: err = %v, want Internal", p, err)
		}
		if p.Label() != "" {
			t.Errorf("purpose %d has the label %q", p, p.Label())
		}
	}
	if _, err := keys.DeriveVersion(AuditSeal, 2); fault.KindOf(err) != fault.NotFound {
		t.Errorf("missing version: err = %v, want NotFound", err)
	}
	var none *Keys
	if _, err := none.Derive(AuditSeal); err == nil {
		t.Error("a nil Keys derived a key")
	}
	if _, err := (&Keys{}).Derive(Backup); err == nil {
		t.Error("the zero Keys derived a key")
	}
}

// TestDerivedKeyServesOnlyItsPurpose guards that a key derived for one
// purpose is refused by another's code: a Backup key makes no signer, and an
// AuditSeal key opens no backup file.
func TestDerivedKeyServesOnlyItsPurpose(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1))
	if _, err := mustDerive(t, keys, Backup, 1).Signer(); err == nil {
		t.Error("a Backup key made a signer")
	}
	if _, err := fileAEAD(mustDerive(t, keys, AuditSeal, 1), make([]byte, backupSaltSize)); err == nil {
		t.Error("an AuditSeal key made a backup cipher")
	}
	var none *DerivedKey
	if _, err := none.Signer(); err == nil {
		t.Error("a nil derived key made a signer")
	}
	var noSigner *Signer
	if _, err := noSigner.Sign([]byte("m")); err == nil {
		t.Error("a nil signer signed")
	}
}

// TestSignerSignatureVerifies guards that the signer makes pure Ed25519
// signatures (RFC 8032 5.1.6) that the standard verifier accepts with the
// public key alone, and that it rejects for another message.
func TestSignerSignatureVerifies(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1))
	s, err := mustDerive(t, keys, AuditSeal, 1).Signer()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("checkpoint text")
	sig, err := s.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != ed25519.SignatureSize || !ed25519.Verify(s.Public(), msg, sig) {
		t.Fatalf("a %d-byte signature does not verify", len(sig))
	}
	if ed25519.Verify(s.Public(), []byte("checkpoint texT"), sig) {
		t.Error("the signature verifies for another message")
	}
}
