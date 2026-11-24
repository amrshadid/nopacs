package secret

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/amrshadid/nopacs/internal/fault"
)

// apiKeyPrefix starts every API key, so a secret scanner can recognize one
// that leaked into a repository or a log.
const apiKeyPrefix = "nopacs_"

// The lengths of an API key's parts, in characters.
const (
	// idLength gives 12 * log2(58), about 70 bits: enough that two keys
	// sharing an id is not a case worth handling.
	idLength = 12
	// secretLength gives 44 * log2(58), about 257.7 bits, the fewest
	// characters that hold 256 random bits.
	secretLength = 44
	// apiKeyLength is the whole key: prefix, id, separator, secret.
	apiKeyLength = len(apiKeyPrefix) + idLength + 1 + secretLength
)

// base58 is the alphabet of an API key's id and secret: digits and letters
// without 0, O, I and l, which are easy to confuse when a key is read out or
// copied by hand, and without punctuation, so a double-click selects the
// whole key.
const base58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// base58Limit is the largest multiple of 58 that fits in a byte (4 * 58). A
// random byte at or above it is drawn again, because mapping all 256 values
// onto 58 characters would make the first 24 characters more likely than the
// rest.
const base58Limit = 256 - 256%len(base58)

// invalidAPIKey is the one message for every malformed key: the caller is
// told the key is no good, not which part, so a guess cannot be refined.
const invalidAPIKey = "the key is not valid" //nolint:gosec // G101: a message for the caller, not a credential.

// NewAPIKey is a key just made: its public id, the hash of its secret, which
// is what the catalog stores, and the whole key, which is shown once to the
// person it was made for and never stored. The whole key is read only through
// the Key method, so that showing it is a call someone wrote on purpose.
//
// Printed in any way (fmt with any verb, log/slog, encoding/json), a
// NewAPIKey shows only its id. The whole key is held behind a pointer, so
// even as an unexported field of another value, where fmt reads fields
// instead of calling methods, it prints as an address.
type NewAPIKey struct {
	// ID is the public id, which finds the key's row and names the key in
	// listings, logs and audit entries.
	ID string
	// Hash is the SHA-256 of the secret part, as HashAPISecret computes it.
	Hash [sha256.Size]byte

	full *string
}

// Key returns the whole key, nopacs_<id>_<secret>, to be shown once.
func (k NewAPIKey) Key() string {
	if k.full == nil {
		return ""
	}
	return *k.full
}

// Format prints the key's id only, for every verb.
func (k NewAPIKey) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, "secret.NewAPIKey{ID: "+k.ID+"}")
}

// LogValue gives log/slog the key's id only.
func (k NewAPIKey) LogValue() slog.Value {
	return slog.GroupValue(slog.String("id", k.ID))
}

// MarshalJSON writes the key's id only, as {"id":"..."}.
func (k NewAPIKey) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID string `json:"id"`
	}{k.ID})
}

// MakeAPIKey makes a new API key from the system's random source.
func MakeAPIKey() (NewAPIKey, error) {
	id, err := randomBase58(idLength)
	if err != nil {
		return NewAPIKey{}, err
	}
	secret, err := randomBase58(secretLength)
	if err != nil {
		return NewAPIKey{}, err
	}
	full := apiKeyPrefix + id + "_" + secret
	return NewAPIKey{
		ID:   id,
		Hash: HashAPISecret(secret),
		full: &full,
	}, nil
}

// randomBase58 returns n characters drawn uniformly from the base58 alphabet.
func randomBase58(n int) (string, error) {
	out := make([]byte, 0, n)
	// About one byte in eleven is rejected (24 of 256 values), so twice n
	// bytes almost always suffice in one read.
	buf := make([]byte, 2*n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fault.Wrap(fault.Internal, err, "secret: no random key could be made")
		}
		for _, b := range buf {
			if int(b) >= base58Limit {
				continue
			}
			out = append(out, base58[int(b)%len(base58)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}

// ParseAPIKey splits a presented key into its id and its secret. Anything
// that is not exactly nopacs_, 12 base58 characters, an underscore and 44 base58
// characters is refused with kind Unauthenticated and one fixed message,
// which never quotes the key.
func ParseAPIKey(presented string) (id, secret string, err error) {
	if len(presented) != apiKeyLength ||
		!strings.HasPrefix(presented, apiKeyPrefix) ||
		presented[len(apiKeyPrefix)+idLength] != '_' {
		return "", "", fault.New(fault.Unauthenticated, invalidAPIKey)
	}
	id = presented[len(apiKeyPrefix) : len(apiKeyPrefix)+idLength]
	secret = presented[len(apiKeyPrefix)+idLength+1:]
	if !isBase58(id) || !isBase58(secret) {
		return "", "", fault.New(fault.Unauthenticated, invalidAPIKey)
	}
	return id, secret, nil
}

// isBase58 reports whether every byte of s is in the base58 alphabet.
func isBase58(s string) bool {
	for i := range len(s) {
		if strings.IndexByte(base58, s[i]) < 0 {
			return false
		}
	}
	return true
}

// HashAPISecret returns the SHA-256 of an API key's secret part, taken over
// its characters as ASCII. A plain hash is enough: the secret is 256 random
// bits, so there is no dictionary to try, and a slow hash would only make
// every request cost more (NIST SP 800-63B-4, 3.1.2.2).
func HashAPISecret(secret string) [sha256.Size]byte {
	return sha256.Sum256([]byte(secret))
}

// VerifyAPISecret reports whether secret hashes to stored. The comparison
// takes the same time wherever the two hashes first differ, so the time a
// refusal takes says nothing about how close a guess was.
func VerifyAPISecret(secret string, stored []byte) bool {
	h := HashAPISecret(secret)
	return subtle.ConstantTimeCompare(h[:], stored) == 1
}
