package secret

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// testContext and testPlaintext are a synthetic secret and the row it belongs
// to, as the catalog will use them.
const (
	testContext   = "tenant/0f3a/database"
	testPlaintext = "postgres://nopacs:synthetic-test-password@db.invalid:5432/tenant" //nolint:gosec // G101: synthetic, for tests.
)

// TestSealOpen guards the round trip and the layout of a sealed value: format
// byte, big-endian key version, then nonce, ciphertext and tag, 28 bytes
// longer than the plaintext with the five-byte header.
func TestSealOpen(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1)+keyLine(258, 2))
	for _, plaintext := range []string{"", "x", testPlaintext, strings.Repeat("p", maxPlaintext)} {
		sealed := mustSeal(t, keys, testContext, []byte(plaintext))
		if len(sealed) != headerSize+overhead+len(plaintext) {
			t.Errorf("sealed %d bytes into %d, want %d", len(plaintext), len(sealed), headerSize+overhead+len(plaintext))
		}
		if sealed[0] != 0x01 || binary.BigEndian.Uint32(sealed[1:5]) != 258 {
			t.Errorf("header %x, want 01 00000102", sealed[:5])
		}
		// Only plaintexts long enough that random bytes do not contain them
		// by chance: a one-byte plaintext appears in 33 random bytes about
		// one time in eight.
		if len(plaintext) >= 8 && bytes.Contains(sealed, []byte(plaintext)) {
			t.Error("the sealed value contains the plaintext")
		}
		opened, err := keys.Open(testContext, sealed)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if string(opened) != plaintext {
			t.Errorf("opened %d bytes that differ from the %d sealed", len(opened), len(plaintext))
		}
	}
}

// TestSealFreshNonce guards that every seal draws a new nonce: two seals of
// the same plaintext differ in the nonce and in the ciphertext.
func TestSealFreshNonce(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1))
	seen := make(map[string]bool)
	for range 1000 {
		sealed := mustSeal(t, keys, testContext, []byte(testPlaintext))
		nonce := string(sealed[headerSize : headerSize+12])
		if seen[nonce] {
			t.Fatal("a nonce was used twice")
		}
		seen[nonce] = true
	}
}

// TestSealVersions guards rotation: the highest version seals, and a value
// sealed under an older version still opens once a newer line is added.
func TestSealVersions(t *testing.T) {
	old := mustKeys(t, keyLine(1, 1))
	sealedOld := mustSeal(t, old, testContext, []byte(testPlaintext))

	rotated := mustKeys(t, keyLine(1, 1)+keyLine(2, 2))
	if rotated.SealVersion() != 2 {
		t.Fatalf("seal version %d, want 2", rotated.SealVersion())
	}
	sealedNew := mustSeal(t, rotated, testContext, []byte(testPlaintext))
	if v := binary.BigEndian.Uint32(sealedNew[1:5]); v != 2 {
		t.Errorf("sealed with version %d, want 2", v)
	}
	for name, sealed := range map[string][]byte{"old": sealedOld, "new": sealedNew} {
		if opened, err := rotated.Open(testContext, sealed); err != nil || string(opened) != testPlaintext {
			t.Errorf("%s value: Open = %v", name, err)
		}
	}
	if _, err := old.Open(testContext, sealedNew); err == nil {
		t.Error("keys without version 2 opened a value sealed with it")
	}
}

// requireNotOpened fails the test unless err is the one fixed Open failure:
// kind Internal, the fixed message, and nothing wrapped.
func requireNotOpened(t *testing.T, what string, opened []byte, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: opened %d bytes, want a failure", what, len(opened))
	}
	if opened != nil {
		t.Errorf("%s: a failure returned %d bytes", what, len(opened))
	}
	if fault.KindOf(err) != fault.Internal || err.Error() != "secret: a sealed value does not open" {
		t.Errorf("%s: %v (kind %v), want the one fixed Internal failure", what, err, fault.KindOf(err))
	}
}

// TestOpenRefusesWrongContext guards the binding of a value to its row: a
// sealed value moved to another context does not open, however close the
// other context is.
func TestOpenRefusesWrongContext(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1))
	sealed := mustSeal(t, keys, testContext, []byte(testPlaintext))
	for _, other := range []string{
		"tenant/0f3b/database", "tenant/0f3a/databas", "tenant/0f3a/database ",
		"Tenant/0f3a/database", "tenant/0f3a", "x", "",
	} {
		opened, err := keys.Open(other, sealed)
		requireNotOpened(t, "context "+other, opened, err)
	}
}

