package secret

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// printVerbs are the fmt verbs a value may meet in a log line or an error.
// %s, %q, %t, %f and the others matter as much as %v: on a value with no
// Format method, fmt walks into its fields, and a pointer met under a verb
// that does not fit a pointer is printed again with %v from depth 0, which
// follows it into what it points to.
var printVerbs = []string{
	"%v", "%+v", "%#v", "%s", "%q", "%t", "%d", "%x", "%X",
	"%f", "%e", "%g", "%c", "%U", "%b", "%o", "%p",
}

// printedForms returns every way v may be printed by mistake: each verb of
// fmt, the text and JSON handlers of log/slog, and encoding/json.
func printedForms(t *testing.T, v any) map[string]string {
	t.Helper()
	forms := make(map[string]string)
	for _, verb := range printVerbs {
		forms[verb] = fmt.Sprintf(verb, v)
	}
	var text, js bytes.Buffer
	slog.New(slog.NewTextHandler(&text, nil)).Info("m", "v", v)
	slog.New(slog.NewJSONHandler(&js, nil)).Info("m", "v", v)
	forms["slog text"] = text.String()
	forms["slog json"] = js.String()
	if b, err := json.Marshal(v); err == nil {
		forms["encoding/json"] = string(b)
	} else {
		forms["encoding/json"] = "error: " + err.Error()
	}
	return forms
}

// The ways a value can sit inside another: as a value or a pointer, in an
// exported or an unexported field, and a level deeper. fmt calls a field's
// methods only when the field is exported; an unexported one it reads by
// reflection.
type (
	heldValueExported[T any] struct{ X T }
	heldValueHidden[T any]   struct{ x T }
	heldPtrExported[T any]   struct{ X *T }
	heldPtrHidden[T any]     struct{ x *T }
	heldDeepHidden[T any]    struct{ in heldValueHidden[T] }
)

// holdings returns p in each way it can be held.
func holdings[T any](p *T) map[string]any {
	return map[string]any{
		"pointer":                         p,
		"value":                           *p,
		"value, exported field":           heldValueExported[T]{*p},
		"value, unexported field":         heldValueHidden[T]{*p},
		"pointer, exported field":         heldPtrExported[T]{p},
		"pointer, unexported field":       heldPtrHidden[T]{p},
		"value, two unexported levels":    heldDeepHidden[T]{heldValueHidden[T]{*p}},
		"value in a slice":                []T{*p},
		"value in a map":                  map[string]T{"k": *p},
		"pointer, unexported, two levels": heldDeepHidden[*T]{heldValueHidden[*T]{p}},
	}
}

// checkPrints fails the test for every way of printing p, held in every way,
// that carries one of the needles, and unless printing it directly with every
// verb but %p gives want. fmt answers %p itself, before any Format method, with
// the pointer's address, or for a value with the value walked under %v.
func checkPrints[T any](t *testing.T, p *T, want string, needles [][]byte) {
	t.Helper()
	for name, v := range holdings(p) {
		for form, out := range printedForms(t, v) {
			if found, ok := containsAny(out, needles...); ok {
				t.Errorf("%T %s, %s: prints secret material %q in %s", p, name, form, found, out)
			}
		}
	}
	for _, verb := range printVerbs {
		if verb == "%p" {
			continue
		}
		for _, v := range []any{p, *p} {
			if out := fmt.Sprintf(verb, v); out != want {
				t.Errorf("Sprintf(%q, %T) = %q, want the fixed text %q", verb, v, out, want)
			}
		}
	}
}

// mustExtract returns the HKDF pseudorandom key of key, as the key file
// keeps it.
func mustExtract(t *testing.T, key []byte) []byte {
	t.Helper()
	prk, err := extract(key)
	if err != nil {
		t.Fatal(err)
	}
	return prk
}

