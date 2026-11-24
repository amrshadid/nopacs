package pathname

import "strings"

// MaxNameLength is the longest name: a name appears in URLs and paths, and
// 63 is the longest DNS label, so a name can also be a host name's label.
const MaxNameLength = 63

// WellFormedName reports whether name is 1 to MaxNameLength of a-z, 0-9 and
// "-", starting with a letter: the characters every file system, URL and
// DNS label takes as they are, in one case, so two spellings never name one
// thing.
func WellFormedName(name string) bool {
	if name == "" || len(name) > MaxNameLength || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for i := 0; i < len(name); i++ {
		b := name[i]
		if (b < 'a' || b > 'z') && (b < '0' || b > '9') && b != '-' {
			return false
		}
	}
	return true
}

// ValidName reports whether name follows the rule of a name: well formed
// (WellFormedName) and not a device name Windows reserves (DeviceName). A
// tenant on SQLite keeps its database in a file named for it, and a tenant's
// directory under a root is named for it, so a name that is a device on
// Windows is refused on every system: a data directory moved to Windows
// still works.
func ValidName(name string) bool { return WellFormedName(name) && !DeviceName(name) }

// DeviceName reports whether seg, one segment of a path, names a device on
// Windows: its part before the first "." is CON, PRN, AUX, NUL, COM0 to
// COM9 or LPT0 to LPT9, in any case. Windows opens the device for such a
// name whatever extension follows ("nul.txt" is NUL), so a file or
// directory of that name cannot be made there.
func DeviceName(seg string) bool {
	base, _, _ := strings.Cut(seg, ".")
	if len(base) != 3 && len(base) != 4 {
		return false
	}
	base = strings.ToUpper(base)
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	return len(base) == 4 && (base[:3] == "COM" || base[:3] == "LPT") && base[3] >= '0' && base[3] <= '9'
}

// TrailingDotOrSpace reports whether seg ends in "." or " ". Windows drops
// a segment's final dots and spaces when it opens a path, so "mri." and
// "mri " are the directory "mri" there: two storages, or two roots, written
// one way each would share one directory.
func TrailingDotOrSpace(seg string) bool {
	return seg != "" && (seg[len(seg)-1] == '.' || seg[len(seg)-1] == ' ')
}

// Nest reports whether the paths whose segments are a and b are one
// directory or one lies inside the other: the shorter is a prefix of the
// longer, segment by segment, with case folded. Case is folded because a
// file system that ignores it (APFS and NTFS by default) makes two
// spellings one directory, and folding more than a given file system does
// only refuses more. A path of no segments contains every other.
func Nest(a, b []string) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}
