package master

import (
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/amrshadid/nopacs/internal/fault"
)

// The range of times the store keeps. A time is kept as microseconds since
// 1970 in a 64-bit integer by the SQL drivers; the range is narrower than
// that allows so that every driver can keep it, and wide enough for any date
// an archive records.
var (
	minTime = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	maxTime = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
)

// ValidateTime returns an Invalid error unless t is in the years 1970 to
// 9999. The zero time is outside them, so a time the caller forgot to set is
// refused rather than stored as year 1.
func ValidateTime(name string, t time.Time) error {
	if t.Before(minTime) || !t.Before(maxTime) {
		return invalid("the " + name + " is between the years 1970 and 9999")
	}
	return nil
}

// ValidateID returns an Invalid error unless id is 1 to 64 of A-Z, a-z, 0-9,
// "_" and "-". Ids are opaque to the store, but appear in cursors, logs and
// paths, so they are held to characters that need no escaping anywhere.
func ValidateID(name, id string) error {
	if id == "" || len(id) > MaxIDLength || !allBytes(id, func(b byte) bool {
		return isLowerAlnum(b) || (b >= 'A' && b <= 'Z') || b == '_' || b == '-'
	}) {
		return invalid("a " + name + " is 1 to " + strconv.Itoa(MaxIDLength) + " of A-Z, a-z, 0-9, _ and -")
	}
	return nil
}

// validateText returns an Invalid error unless s is valid UTF-8 of min to max
// bytes with no NUL byte. Invalid UTF-8 and NUL are refused because Postgres
// refuses both in a text column and SQLite would keep them, and the two
// drivers must agree.
func validateText(name, s string, min, max int) error {
	if len(s) < min || len(s) > max {
		if min > 0 {
			return invalid("a " + name + " is " + strconv.Itoa(min) + " to " + strconv.Itoa(max) + " bytes")
		}
		return invalid("a " + name + " is at most " + strconv.Itoa(max) + " bytes")
	}
	if !utf8.ValidString(s) {
		return invalid("a " + name + " is UTF-8")
	}
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return invalid("a " + name + " holds no NUL byte")
		}
	}
	return nil
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

// isLowerAlnum reports whether b is a-z or 0-9.
func isLowerAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

// invalid returns an Invalid error with a message for the caller.
func invalid(message string) error {
	return fault.New(fault.Invalid, message)
}
