package record

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeKeys is a Keyring over fixed keys by version, so the record's tests
// need no key file; the derivation itself is package secret's, and the
// outside check runs this package with secret's real keys.
type fakeKeys struct {
	seal uint32
	keys map[uint32][]byte
}

// SealVersion implements Keyring.
func (f fakeKeys) SealVersion() uint32 { return f.seal }

// RecordMAC implements Keyring.
func (f fakeKeys) RecordMAC(version uint32, message []byte) ([]byte, error) {
	k, ok := f.keys[version]
	if !ok {
		return nil, fmt.Errorf("no key version %d", version)
	}
	m := hmac.New(sha256.New, k)
	m.Write(message)
	return m.Sum(nil), nil
}

// OwnerMAC implements OwnerKeyring, under keys of its own: the version's
// key with its bytes inverted, so an owner record's MAC is never a
// correction record's.
func (f fakeKeys) OwnerMAC(version uint32, message []byte) ([]byte, error) {
	k, ok := f.keys[version]
	if !ok {
		return nil, fmt.Errorf("no key version %d", version)
	}
	inv := make([]byte, len(k))
	for i, b := range k {
		inv[i] = ^b
	}
	m := hmac.New(sha256.New, inv)
	m.Write(message)
	return m.Sum(nil), nil
}

// testKeys holds two versions and seals with the second.
var testKeys = fakeKeys{seal: 2, keys: map[uint32][]byte{1: bytes.Repeat([]byte{1}, 32), 2: bytes.Repeat([]byte{2}, 32)}}

// tenantA and tenantB are tenant ids.
const (
	tenantA = "01jd5x7k2m3n4p5q6r7s8t9v0a"
	tenantB = "01jd5x7k2m3n4p5q6r7s8t9v0b"
)

// encode writes c for tenantA under testKeys.
func encode(c Correction) ([]byte, error) { return EncodeCorrection(c, tenantA, testKeys) }

// decodeRec reads a record of tenantA under testKeys.
func decodeRec(r io.Reader) (Correction, error) { return DecodeCorrection(r, tenantA, testKeys) }

// macField matches the MAC field as EncodeCorrection writes it, last.
var macField = regexp.MustCompile(`,"mac":"[0-9a-f]{64}"}\n$`)

// TestCorrectionAuthenticated guards the record's MAC: a record signed then
// read for its tenant verifies, under an older key version too; one with a
// byte of its content changed, read for another tenant, given another
// correction's id or another record's MAC, with no MAC, or naming a key
// version the keys do not hold is refused as unauthenticated, not as a form
// error, and its content is not returned.
func TestCorrectionAuthenticated(t *testing.T) {
	c := published(deleteRec(), StateCommitted)
	good, err := encode(c)
	if err != nil {
		t.Fatal(err)
	}
	if !macField.Match(good) || !bytes.Contains(good, []byte(`"mac_key_version":2,"mac":"`)) {
		t.Fatalf("the MAC is not the record's last field: %s", good)
	}
	if _, err := decodeRec(bytes.NewReader(good)); err != nil {
		t.Fatalf("a signed record does not verify: %v", err)
	}
	// Written under version 1, read once version 2 seals.
	old, err := EncodeCorrection(c, tenantA, fakeKeys{seal: 1, keys: testKeys.keys})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decodeRec(bytes.NewReader(old)); err != nil || got.KeyVersion != 1 {
		t.Fatalf("a record of an older key version: %+v, %v", got, err)
	}

	other := c
	other.ID = "01jd5x7k2m3n4p5q6r7s8t9v0x"
	otherRec, err := encode(other)
	if err != nil {
		t.Fatal(err)
	}
	otherMAC := macField.Find(otherRec)
	s := string(good)
	for name, in := range map[string]string{
		"one byte":       strings.Replace(s, `"created":"2026-11-20T08:30:00.123456Z"`, `"created":"2026-11-20T08:30:00.123457Z"`, 1),
		"undone":         strings.Replace(s, `"state":"committed"`, `"state":"undone"`, 1),
		"subject":        strings.Replace(s, `"subject":"SUB-001"`, `"subject":"SUB-002"`, 1),
		"operation id":   strings.Replace(s, opID, other.ID, 1),
		"another's mac":  macField.ReplaceAllString(s, string(otherMAC)),
		"no mac":         macField.ReplaceAllString(s, "}\n"),
		"no mac version": strings.Replace(macField.ReplaceAllString(s, "}\n"), `,"mac_key_version":2`, "", 1),
		"version 3":      strings.Replace(s, `"mac_key_version":2`, `"mac_key_version":3`, 1),
		"version 1":      strings.Replace(s, `"mac_key_version":2`, `"mac_key_version":1`, 1),
		"upper mac":      strings.Replace(s, `"mac":"`, `"mac":"A`, 1)[:len(s)-3] + "\"}\n",
	} {
		if in == s {
			t.Fatalf("%s: the case changes nothing", name)
		}
		got, err := decodeRec(strings.NewReader(in))
		if !errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrFormat) {
			t.Errorf("%s: err = %v, want ErrUnauthenticated", name, err)
		}
		if got.ID != "" {
			t.Errorf("%s: the content of a refused record was returned", name)
		}
	}
	if _, err := DecodeCorrection(bytes.NewReader(good), tenantB, testKeys); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("another tenant: err = %v, want ErrUnauthenticated", err)
	}
	if _, err := DecodeCorrection(bytes.NewReader(good), tenantA, fakeKeys{seal: 2, keys: map[uint32][]byte{2: bytes.Repeat([]byte{9}, 32)}}); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("another installation's key: err = %v, want ErrUnauthenticated", err)
	}
}

