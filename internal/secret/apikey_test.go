package secret

import (
	"crypto/sha256"
	"math"
	"regexp"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// apiKeyPattern is the documented shape of an API key, written out
// independently of the code that makes and parses one.
var apiKeyPattern = regexp.MustCompile(`^nopacs_([1-9A-HJ-NP-Za-km-z]{12})_([1-9A-HJ-NP-Za-km-z]{44})$`)

// TestMakeAPIKeyShape guards the made key: the documented shape, its id is
// the one returned, and its hash is the SHA-256 of the secret part.
func TestMakeAPIKeyShape(t *testing.T) {
	for range 100 {
		k, err := MakeAPIKey()
		if err != nil {
			t.Fatal(err)
		}
		m := apiKeyPattern.FindStringSubmatch(k.Key())
		if m == nil {
			t.Fatalf("a made key of %d characters does not have the documented shape", len(k.Key()))
		}
		if m[1] != k.ID {
			t.Errorf("the key's id is %q, but %q was returned", m[1], k.ID)
		}
		if k.Hash != sha256.Sum256([]byte(m[2])) {
			t.Error("the hash is not the SHA-256 of the secret part")
		}
	}
}

// TestAPIKeyRoundTrip guards that a made key parses back to its id and a
// secret that verifies against its stored hash.
func TestAPIKeyRoundTrip(t *testing.T) {
	k, err := MakeAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	id, secret, err := ParseAPIKey(k.Key())
	if err != nil {
		t.Fatalf("ParseAPIKey of a made key: %v", err)
	}
	if id != k.ID {
		t.Errorf("parsed id %q, made %q", id, k.ID)
	}
	if !VerifyAPISecret(secret, k.Hash[:]) {
		t.Error("a made key does not verify")
	}
}

// authenticate does what the API will: parse the presented key, find the
// stored hash by id, verify the secret. It stands in for the catalog here.
func authenticate(stored map[string][]byte, presented string) bool {
	id, secret, err := ParseAPIKey(presented)
	if err != nil {
		return false
	}
	hash, ok := stored[id]
	return ok && VerifyAPISecret(secret, hash)
}

// TestAPIKeyOneCharacterChanged guards that changing any one character of a
// key, to any other character of the alphabet or to one outside it, makes it
// fail: in the prefix or a separator it does not parse, in the id it finds no
// row, in the secret it does not verify.
func TestAPIKeyOneCharacterChanged(t *testing.T) {
	k, err := MakeAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string][]byte{k.ID: k.Hash[:]}
	if !authenticate(stored, k.Key()) {
		t.Fatal("the unchanged key does not authenticate")
	}
	replacements := base58 + "0OIl_-+/= \x00é"
	for i := range len(k.Key()) {
		for _, r := range replacements {
			s := string(r)
			if k.Key()[i:i+1] == s {
				continue
			}
			changed := k.Key()[:i] + s + k.Key()[i+1:]
			if authenticate(stored, changed) {
				t.Fatalf("the key with character %d changed authenticates", i)
			}
		}
	}
}

