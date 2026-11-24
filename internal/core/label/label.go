package label

import (
	"strconv"
	"strings"
)

// MaxLength is the longest label, in bytes. Three labels and an instance's
// file name make a data key of at most 460 bytes, under the 1024 bytes of an
// S3 key, and each stays far under the 255 bytes most file systems allow a
// path segment.
const MaxLength = 128

// Fault says how a value breaks the rule for a label.
type Fault int

// The faults, in the order Check looks for them.
const (
	// Empty is a value with no characters.
	Empty Fault = iota + 1
	// TooLong is a value longer than MaxLength bytes.
	TooLong
	// BadCharacter is a value holding a byte outside A-Z, a-z, 0-9, ".",
	// "_" and "-".
	BadCharacter
	// LeadingDot is a value starting with ".".
	LeadingDot
	// TrailingDot is a value ending with ".".
	TrailingDot
	// DeviceName is a value Windows reserves for a device, such as "CON"
	// or "com1.txt".
	DeviceName
)

// String returns the fault in words, as it appears in an error.
func (f Fault) String() string {
	switch f {
	case Empty:
		return "is empty"
	case TooLong:
		return "is longer than " + strconv.Itoa(MaxLength) + " bytes"
	case BadCharacter:
		return "holds a character other than A-Z, a-z, 0-9, '.', '_' and '-'"
	case LeadingDot:
		return "starts with a dot"
	case TrailingDot:
		return "ends with a dot"
	case DeviceName:
		return "is a name Windows reserves for a device"
	}
	return "Fault(" + strconv.Itoa(int(f)) + ")"
}

// Error is a value refused as a label.
type Error struct {
	// Value is the refused value. It is not part of the error's text,
	// because it may identify a patient; a caller that may show it, such as
	// a quarantine record, reads it here.
	Value string
	// Fault is how the value breaks the rule.
	Fault Fault
	// Index is the byte offset of the first offending byte for
	// BadCharacter, and -1 otherwise.
	Index int
}

// Error returns the fault, and for a bad character its position and byte,
// without the value.
func (e *Error) Error() string {
	if e.Fault == BadCharacter && e.Index >= 0 && e.Index < len(e.Value) {
		return "label " + e.Fault.String() + ": byte " + strconv.Quote(e.Value[e.Index:e.Index+1]) +
			" at offset " + strconv.Itoa(e.Index)
	}
	return "label " + e.Fault.String()
}

// Check returns nil if s is a valid label, else an *Error naming its fault.
func Check(s string) error {
	fail := func(f Fault, i int) error { return &Error{Value: s, Fault: f, Index: i} }
	switch {
	case s == "":
		return fail(Empty, -1)
	case len(s) > MaxLength:
		return fail(TooLong, -1)
	}
	for i := 0; i < len(s); i++ {
		if !Allowed(s[i]) {
			return fail(BadCharacter, i)
		}
	}
	switch {
	case s[0] == '.':
		return fail(LeadingDot, -1)
	case s[len(s)-1] == '.':
		return fail(TrailingDot, -1)
	case IsDeviceName(s):
		return fail(DeviceName, -1)
	}
	return nil
}

// Valid reports whether s is a valid label.
func Valid(s string) bool { return Check(s) == nil }

// Allowed reports whether b may appear in a label: A-Z, a-z, 0-9, ".", "_"
// and "-". None of them is a path separator or has a special meaning in a
// file name on Linux, macOS or Windows, in a URL path, or in an S3 key.
func Allowed(b byte) bool {
	return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9' ||
		b == '.' || b == '_' || b == '-'
}

// IsDeviceName reports whether s names a Windows device: CON, PRN, AUX, NUL,
// COM0 to COM9 or LPT0 to LPT9, in any case, alone or followed by an
// extension ("NUL.txt" and "NUL.tar.gz" are both NUL on Windows). Microsoft's
// "Naming Files, Paths, and Namespaces" lists COM1 to COM9 and LPT1 to LPT9;
// COM0 and LPT0 are refused as well because earlier versions of that page
// listed them and refusing two more names costs nothing. The superscript
// forms Windows also reserves (COM¹, LPT³ and the like) cannot reach this
// function through Check, whose character rule refuses any byte outside
// ASCII, but are recognized here too so the function answers what its name
// says.
func IsDeviceName(s string) bool {
	base, _, _ := strings.Cut(s, ".")
	switch strings.ToUpper(base) {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(base) < 4 {
		return false
	}
	switch strings.ToUpper(base[:3]) {
	case "COM", "LPT":
	default:
		return false
	}
	switch rest := base[3:]; rest {
	case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
		return true
	}
	return false
}

// Path is the three labels that place a scan: its subject's, its session's
// and its own, in that order. The naming rule makes one; the layout of a
// storage turns one into keys.
type Path struct {
	Subject, Session, Scan string
}

// Check returns nil if each of the three labels is valid, else the first
// fault, as a *PathError naming the level.
func (p Path) Check() error {
	for _, l := range []struct {
		level, value string
	}{{"subject", p.Subject}, {"session", p.Session}, {"scan", p.Scan}} {
		if err := Check(l.value); err != nil {
			return &PathError{Level: l.level, Err: err}
		}
	}
	return nil
}

// PathError is a fault of one label of a Path.
type PathError struct {
	// Level is "subject", "session" or "scan".
	Level string
	// Err is the label's *Error.
	Err error
}

// Error returns the level and the fault, without the value.
func (e *PathError) Error() string { return "the " + e.Level + " " + e.Err.Error() }

// Unwrap returns the label's error, so errors.As finds its *Error.
func (e *PathError) Unwrap() error { return e.Err }

// Fold returns the form of a valid label that labels colliding with it share
// on a file system that ignores case, as the default ones of macOS and
// Windows do: its ASCII letters in lowercase. Two different labels with the
// same folded form must not be held at the same level of one tenant, or one
// would be written into the other's directory. Labels are ASCII, so no
// Unicode case rule or normalization is involved.
func Fold(s string) string {
	return strings.ToLower(s)
}