// TestKeysPrintNoKey guards that no way of printing Keys, on its own or
// inside another value, carries key material: not the keys, and not the HKDF
// pseudorandom keys from which every derived key follows, in raw bytes,
// base64, hex, decimal byte lists or 32-bit words in decimal, which is how
// fmt prints the cipher's expanded key.
func TestKeysPrintNoKey(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1)+keyLine(2, 2))
	var needles [][]byte
	for _, seed := range []byte{1, 2} {
		needles = append(needles, keyForms(testKey(seed))...)
		needles = append(needles, keyForms(mustExtract(t, testKey(seed)))...)
	}
	checkPrints(t, keys, "secret.Keys{2 versions}", needles)
}

// TestDerivedKeysPrintNoKey guards that no way of printing a derived key or
// a signer, on its own or inside another value, carries the derived key, the
// Ed25519 private key or the installation key, in any of the forms a key can
// leak in.
func TestDerivedKeysPrintNoKey(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1))
	audit := mustDerive(t, keys, AuditSeal, 1)
	backup := mustDerive(t, keys, Backup, 1)
	s, err := audit.Signer()
	if err != nil {
		t.Fatal(err)
	}
	// The Ed25519 private key is the seed, which is the audit key, followed
	// by the public key, which is not secret.
	needles := append(append(keyForms(derivedBytes(audit)), keyForms(derivedBytes(backup))...), keyForms(testKey(1))...)
	needles = append(needles, keyForms(mustExtract(t, testKey(1)))...)
	needles = append(needles, keyForms(signerPrivate(s))...)
	checkPrints(t, audit, "secret.DerivedKey{nopacs/audit-seal/v1, version 1}", needles)
	checkPrints(t, backup, "secret.DerivedKey{nopacs/backup/v1, version 1}", needles)
	checkPrints(t, s, "secret.Signer{Ed25519, key version 1}", needles)
}

// backupNeedles returns what a backup reader or writer must never print: the
// installation key, its pseudorandom key, the Backup key, the file's own key
// derived from it and the file's salt, and the plaintext it holds.
func backupNeedles(t *testing.T, keys *Keys, header, plain []byte) [][]byte {
	t.Helper()
	d := mustDerive(t, keys, Backup, 1)
	fileKey, err := hkdf.Key(sha256.New, derivedBytes(d), header[17:backupHeaderSize], backupFileInfo, 32)
	if err != nil {
		t.Fatal(err)
	}
	needles := append(keyForms(testKey(1)), keyForms(mustExtract(t, testKey(1)))...)
	needles = append(needles, keyForms(derivedBytes(d))...)
	needles = append(needles, keyForms(fileKey)...)
	return append(needles, plain, decimalList(plain), []byte(hex.EncodeToString(plain)))
}

// TestBackupStreamsPrintNoKey guards that a backup writer holding buffered
// plaintext, and a reader holding a decrypted chunk, print a fixed text in
// every way and never the cipher's keys or the plaintext they hold.
func TestBackupStreamsPrintNoKey(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1))
	plain := []byte("buffered plaintext of a synthetic backup, 0123456789")

	var buf bytes.Buffer
	w, err := newBackupWriter(&buf, keys, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	header := bytes.Clone(buf.Bytes())
	checkPrints(t, w, "secret.BackupWriter{key version 1}", backupNeedles(t, keys, header, plain))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := NewBackupReader(bytes.NewReader(buf.Bytes()), keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(r, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	checkPrints(t, r, "secret.BackupReader{key version 1}", backupNeedles(t, keys, header, plain))
}

// TestNewAPIKeyPrintsOnlyID guards that a made API key printed in any way,
// on its own or inside another value, shows its id and never its secret or
// the whole key, which only the Key method returns.
func TestNewAPIKeyPrintsOnlyID(t *testing.T) {
	k, err := MakeAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	full := k.Key()
	secret := full[len(full)-secretLength:]
	checkPrints(t, &k, "secret.NewAPIKey{ID: "+k.ID+"}", [][]byte{[]byte(secret)})
	if b, _ := json.Marshal(k); string(b) != `{"id":"`+k.ID+`"}` {
		t.Errorf("json.Marshal = %s, want the id alone", b)
	}
	if !strings.HasPrefix(full, "nopacs_"+k.ID+"_") {
		t.Errorf("Key() = a %d-character value that does not start with the id", len(full))
	}
}
