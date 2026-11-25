package storageswitch

import (
	"crypto/hmac"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// MACKey makes the MAC of a confirm token under a key only the server
// holds: secret.DerivedKey derived for secret.ConfirmToken, the key a
// delete's preview token is made with.
type MACKey interface {
	// MAC returns the HMAC-SHA-256 (RFC 2104) of message.
	MAC(message []byte) ([]byte, error)
}

// TokenLife is how long a confirm token matches after its issue
// (replica.proto, SwitchStorageResponse.confirm).
const TokenLife = time.Hour

// tokenSkew is how far in the future a token's issue time may be and still
// match: two servers' clocks, or a clock stepped back, a little apart.
const tokenSkew = time.Minute

// tokenPrefix starts every switch token, so a delete's or a recall's token
// is never taken for one.
const tokenPrefix = "w1"

// tokenMessage returns what a switch token's MAC is made over: its kind and
// version, the tenant's id, the time it was issued, both storages' names
// and places (and the replica's role and tier), and the primary's switch
// generation. The tenant's id is in it because every tenant's tokens are
// made under the one confirm key of the installation, and two tenants may
// name storages alike, even on the same places: a token one tenant's
// preview gave never matches on another. Each part is written with its
// length, so no two lists of parts make the same text.
func tokenMessage(tenantID string, issued time.Time, f tenant.SwitchFacts) []byte {
	var b strings.Builder
	w := func(parts ...string) {
		for _, s := range parts {
			b.WriteString(strconv.Itoa(len(s)))
			b.WriteByte(':')
			b.WriteString(s)
		}
	}
	bit := func(v bool) string { return strconv.FormatBool(v) }
	place := func(s tenant.Storage) {
		w(s.Name, string(s.Type), s.Directory.Root, s.Directory.Path, s.S3.Endpoint, s.S3.Region, s.S3.Bucket, s.S3.Prefix,
			bit(s.S3.PathStyle), s.S3.AccessKeyID, s.ReplicaOf, string(s.Tier))
	}
	w("nopacs-switch-confirm-1", tenantID, strconv.FormatInt(issued.UnixMicro(), 10))
	place(f.Primary)
	place(f.Replica)
	w(strconv.FormatInt(f.Primary.SwitchGeneration, 10))
	return []byte(b.String())
}

// makeToken returns the confirm token of a preview of f, in the tenant of
// tenantID, issued at issued: the prefix, the issue time in microseconds
// and the MAC in lowercase hex, 86 bytes, within tenant.MaxSwitchToken.
func makeToken(key MACKey, tenantID string, issued time.Time, f tenant.SwitchFacts) (string, error) {
	issued = issued.UTC().Truncate(time.Microsecond)
	mac, err := key.MAC(tokenMessage(tenantID, issued, f))
	if err != nil {
		return "", err
	}
	return tokenPrefix + "." + strconv.FormatInt(issued.UnixMicro(), 10) + "." + hex.EncodeToString(mac), nil
}

// splitToken returns the issue time and MAC of a switch token, and false
// for a string that is not one.
func splitToken(token string) (time.Time, []byte, bool) {
	f := strings.Split(token, ".")
	if len(f) != 3 || f[0] != tokenPrefix || len(f[2]) != 64 {
		return time.Time{}, nil, false
	}
	v, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil || v < 0 {
		return time.Time{}, nil, false
	}
	mac, err := hex.DecodeString(f[2])
	if err != nil {
		return time.Time{}, nil, false
	}
	return time.UnixMicro(v).UTC(), mac, true
}

// tokenMatches reports whether mac is the MAC key makes for a preview of f,
// in the tenant of tenantID, issued at issued.
func tokenMatches(key MACKey, tenantID string, issued time.Time, mac []byte, f tenant.SwitchFacts) (bool, error) {
	want, err := key.MAC(tokenMessage(tenantID, issued, f))
	if err != nil {
		return false, err
	}
	return hmac.Equal(mac, want), nil
}