// TestCorrectionNeedsKeys guards that no keys, or no tenant, is refused on
// both sides rather than a record written or read without its MAC: an error
// of neither the form's kind nor the MAC's, so a reader fails rather than
// counting the record as one to pass over.
func TestCorrectionNeedsKeys(t *testing.T) {
	good, err := encode(deleteRec())
	if err != nil {
		t.Fatal(err)
	}
	var typedNil *fakeKeysNil
	for name, keys := range map[string]Keyring{"nil": nil, "empty": fakeKeys{}, "typed nil": typedNil} {
		if b, err := EncodeCorrection(deleteRec(), tenantA, keys); err == nil {
			t.Errorf("%s keys: wrote %s", name, b)
		}
		_, err := DecodeCorrection(bytes.NewReader(good), tenantA, keys)
		if err == nil || errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrFormat) {
			t.Errorf("%s keys: err = %v, want an error of the caller's", name, err)
		}
	}
	if _, err := EncodeCorrection(deleteRec(), "", testKeys); err == nil {
		t.Error("a record was written for no tenant")
	}
	if _, err := DecodeCorrection(bytes.NewReader(good), "", testKeys); err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Errorf("no tenant: err = %v", err)
	}
}

// TestOwnerAuthenticated guards the owner record's MAC: a record written
// verifies, under an older key version too; one whose installation, tenant
// or generation is changed, whose MAC is another record's or missing, or
// that names a key version the keys do not hold, or that other keys check,
// is refused as unauthenticated; and no record is written without keys.
func TestOwnerAuthenticated(t *testing.T) {
	read := func(b []byte) Owner {
		t.Helper()
		o, err := DecodeOwner(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	good, err := EncodeOwner(ownerRec(), testKeys)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyOwner(read(good), testKeys); err != nil {
		t.Fatalf("a good record: %v", err)
	}
	older := fakeKeys{seal: 1, keys: testKeys.keys}
	old, err := EncodeOwner(ownerRec(), older)
	if err != nil {
		t.Fatal(err)
	}
	if o := read(old); o.KeyVersion != 1 || VerifyOwner(o, testKeys) != nil {
		t.Fatalf("a record under an older version: %+v", o)
	}
	other, _ := EncodeOwner(Owner{Format: Format, Installation: "inst_B", Tenant: "lab1", Generation: 3, Written: t0}, testKeys)
	stranger := fakeKeys{seal: 2, keys: map[uint32][]byte{2: bytes.Repeat([]byte{9}, 32)}}
	for name, o := range map[string]Owner{
		"installation": func() Owner { o := read(good); o.Installation = "inst_B"; return o }(),
		"tenant":       func() Owner { o := read(good); o.Tenant = "lab2"; return o }(),
		"generation":   func() Owner { o := read(good); o.Generation = 4; return o }(),
		"written":      func() Owner { o := read(good); o.Written = o.Written.Add(time.Second); return o }(),
		"other's mac":  func() Owner { o := read(good); o.MAC = read(other).MAC; return o }(),
		"no mac":       func() Owner { o := read(good); o.MAC, o.KeyVersion = "", 0; return o }(),
		"version":      func() Owner { o := read(good); o.KeyVersion = 7; return o }(),
	} {
		if err := VerifyOwner(o, testKeys); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: err = %v, want unauthenticated", name, err)
		}
	}
	if err := VerifyOwner(read(good), stranger); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("another installation's keys: err = %v", err)
	}
	if b, err := EncodeOwner(ownerRec(), fakeKeys{}); err == nil {
		t.Errorf("written with no keys: %s", b)
	}
	if err := VerifyOwner(read(good), nil); err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Errorf("checked with no keys: %v", err)
	}
}

// fakeKeysNil is a Keyring whose nil pointer holds no key, as a nil
// *secret.Keys does.
type fakeKeysNil struct{}

// SealVersion implements Keyring: 0 for a nil pointer.
func (f *fakeKeysNil) SealVersion() uint32 { return 0 }

// RecordMAC implements Keyring and is never reached.
func (f *fakeKeysNil) RecordMAC(uint32, []byte) ([]byte, error) { return nil, errors.New("no key") }
