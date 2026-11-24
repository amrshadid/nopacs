package uid

import (
	"errors"
	"strconv"
)

// MaxLength is the longest UID, in bytes (PS3.5 9.1: "shall not exceed 64
// characters").
const MaxLength = 64

// The faults Check reports. Their text never carries the value, though a UID
// identifies nothing about a person by itself, so every error of the core
// about a value is written one way.
var (
	ErrEmpty     = errors.New("UID is empty")
	ErrTooLong   = errors.New("UID is longer than 64 bytes")
	ErrCharacter = errors.New("UID holds a character other than 0-9 and '.'")
	// ErrComponent is an empty component: a leading, trailing or doubled dot.
	ErrComponent = errors.New("UID has an empty component")
	// ErrLeadingZero is a component of more than one digit that starts
	// with 0, which PS3.5 9.1 forbids ("the first digit of each component
	// shall not be zero unless the component is a single digit").
	ErrLeadingZero = errors.New("UID has a component with a leading zero")
)

// Check returns nil if s is a well-formed UID under PS3.5 9.1: 1 to 64 bytes
// of numeric components separated by single dots, each component either "0"
// or digits not starting with 0. It does not require the first component to
// be 0, 1 or 2, as an ISO object identifier would (ITU-T X.660): PS3.5 9.1
// does not state it, and an archive that refused such UIDs would refuse data
// other systems accept.
func Check(s string) error {
	switch {
	case s == "":
		return ErrEmpty
	case len(s) > MaxLength:
		return ErrTooLong
	}
	start := 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] != '.' {
			if s[i] < '0' || s[i] > '9' {
				return errAt(ErrCharacter, i)
			}
			continue
		}
		switch n := i - start; {
		case n == 0:
			return errAt(ErrComponent, i)
		case n > 1 && s[start] == '0':
			return errAt(ErrLeadingZero, start)
		}
		start = i + 1
	}
	return nil
}

// Valid reports whether s is a well-formed UID.
func Valid(s string) bool { return Check(s) == nil }

// Error is a fault of Check with the byte offset where it was found.
type Error struct {
	// Fault is one of the Err values of this package.
	Fault error
	// Offset is the byte offset in the value.
	Offset int
}

// Error returns the fault and its offset.
func (e *Error) Error() string { return e.Fault.Error() + " at offset " + strconv.Itoa(e.Offset) }

// Unwrap returns the fault, so errors.Is matches it.
func (e *Error) Unwrap() error { return e.Fault }

func errAt(fault error, offset int) error { return &Error{Fault: fault, Offset: offset} }