// TestParseAPIKeyRefuses guards that every malformed key is refused with kind
// Unauthenticated and the one fixed message, which never quotes the input.
func TestParseAPIKeyRefuses(t *testing.T) {
	k, err := MakeAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	id, secret := k.ID, k.Key()[len(k.Key())-secretLength:]
	tests := map[string]string{
		"empty":              "",
		"prefix only":        "nopacs_",
		"upper prefix":       "NOPACS_" + id + "_" + secret,
		"other prefix":       "mgx_" + id + "_" + secret,
		"no prefix":          id + "_" + secret + "xxxx",
		"dash separator":     "nopacs_" + id + "-" + secret,
		"short id":           "nopacs_" + id[:11] + "_" + secret + "1",
		"long id":            "nopacs_" + id + "1_" + secret[:43],
		"short secret":       "nopacs_" + id + "_" + secret[:43],
		"long secret":        "nopacs_" + id + "_" + secret + "1",
		"zero in secret":     "nopacs_" + id + "_0" + secret[1:],
		"O in id":            "nopacs_O" + id[1:] + "_" + secret,
		"I in secret":        "nopacs_" + id + "_" + secret[:43] + "I",
		"l in id":            "nopacs_" + id[:11] + "l_" + secret,
		"space":              "nopacs_" + id + "_" + secret[:43] + " ",
		"newline":            "nopacs_" + id + "_" + secret[:43] + "\n",
		"leading space":      " nopacs_" + id + "_" + secret[:43],
		"multibyte":          "nopacs_" + id + "_" + secret[:42] + "é",
		"bearer word":        "Bearer " + k.Key(),
		"underscore in id":   "nopacs_" + id[:11] + "__" + secret[:43],
		"key twice":          k.Key() + k.Key(),
		"very long":          strings.Repeat("a", 1<<20),
		"nul in secret":      "nopacs_" + id + "_" + secret[:43] + "\x00",
		"separator replaced": "nopacs_" + id + "1" + secret,
	}
	for name, presented := range tests {
		t.Run(name, func(t *testing.T) {
			gotID, gotSecret, err := ParseAPIKey(presented)
			if err == nil {
				t.Fatalf("parsed a malformed key as id %q", gotID)
			}
			if gotID != "" || gotSecret != "" {
				t.Error("a refusal returned parts of the key")
			}
			if fault.KindOf(err) != fault.Unauthenticated {
				t.Errorf("kind %v, want Unauthenticated", fault.KindOf(err))
			}
			if err.Error() != "the key is not valid" || fault.Message(err) != "the key is not valid" {
				t.Errorf("message %q, want the one fixed message", err.Error())
			}
		})
	}
}

// TestVerifyAPISecretRefuses guards that a wrong secret, and a stored hash of
// the wrong length or missing, never verify.
func TestVerifyAPISecretRefuses(t *testing.T) {
	k, err := MakeAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := ParseAPIKey(k.Key())
	if err != nil {
		t.Fatal(err)
	}
	other, err := MakeAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	for name, stored := range map[string][]byte{
		"other key's hash": other.Hash[:],
		"nil":              nil,
		"empty":            {},
		"31 bytes":         k.Hash[:31],
		"33 bytes":         append(k.Hash[:], 0),
	} {
		if VerifyAPISecret(secret, stored) {
			t.Errorf("%s: verified", name)
		}
	}
	if VerifyAPISecret("", k.Hash[:]) {
		t.Error("an empty secret verified")
	}
}

// TestAPIKeysUniqueAndUniform guards the randomness of made keys: a million
// keys share no id and no secret, and the characters of their secrets are
// spread evenly over the alphabet. A modulo bias in drawing characters (all
// 256 byte values mapped onto 58) would make 24 characters a quarter more
// likely than the rest, which the spread check catches by a wide margin.
func TestAPIKeysUniqueAndUniform(t *testing.T) {
	if testing.Short() {
		t.Skip("a million keys take a few seconds")
	}
	const n = 1_000_000
	ids := make(map[string]struct{}, n)
	secrets := make(map[[sha256.Size]byte]struct{}, n)
	var counts [256]int
	for range n {
		k, err := MakeAPIKey()
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := ids[k.ID]; dup {
			t.Fatal("two made keys share an id")
		}
		ids[k.ID] = struct{}{}
		if _, dup := secrets[k.Hash]; dup {
			t.Fatal("two made keys share a secret")
		}
		secrets[k.Hash] = struct{}{}
		for i := len(k.Key()) - secretLength; i < len(k.Key()); i++ {
			counts[k.Key()[i]]++
		}
	}
	// Each character is expected n*44/58 times, with a binomial standard
	// deviation of about 860; six of those is far from both chance and bias.
	expected := float64(n*secretLength) / float64(len(base58))
	sd := math.Sqrt(expected * (1 - 1/float64(len(base58))))
	for i := range len(base58) {
		c := base58[i]
		if dev := math.Abs(float64(counts[c]) - expected); dev > 6*sd {
			t.Errorf("character %q appears %d times, expected %.0f +- %.0f", c, counts[c], expected, 6*sd)
		}
	}
}
