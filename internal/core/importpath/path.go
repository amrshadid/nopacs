package importpath

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/amrshadid/nopacs/internal/core/pathname"
)

// The bounds of a path. 255 bytes is the longest name most file systems
// take (ext4, XFS, APFS). 2048 bytes is the longest path a store keeps
// where an index orders it: a Postgres B-tree refuses an index row longer
// than a third of its 8 KiB page, 2704 bytes (Postgres manual, "B-Tree
// Indexes", 67.4.1), and the widest import index holds a path with a job
// id (64 bytes), a unit id and a kind beside it, so 2048 leaves room for
// those and the row's own header. It is also well below Linux's PATH_MAX
// (4096), so no path the rule takes is one the server cannot open.
//
// MaxEntryPathLength is the longest path a walk meets: a name in a
// directory whose path fits MaxPathLength. Such an entry is not handed in
// or entered; it is skipped as too long (CheckEntry, Fitting).
const (
	MaxSegmentLength   = 255
	MaxPathLength      = 2048
	MaxEntryPathLength = MaxPathLength + 1 + MaxSegmentLength
)

// Fault says why a path breaks the rule.
type Fault int

// The faults.
const (
	// Empty is the empty path. A job's path may be empty, for the whole
	// root; a caller that allows it checks for "" before Check.
	Empty Fault = iota + 1
	// TooLong is a path longer than MaxPathLength bytes.
	TooLong
	// Absolute is a path starting with "/".
	Absolute
	// TrailingSlash is a path ending with "/".
	TrailingSlash
	// EmptySegment is "//" inside a path.
	EmptySegment
	// SegmentTooLong is a segment longer than MaxSegmentLength bytes.
	SegmentTooLong
	// DotSegment is a segment "." or "..", which would name the directory
	// itself or leave it.
	DotSegment
	// BadByte is a segment holding "\", ":" or NUL: a separator or a drive
	// on Windows, or the end of a name to the system's C interface.
	BadByte
	// NotUTF8 is a segment that is not valid UTF-8.
	NotUTF8
	// EntryTooLong is an entry's path longer than MaxEntryPathLength
	// bytes, which no walk that enters only directories whose paths fit
	// can meet (CheckEntry).
	EntryTooLong
)

// String returns the fault in words, as it appears in an error.
func (f Fault) String() string {
	switch f {
	case Empty:
		return "is empty"
	case TooLong:
		return "is longer than " + strconv.Itoa(MaxPathLength) + " bytes"
	case Absolute:
		return "starts with \"/\""
	case TrailingSlash:
		return "ends with \"/\""
	case EmptySegment:
		return "is empty"
	case SegmentTooLong:
		return "is longer than " + strconv.Itoa(MaxSegmentLength) + " bytes"
	case DotSegment:
		return "is \".\" or \"..\""
	case BadByte:
		return "holds \"\\\", \":\" or NUL"
	case NotUTF8:
		return "is not valid UTF-8"
	case EntryTooLong:
		return "is longer than " + strconv.Itoa(MaxEntryPathLength) + " bytes"
	}
	return "Fault(" + strconv.Itoa(int(f)) + ")"
}

// PathError is a path that breaks the rule. Its text names the segment by
// its number, from 1, never the path or the segment's text.
type PathError struct {
	// Segment is the number of the segment at fault, from 1, or 0 when the
	// fault is the whole path's.
	Segment int
	// Fault is what is wrong.
	Fault Fault
}

// Error returns the segment's number and the fault.
func (e *PathError) Error() string {
	if e.Segment == 0 {
		return "importpath: the path " + e.Fault.String()
	}
	return "importpath: segment " + strconv.Itoa(e.Segment) + " of the path " + e.Fault.String()
}

// Check returns nil when p follows the rule of a path relative to an
// import root, or a *PathError. The empty path is refused (Empty).
func Check(p string) error {
	if len(p) > MaxPathLength {
		return &PathError{Fault: TooLong}
	}
	return checkShape(p)
}

// CheckEntry returns nil, and whether p fits MaxPathLength, when p follows
// the rule of a path but for its length, which may reach
// MaxEntryPathLength: the path of an entry a walk meets. An entry that
// does not fit is skipped as too long, and recorded under the deepest of
// its ancestors that fits (Fitting). The empty path is refused (Empty).
func CheckEntry(p string) (bool, error) {
	if len(p) > MaxEntryPathLength {
		return false, &PathError{Fault: EntryTooLong}
	}
	if err := checkShape(p); err != nil {
		return false, err
	}
	return len(p) <= MaxPathLength, nil
}

// Fitting returns the deepest of p and its ancestors whose path is at most
// MaxPathLength bytes: p itself when it fits, and otherwise the path cut
// at a "/". It returns "" when no ancestor fits, which no path CheckEntry
// takes can give, since a segment is at most MaxSegmentLength bytes.
func Fitting(p string) string {
	for len(p) > MaxPathLength {
		i := strings.LastIndexByte(p, '/')
		if i < 0 {
			return ""
		}
		p = p[:i]
	}
	return p
}

// checkShape returns the fault of p but for its length: empty, absolute,
// a trailing "/", or a segment's.
func checkShape(p string) error {
	switch {
	case p == "":
		return &PathError{Fault: Empty}
	case p[0] == '/':
		return &PathError{Fault: Absolute}
	case p[len(p)-1] == '/':
		return &PathError{Fault: TrailingSlash}
	}
	n := 0
	for seg := range strings.SplitSeq(p, "/") {
		n++
		if err := checkSegment(seg); err != 0 {
			return &PathError{Segment: n, Fault: err}
		}
	}
	return nil
}

// checkSegment returns the fault of one segment, or 0.
func checkSegment(s string) Fault {
	switch {
	case s == "":
		return EmptySegment
	case len(s) > MaxSegmentLength:
		return SegmentTooLong
	case s == "." || s == "..":
		return DotSegment
	case strings.ContainsAny(s, "\\:\x00"):
		return BadByte
	case !utf8.ValidString(s):
		return NotUTF8
	}
	return 0
}

// Compare returns -1, 0 or +1 as a comes before, is, or comes after b in
// the order an import walks a tree: segment by segment, each segment byte
// by byte, a path before every path it is a prefix of. So a directory's
// whole subtree comes before the next name in that directory: "a/b/c"
// before "a/b.d" before "a-c", which plain byte order of whole paths
// would give as "a-c", "a/b.d", "a/b/c" ("-" and "." being below "/").
// It is byte order with "/" taken as lower than every other byte, which no
// segment holds; a unit's cursor is a path in this order, and a walk
// resumes after it.
func Compare(a, b string) int {
	n := min(len(a), len(b))
	for i := range n {
		x, y := a[i], b[i]
		if x == y {
			continue
		}
		switch {
		case x == '/':
			return -1
		case y == '/':
			return 1
		case x < y:
			return -1
		}
		return 1
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// Overlap reports whether two paths under one root read some of the same
// files: they are one path, or one lies under the other, segment by
// segment, so "a" overlaps "a/b" and not "ab". The empty path, the whole
// root, overlaps every path. Case is folded (pathname.Nest), since a file
// system that ignores case makes two spellings one directory; folding
// where a file system does not only refuses more.
func Overlap(a, b string) bool {
	return pathname.Nest(segments(a), segments(b))
}

// segments returns p's segments; none for the empty path.
func segments(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}
