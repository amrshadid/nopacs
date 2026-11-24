package auditchain

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"testing"
	"time"
)

// knownCheckpoint is the checkpoint whose signed text knownSignedHex pins.
var knownCheckpoint = Checkpoint{
	Installation: "inst-1",
	Log:          MasterLog,
	From:         1,
	To:           7,
	Head:         Digest(bytes.Repeat([]byte{0x11}, 32)),
	Previous:     Digest(bytes.Repeat([]byte{0x22}, 32)),
	Time:         time.UnixMicro(1795000001000000).UTC(),
	KeyVersion:   2,
}

// knownSignedHex and knownCheckpointDigestHex are knownCheckpoint's signed
// text and digest, computed by scripts/check/auditchain.py (struct and
// hashlib), not by this package.
const (
	knownSignedHex           = "0000001a6e6f706163732f61756469742d636865636b706f696e742f763100000006696e73742d31000000066d6173746572000000080000000000000001000000080000000000000007000000201111111111111111111111111111111111111111111111111111111111111111000000202222222222222222222222222222222222222222222222222222222222222222000000080006608b016e72400000000400000002"
	knownCheckpointDigestHex = "b788032a61551bb944c83a07f556cc6dd97ae4ec88b750ee3c0ab6ab0bf89e1c"
)

// TestSignedTextKnownAnswer guards the checkpoint's signed text and digest
// against drift: both equal what an independent implementation made from
// the documented layout.
func TestSignedTextKnownAnswer(t *testing.T) {
	text, err := knownCheckpoint.SignedText()
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(text); got != knownSignedHex {
		t.Fatalf("SignedText = %s\nwant         %s", got, knownSignedHex)
	}
	d, err := knownCheckpoint.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if d.String() != knownCheckpointDigestHex {
		t.Fatalf("Digest = %s, want %s", d, knownCheckpointDigestHex)
	}
}

// TestSignedTextCoversEveryField guards that a change to any field but the
// signature changes the signed text, so the signature no longer verifies.
func TestSignedTextCoversEveryField(t *testing.T) {
	s := newTestSigner(2, 7)
	signed, err := Sign(s, knownCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !signed.VerifySignature(s.public()) {
		t.Fatal("a signed checkpoint does not verify")
	}
	for name, change := range map[string]func(*Checkpoint){
		"installation": func(c *Checkpoint) { c.Installation = "inst-2" },
		"log":          func(c *Checkpoint) { c.Log = "t-1" },
		"from":         func(c *Checkpoint) { c.From = 2 },
		"to":           func(c *Checkpoint) { c.To = 8 },
		"head":         func(c *Checkpoint) { c.Head[31] ^= 1 },
		"previous":     func(c *Checkpoint) { c.Previous[0] ^= 1 },
		"time":         func(c *Checkpoint) { c.Time = c.Time.Add(time.Microsecond) },
		"key version":  func(c *Checkpoint) { c.KeyVersion = 3 },
		"signature":    func(c *Checkpoint) { c.Signature = append([]byte(nil), c.Signature...); c.Signature[5] ^= 1 },
	} {
		c := signed
		change(&c)
		if c.VerifySignature(s.public()) {
			t.Errorf("%s changed: the signature still verifies", name)
		}
	}
	if signed.VerifySignature(newTestSigner(2, 8).public()) {
		t.Error("the signature verifies under another key")
	}
	if signed.VerifySignature(s.public()[:31]) {
		t.Error("a short public key was taken")
	}
}

// TestSignRefusesAnEmptyRange guards that no checkpoint covers no entry or
// starts at seq 0.
func TestSignRefusesAnEmptyRange(t *testing.T) {
	s := newTestSigner(1, 1)
	for _, r := range [][2]uint64{{0, 0}, {0, 3}, {5, 4}} {
		c := knownCheckpoint
		c.From, c.To = r[0], r[1]
		if _, err := Sign(s, c); err == nil {
			t.Errorf("from %d to %d was signed", r[0], r[1])
		}
	}
	if _, _, err := Seal(s, "i", MasterLog, State{}, nil, baseTime); err == nil {
		t.Error("Seal made a checkpoint of no entries")
	}
	if _, _, err := Seal(s, "i", MasterLog, State{}, testEntries(2, 2), baseTime); err == nil {
		t.Error("Seal took entries that do not start at seq 1")
	}
}

// TestPublicKeyPEMRoundTrip guards the public key's export: PEM of the PKIX
// structure, read back to the same key, and refused when it is not exactly
// one Ed25519 PUBLIC KEY block.
func TestPublicKeyPEMRoundTrip(t *testing.T) {
	pub := newTestSigner(1, 9).public()
	text, err := MarshalPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(text, []byte("-----BEGIN PUBLIC KEY-----\n")) {
		t.Fatalf("not a PEM public key:\n%s", text)
	}
	back, err := ParsePublicKey(text)
	if err != nil || !bytes.Equal(back, pub) {
		t.Fatalf("ParsePublicKey = %x, %v", back, err)
	}
	if _, err := MarshalPublicKey(pub[:16]); err == nil {
		t.Error("a 16-byte key was marshaled")
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&ec.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	other := newTestSigner(1, 10).public()
	otherText, _ := MarshalPublicKey(other)
	for name, bad := range map[string][]byte{
		"empty":          nil,
		"two keys":       append(append([]byte(nil), text...), otherText...),
		"another type":   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
		"an ECDSA key":   pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}),
		"not DER":        pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("nope")}),
		"trailing words": append(append([]byte(nil), text...), "trailing"...),
	} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Errorf("%s: read as a key", name)
		}
	}
}
