package secret

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestErrorsCarryNoSecret guards the package's promise that no error it
// returns carries a key, a plaintext or an API key's secret. It provokes
// every error the package can return with secret material in reach (each
// refusal of a key file, each Seal and Open failure, each malformed API key)
// and searches the log's text, the caller's message and the %+v and %#v
// forms of each for the material in raw, base64 and hex form.
func TestErrorsCarryNoSecret(t *testing.T) {
	key1, key2 := testKey(1), testKey(2)
	k, err := MakeAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	apiSecret := k.Key()[len(k.Key())-secretLength:]
	plaintext := []byte(testPlaintext)

	var needles [][]byte
	for _, s := range [][]byte{key1, key2, bytes.Repeat([]byte{0xff}, keySize), plaintext, []byte(apiSecret), []byte(k.Key())} {
		needles = append(needles, s,
			[]byte(base64.StdEncoding.EncodeToString(s)),
			[]byte(base64.RawStdEncoding.EncodeToString(s)),
			[]byte(base64.URLEncoding.EncodeToString(s)),
			[]byte(hex.EncodeToString(s)))
	}
	// The keys' 32-bit words in decimal too, as fmt would print a key
	// schedule. Not for the all-ones key: its word, 4294967295, is the
	// largest uint32 and may be named by a message for reasons of its own.
	needles = append(needles, keyForms(key1)...)
	needles = append(needles, keyForms(key2)...)
	// A truncated key in a refused line must not be quoted either.
	needles = append(needles, []byte(base64.StdEncoding.EncodeToString(key1)[:20]))

	var errs []error
	for _, text := range malformedKeyFiles() {
		_, err := loadText(t, text)
		errs = append(errs, err)
		_, err = parseKeyFile([]byte(text))
		errs = append(errs, err)
	}
	_, err = loadText(t, string(bytes.Repeat([]byte(keyLine(1, 1)), maxKeyFileSize/63+1)))
	errs = append(errs, err)
	_, err = LoadKeyFile(filepath.Join(privateDir(t), "absent.key"))
	errs = append(errs, err)

	existing := filepath.Join(privateDir(t), "nopacs.key")
	if err := os.WriteFile(existing, []byte(keyLine(1, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	errs = append(errs, CreateKeyFile(existing))

	keys := mustKeys(t, keyLine(1, 1)+keyLine(2, 2))
	sealed := mustSeal(t, keys, testContext, plaintext)
	_, err = keys.Seal("", plaintext)
	errs = append(errs, err)
	_, err = keys.Seal(testContext, append(bytes.Repeat([]byte{'x'}, maxPlaintext), plaintext...))
	errs = append(errs, err)
	var zero Keys
	_, err = zero.Seal(testContext, plaintext)
	errs = append(errs, err)
	_, err = keys.Open("tenant/other/database", sealed)
	errs = append(errs, err)
	_, err = mustKeys(t, keyLine(1, 1)+keyLine(2, 9)).Open(testContext, sealed)
	errs = append(errs, err)
	changed := bytes.Clone(sealed)
	changed[len(changed)-1] ^= 1
	_, err = keys.Open(testContext, changed)
	errs = append(errs, err)
	_, err = keys.Open(testContext, sealed[:20])
	errs = append(errs, err)

	for _, presented := range []string{
		k.Key() + "x", k.Key()[:len(k.Key())-1], "NOPACS_" + k.Key()[len("nopacs_"):], k.Key()[:len(k.Key())-1] + "0", " " + k.Key(),
	} {
		_, _, err := ParseAPIKey(presented)
		errs = append(errs, err)
	}

	for i, err := range errs {
		if err == nil {
			t.Errorf("case %d: no error, so nothing was checked", i)
			continue
		}
		for _, form := range []string{err.Error(), fault.Message(err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
			if found, ok := containsAny(form, needles...); ok {
				t.Errorf("case %d: an error carries secret material (%d bytes of it)", i, len(found))
			}
		}
	}
	t.Logf("searched %d errors for %d forms of secret material", len(errs), len(needles))
}
