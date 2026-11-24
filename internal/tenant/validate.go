package tenant

import (
	"net/netip"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amrshadid/nopacs/internal/core/uid"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// The bounds of the pipeline's values. Each is the API contract's where it
// states one; the others keep a hostile or broken caller from making a row,
// a transaction or an answer unbounded.
const (
	// MaxReceivedBatch is the most received files one AddReceivedFiles
	// records. A receiver writes a batch a few times a second; this bounds
	// the time one transaction holds the writer.
	MaxReceivedBatch = 1000
	// MaxDuplicateBatch is the most files one CheckDuplicates asks about.
	MaxDuplicateBatch = 1000
	// MaxQuarantineBatch is the most records one HoldFiles writes.
	MaxQuarantineBatch = 1000
	// MaxArrivals is the most arrivals Reception returns with a reception:
	// the contract's 100 latest.
	MaxArrivals = 100
	// MaxStepFilter is the most steps a reception filter names.
	MaxStepFilter = 16
	// MaxFailureLength bounds a reception's failure, a phrase for a person.
	MaxFailureLength = 256
	// MaxDecisionReasonLength bounds the reason a person gives to discard a
	// reception or reject a file.
	MaxDecisionReasonLength = 1 << 10
	// MaxIncomingLength bounds where a file is in the incoming area.
	MaxIncomingLength = 512
	// MaxAETitleLength is an AE title's bound (PS3.5 6.2, value
	// representation AE).
	MaxAETitleLength = 16
	// MaxCallingAETitles is how many calling AE titles a reception keeps.
	MaxCallingAETitles = 16
	// MaxAddressLength bounds a caller's address with its port.
	MaxAddressLength = 64
	// MaxWorkTextLength bounds a unit of work's kind, subject and unique
	// key, and its last error.
	MaxWorkTextLength = 256
	// MinLease is the shortest lease a claim or a renewal takes.
	MinLease = time.Second
	// MaxLease is the longest lease a claim or a renewal takes.
	MaxLease = time.Hour
	// MaxWorkAttempts bounds a unit's allowed attempts.
	MaxWorkAttempts = 100
)

// SHA256 is a SHA-256 checksum.
type SHA256 [32]byte

// IsZero reports whether the checksum is all zero, which stands for none.
func (s SHA256) IsZero() bool { return s == SHA256{} }

// String returns the checksum in lowercase hexadecimal, or the empty string
// for none.
func (s SHA256) String() string {
	if s.IsZero() {
		return ""
	}
	const digits = "0123456789abcdef"
	b := make([]byte, 64)
	for i, c := range s {
		b[2*i], b[2*i+1] = digits[c>>4], digits[c&15]
	}
	return string(b)
}

// Caller is who sent a file: the AE titles the association gave and the
// address its connection came from, with its port.
type Caller struct {
	// CallingAETitle is the caller's AE title, without padding; empty when
	// it was not read.
	CallingAETitle string
	// CalledAETitle is the AE title the caller called; empty when not read.
	CalledAETitle string
	// Address is the connection's address and port, as "192.0.2.7:51234"
	// or "[2001:db8::7]:51234"; empty when not read.
	Address string
}

// Validate returns an Invalid error unless each part is empty or well
// formed.
func (c Caller) Validate() error {
	for _, ae := range []string{c.CallingAETitle, c.CalledAETitle} {
		if ae != "" {
			if err := ValidateAETitle(ae); err != nil {
				return err
			}
		}
	}
	if c.Address != "" {
		if len(c.Address) > MaxAddressLength {
			return invalid("a caller's address is at most " + strconv.Itoa(MaxAddressLength) + " bytes")
		}
		if _, err := netip.ParseAddrPort(c.Address); err != nil {
			return invalid("a caller's address is an IP address with its port")
		}
	}
	return nil
}

// ValidateAETitle returns an Invalid error unless ae is 1 to 16 characters
// of printable ASCII other than backslash, not only spaces, and without
// leading or trailing spaces, which are not significant and are removed
// before an AE title reaches a store (PS3.5 6.2).
func ValidateAETitle(ae string) error {
	if ae == "" || len(ae) > MaxAETitleLength || strings.TrimSpace(ae) != ae ||
		!allBytes(ae, func(b byte) bool { return b >= 0x20 && b < 0x7f && b != '\\' }) {
		return invalid("an AE title is 1 to 16 characters of printable ASCII other than backslash, with no leading or trailing space")
	}
	return nil
}

// ValidateName returns an Invalid error unless name follows the rule of a
// tenant's name, which storages and receivers share: their names appear in
// paths, URLs and keys.
func ValidateName(what, name string) error {
	if err := master.ValidateTenantName(name); err != nil {
		return fault.Wrap(fault.Invalid, err, "a "+what+"'s name is 1 to 63 of a-z, 0-9 and -, starting with a letter, and not a device name Windows reserves")
	}
	return nil
}

// validateUID returns an Invalid error naming what unless s is a UID
// (PS3.5 9.1), checked by the core.
func validateUID(what, s string) error {
	if err := uid.Check(s); err != nil {
		return fault.Wrap(fault.Invalid, err, "a "+what+" is a UID: 1 to 64 of digits and dots (PS3.5 9.1)")
	}
	return nil
}

// validateText returns an Invalid error unless s is UTF-8 of min to max
// bytes with no NUL byte, the master's rule for text: Postgres refuses
// invalid UTF-8 and NUL in a text column and SQLite would keep them, and the
// drivers must agree.
func validateText(what, s string, min, max int) error {
	if len(s) < min || len(s) > max {
		if min > 0 {
			return invalid("a " + what + " is " + strconv.Itoa(min) + " to " + strconv.Itoa(max) + " bytes")
		}
		return invalid("a " + what + " is at most " + strconv.Itoa(max) + " bytes")
	}
	if !utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0 {
		return invalid("a " + what + " is UTF-8 with no NUL byte")
	}
	return nil
}

// validateReason returns an Invalid error unless a person's reason is 1 to
// 1024 bytes of UTF-8 and not only spaces, the contract's rule.
func validateReason(what, s string) error {
	if err := validateText(what, s, 1, MaxDecisionReasonLength); err != nil {
		return err
	}
	if strings.TrimSpace(s) == "" {
		return invalid("a " + what + " is not only spaces")
	}
	return nil
}

// validateIncoming returns an Invalid error unless where is a relative path
// of segments of ASCII letters, digits, ".", "_" and "-", none empty or
// starting with a dot, so it can never leave the incoming area it is joined
// to.
func validateIncoming(where string) error {
	if where == "" || len(where) > MaxIncomingLength {
		return invalid("a place in the incoming area is 1 to " + strconv.Itoa(MaxIncomingLength) + " bytes")
	}
	for seg := range strings.SplitSeq(where, "/") {
		if seg == "" || seg[0] == '.' || !allBytes(seg, pathByte) {
			return invalid("a place in the incoming area is segments of A-Z, a-z, 0-9, ., _ and -, joined by /, none empty or starting with a dot")
		}
	}
	return nil
}

// pathByte reports whether b may be in a segment of a path the port stores.
func pathByte(b byte) bool {
	return isAlnum(b) || b == '.' || b == '_' || b == '-'
}

// isAlnum reports whether b is an ASCII letter or digit.
func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// allBytes reports whether ok holds for every byte of s.
func allBytes(s string, ok func(byte) bool) bool {
	for i := 0; i < len(s); i++ {
		if !ok(s[i]) {
			return false
		}
	}
	return true
}

// invalid returns an Invalid error with a message for the caller.
func invalid(message string) error {
	return fault.New(fault.Invalid, message)
}
