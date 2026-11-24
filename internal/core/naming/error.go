package naming

import (
	"errors"
	"strconv"

	"github.com/amrshadid/nopacs/internal/core/label"
)

// Fault says why a rule could not make a label from a file's attributes.
type Fault int

// The faults.
const (
	// Missing is an attribute the data set does not hold.
	Missing Fault = iota + 1
	// Empty is an attribute, or a part of the pattern, with no value.
	Empty
	// TooLong is a value longer than a label (128 bytes), or, for the
	// pattern's attribute, longer than MaxValueLength.
	TooLong
	// BadCharacter is a value holding a byte a label may not hold.
	BadCharacter
	// BadUID is a value of a UID attribute that is not a well-formed UID
	// (PS3.5 9.1).
	BadUID
	// NoMatch is a value the pattern does not match whole.
	NoMatch
	// BadLabel is a label made of valid values that is still not a valid
	// label: too long once joined, starting or ending with a dot, or a
	// Windows device name.
	BadLabel
	// NoHasher is a {#...} placeholder with no Hasher given to Labels.
	NoHasher
	// BadHash is a Hasher that returned something other than HashLength
	// characters of the lower case base32 alphabet: a mistake of the
	// caller, refused so that a label never holds what a hasher let
	// through.
	BadHash
)

// String returns the fault in words, as it appears in an error.
func (f Fault) String() string {
	switch f {
	case Missing:
		return "is missing"
	case Empty:
		return "is empty"
	case TooLong:
		return "is too long"
	case BadCharacter:
		return "holds a character other than A-Z, a-z, 0-9, '.', '_' and '-'"
	case BadUID:
		return "is not a well-formed UID"
	case NoMatch:
		return "does not match the pattern"
	case NoHasher:
		return "is to be hashed, and no hasher was given"
	case BadHash:
		return "was hashed to text that is not a keyed hash's"
	case BadLabel:
		return "do not make a valid label"
	}
	return "Fault(" + strconv.Itoa(int(f)) + ")"
}

// Error is a file whose attributes the rule could not make a label from.
// Its text names the label, the attribute and the fault, never the value,
// which can identify a patient before de-identification; the value is in
// Err's *label.Error where there is one, for a caller entitled to it.
type Error struct {
	// Level is "subject", "session" or "scan".
	Level string
	// Attribute is the attribute as the rule writes it, such as
	// "PatientID" or "(0010,4000)". For BadLabel it lists every attribute
	// and part the label was made of, comma separated.
	Attribute string
	// Part is the pattern's part, when the fault is in one.
	Part string
	// Fault is what is wrong.
	Fault Fault
	// Err is the underlying fault from package label or uid, or nil.
	Err error
}

// Error returns the label, the attribute and the fault.
func (e *Error) Error() string {
	s := "naming: the " + e.Level + " label: "
	switch {
	case e.Fault == BadLabel:
		s += "the values of " + e.Attribute + " " + e.Fault.String()
	case e.Part != "":
		s += "part " + e.Part + " of " + e.Attribute + " " + e.Fault.String()
	default:
		s += e.Attribute + " " + e.Fault.String()
	}
	var le *label.Error
	switch {
	case e.Fault == BadCharacter && errors.As(e.Err, &le) && le.Index >= 0 && le.Index < len(le.Value):
		s += ": byte " + strconv.Quote(le.Value[le.Index:le.Index+1]) + " at offset " + strconv.Itoa(le.Index)
	case e.Err != nil:
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap returns the underlying fault, so errors.As finds a *label.Error or
// errors.Is a uid fault.
func (e *Error) Unwrap() error { return e.Err }
