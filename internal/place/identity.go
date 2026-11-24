package place

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/text/unicode/norm"

	"github.com/amrshadid/nopacs/internal/core/pathname"
)

// overlap reports whether the directories two absolute paths stand for,
// each already resolved through its symbolic links (resolveLoose), are one
// directory or one lies inside the other.
//
// Names alone cannot tell. A file system that ignores Unicode
// normalization (APFS and HFS+ do) makes "é" written as one code point and
// as "e" with a combining accent one directory; a bind mount, or one share
// mounted twice, puts one directory under two unrelated paths. So the names
// are compared first, folded in case and to Unicode NFC (UAX #15), and a
// match is final; then the directories themselves: the deepest existing
// part of each path is found, and each directory at or above one is tested
// with os.SameFile (device and inode, or the volume and file index on
// Windows) against the other's deepest existing part, both ways. On a match,
// what lies below in each path is compared by name, since a part not made
// yet has no identity to compare.
//
// A part the server cannot examine, other than one that is not there, is
// an error: whether the two lie apart cannot be known.
func overlap(a, b string) (bool, error) {
	if nestNames(segments(a), segments(b)) {
		return true, nil
	}
	aDir, aInfo, aTail, err := deepestExisting(a)
	if err != nil {
		return false, err
	}
	bDir, bInfo, bTail, err := deepestExisting(b)
	if err != nil {
		return false, err
	}
	if hit, err := reaches(aDir, aTail, bInfo, bTail); err != nil || hit {
		return hit, err
	}
	return reaches(bDir, bTail, aInfo, aTail)
}

// nestNames is pathname.Nest over segments folded to Unicode NFC, so two
// spellings of one name, composed and decomposed, are one name.
func nestNames(a, b []string) bool {
	return pathname.Nest(nfc(a), nfc(b))
}

// nfc returns segs, each in Unicode normalization form C.
func nfc(segs []string) []string {
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = norm.NFC.String(s)
	}
	return out
}

// deepestExisting returns the longest part of the clean absolute path p
// that exists, what os.Stat answers for it, and the segments of p below it.
func deepestExisting(p string) (string, fs.FileInfo, []string, error) {
	p = filepath.Clean(p)
	var tail []string
	for {
		info, err := os.Stat(p)
		if err == nil {
			return p, info, tail, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", nil, nil, err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", nil, nil, err
		}
		tail = append([]string{filepath.Base(p)}, tail...)
		p = parent
	}
}

// reaches reports whether a directory at or above dir, the deepest
// existing part of a path whose segments below it are tail, is the
// directory other, the deepest existing part of a second path whose
// segments below it are otherTail; and, when it is, whether the two paths
// below that directory are one or one inside the other by name.
func reaches(dir string, tail []string, other fs.FileInfo, otherTail []string) (bool, error) {
	var below []string
	for p := dir; ; {
		info, err := os.Stat(p)
		if err != nil {
			return false, err
		}
		if os.SameFile(info, other) {
			return nestNames(append(below, tail...), otherTail), nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false, nil
		}
		below = append([]string{filepath.Base(p)}, below...)
		p = parent
	}
}