// TestOpenRefusesWrongKey guards that the same version number with another
// key does not open, so a key file restored from the wrong installation fails
// loudly rather than returning garbage.
func TestOpenRefusesWrongKey(t *testing.T) {
	sealed := mustSeal(t, mustKeys(t, keyLine(1, 1)), testContext, []byte(testPlaintext))
	other := mustKeys(t, keyLine(1, 2))
	opened, err := other.Open(testContext, sealed)
	requireNotOpened(t, "wrong key", opened, err)
}

// TestOpenRefusesEveryChangedByte guards integrity: changing any bit pattern
// of any byte (header, nonce, ciphertext or tag) makes Open fail.
func TestOpenRefusesEveryChangedByte(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1)+keyLine(2, 2))
	sealed := mustSeal(t, keys, testContext, []byte(testPlaintext))
	for i := range sealed {
		for _, mask := range []byte{0x01, 0x02, 0x10, 0x80, 0xff} {
			changed := bytes.Clone(sealed)
			changed[i] ^= mask
			opened, err := keys.Open(testContext, changed)
			if err == nil {
				t.Fatalf("byte %d xor %#02x: opened", i, mask)
			}
			requireNotOpened(t, "changed byte", opened, err)
		}
	}
}

// TestOpenRefusesWrongLength guards that every truncation of a sealed value,
// and a value with a byte added, fails, without a panic on the short ones.
func TestOpenRefusesWrongLength(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1))
	sealed := mustSeal(t, keys, testContext, []byte(testPlaintext))
	for n := range len(sealed) {
		opened, err := keys.Open(testContext, sealed[:n])
		requireNotOpened(t, "truncated", opened, err)
	}
	opened, err := keys.Open(testContext, append(bytes.Clone(sealed), 0))
	requireNotOpened(t, "extended", opened, err)
	opened, err = keys.Open(testContext, nil)
	requireNotOpened(t, "nil", opened, err)
	opened, err = keys.Open(testContext, make([]byte, headerSize+overhead+maxPlaintext+1))
	requireNotOpened(t, "too long", opened, err)
}

// TestOpenRefusesUnknownHeader guards that an unknown format byte or a key
// version the file does not hold fails, even with every other byte intact.
func TestOpenRefusesUnknownHeader(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1))
	sealed := mustSeal(t, keys, testContext, []byte(testPlaintext))
	for _, format := range []byte{0x00, 0x02, 0xff} {
		changed := bytes.Clone(sealed)
		changed[0] = format
		opened, err := keys.Open(testContext, changed)
		requireNotOpened(t, "format", opened, err)
	}
	changed := bytes.Clone(sealed)
	binary.BigEndian.PutUint32(changed[1:5], 9)
	opened, err := keys.Open(testContext, changed)
	requireNotOpened(t, "version 9", opened, err)
}

// TestSealRefuses guards Seal's refusals: no context, a plaintext over the
// bound, and Keys with no key loaded.
func TestSealRefuses(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1))
	if _, err := keys.Seal("", []byte("x")); fault.KindOf(err) != fault.Invalid {
		t.Errorf("empty context: %v, want Invalid", err)
	}
	if _, err := keys.Seal(testContext, make([]byte, maxPlaintext+1)); fault.KindOf(err) != fault.Invalid {
		t.Errorf("too large: %v, want Invalid", err)
	}
	var zero Keys
	if _, err := zero.Seal(testContext, []byte("x")); err == nil {
		t.Error("the zero Keys sealed")
	}
	if _, err := zero.Open(testContext, make([]byte, 40)); err == nil {
		t.Error("the zero Keys opened")
	}
}

// TestNilKeys guards that a nil *Keys, as a caller holds after a failed
// load, refuses to seal and open with the same errors the zero Keys gives,
// and reports no versions, instead of panicking.
func TestNilKeys(t *testing.T) {
	var keys *Keys
	var zero Keys
	sealed := mustSeal(t, mustKeys(t, keyLine(1, 1)), testContext, []byte(testPlaintext))

	_, nilErr := keys.Seal(testContext, []byte("x"))
	_, zeroErr := zero.Seal(testContext, []byte("x"))
	if nilErr == nil || nilErr.Error() != zeroErr.Error() || fault.KindOf(nilErr) != fault.KindOf(zeroErr) {
		t.Errorf("nil Seal: %v, zero Seal: %v; want the same refusal", nilErr, zeroErr)
	}
	opened, err := keys.Open(testContext, sealed)
	requireNotOpened(t, "nil Keys", opened, err)
	if keys.Versions() != 0 || keys.SealVersion() != 0 {
		t.Error("a nil Keys reports versions")
	}
	if got := fmt.Sprint(keys); got != "<nil>" {
		t.Errorf("Sprint(nil *Keys) = %q", got)
	}
}
