package tenant

import (
	"bytes"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amrshadid/nopacs/internal/core/importpath"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// What an import unit writes as it walks (ImportWork): the arrival of a
// file it hands in, the skip of an entry it does not, the folders the
// listing unit queues, and the keys a store orders paths by.

// ErrImportEnded is in the chain of a file handed in to an import job that
// has ended (ImportEnded): the worker ends its unit's walk and lets the
// copy it landed go, since the job counts nothing more it hands in.
var ErrImportEnded = errors.New("the import job has ended")

// ImportEnded returns the refusal of a file handed in to an import job
// that has ended (canceled): a precondition, with ErrImportEnded in the
// chain. Nothing was recorded, the file's batch included.
func ImportEnded() error {
	return fault.Wrap(fault.Precondition, ErrImportEnded,
		"the import job has ended; it takes no more files, and the unit ends its walk")
}

// ImportArrival is the source of a file an import unit hands in: carried
// by its received file (ReceivedFile.Import) or by the record of a file
// held as it arrived (QuarantinedFile.Import). The store writes the
// arrival, the unit's cursor and its counts in the transaction of the
// file's record, only while the fence holds; the job and unit are the
// fence's.
type ImportArrival struct {
	// Fence is the import unit's lease.
	Fence WorkFence
	// Path is the file's path relative to the root, under the unit's
	// folder, after its cursor.
	Path string
	// Size is its size as read from the root.
	Size int64
	// Parts are the parts its path gave the naming rule (the layout's,
	// and the one its map filled), by name: kept with the arrival until
	// its reception is named (Importing.ArrivalParts). They can identify a
	// person, as the path can, and are kept as the path is.
	Parts map[string]string
}

// The bounds of an arrival's parts: a layout's parts and the map's one,
// each value a path's segments or a map's cell.
const (
	MaxImportParts     = 32
	MaxImportPartValue = importpath.MaxPathLength
)

// Validate returns an Invalid error unless a is well formed.
func (a ImportArrival) Validate() error {
	if err := a.Fence.Validate(); err != nil {
		return err
	}
	if err := validateImportPath("file's path", a.Path, false); err != nil {
		return err
	}
	if a.Size < 0 {
		return invalid("a file's size is not negative")
	}
	if len(a.Parts) > MaxImportParts {
		return invalid("a file's path gives at most " + strconv.Itoa(MaxImportParts) + " parts")
	}
	for name, v := range a.Parts {
		if !validPartName(name) {
			return invalid("a part's name is a lowercase ASCII letter followed by lowercase letters, digits and _, at most 64 bytes")
		}
		if len(v) > MaxImportPartValue || !utf8.ValidString(v) {
			return invalid("a part's value is UTF-8 of at most " + strconv.Itoa(MaxImportPartValue) + " bytes")
		}
	}
	return nil
}

// ImportSkip is a file, or a directory, an import unit judged and did not
// hand in (ImportWork.RecordImportSkip).
type ImportSkip struct {
	// Fence is the unit's lease.
	Fence WorkFence
	// Path is the entry's path relative to the root, under the unit's
	// folder, after its cursor, or the unit's folder itself before its
	// first entry (a folder that is no longer a directory reached through
	// no link): longer than importpath.MaxPathLength, up to
	// importpath.MaxEntryPathLength (importpath.CheckEntry), exactly when
	// Code is SkipPathTooLong; a name in a folder that follows the rule,
	// the name breaking it, exactly when Code is SkipBadName. The unit's
	// cursor moves to it; the row is kept under RowPath.
	Path string
	// Code is why it was skipped.
	Code ImportSkipCode
	// Size is its size as the walk saw it; 0 when not read.
	Size int64
	// Detail is one of the server's phrases (ImportDetailMaxDirEntries,
	// ImportDetailMaxFileBytes) or empty: never the operating system's
	// text.
	Detail string
	// Now is when it was judged.
	Now time.Time
}

// Validate returns an Invalid error unless s can be recorded.
func (s ImportSkip) Validate() error {
	if err := s.Fence.Validate(); err != nil {
		return err
	}
	if !s.Code.Valid() {
		return invalid("a skip's code is one of not_part10, outside_layout, unmapped_session, directory_too_wide, unreadable, not_regular, too_large, path_too_long, bad_name")
	}
	if s.Code == SkipBadName {
		if err := validateBadName(s.Path); err != nil {
			return err
		}
	} else {
		fits, err := importpath.CheckEntry(s.Path)
		if err != nil {
			return invalid("an import skipped path breaks the rule of a path under a root: " +
				strings.TrimPrefix(err.Error(), "importpath: "))
		}
		if fits == (s.Code == SkipPathTooLong) {
			return invalid("a skipped path is longer than " + strconv.Itoa(importpath.MaxPathLength) +
				" bytes exactly when its code is path_too_long")
		}
	}
	if s.Size < 0 {
		return invalid("a file's size is not negative")
	}
	if !slices.Contains(importDetails, s.Detail) {
		return invalid("a skip's detail is one of the server's phrases")
	}
	return master.ValidateTime("skip time", s.Now)
}

// MaxBadNameLength bounds the name of an entry skipped as SkipBadName: 255
// characters of four bytes each, the longest name a file system that counts
// characters rather than bytes (APFS, NTFS) holds.
const MaxBadNameLength = 4 * importpath.MaxSegmentLength

// RowPath returns the path the row of s is kept under: for SkipBadName the
// folder holding the entry, since the name follows no rule a row can keep
// ("" for the root); otherwise the deepest of Path and its ancestors that
// fits a store's index (importpath.Fitting).
func (s ImportSkip) RowPath() string {
	if s.Code == SkipBadName {
		dir, _ := splitImportName(s.Path)
		return dir
	}
	return importpath.Fitting(s.Path)
}

// validateBadName returns an Invalid error unless p is a name that breaks
// the rule of a segment, of 1 to MaxBadNameLength bytes, holding no NUL
// (which a path's key takes as its separator, ImportPathKey) and no "/", in
// a folder that follows the rule (or the root).
func validateBadName(p string) error {
	dir, name := splitImportName(p)
	if dir != "" && importpath.Check(dir) != nil {
		return invalid("a name skipped as bad_name is in a folder that follows the rule of a path under a root")
	}
	if name == "" || name == "." || name == ".." || len(name) > MaxBadNameLength || strings.IndexByte(name, 0) >= 0 {
		return invalid("a name skipped as bad_name is 1 to " + strconv.Itoa(MaxBadNameLength) + " bytes, never NUL, \".\" or \"..\"")
	}
	if _, err := importpath.CheckEntry(p); err == nil {
		return invalid("a name skipped as bad_name breaks the rule of a path segment")
	}
	return nil
}

// splitImportName returns the folder of p ("" for the root) and its last
// segment.
func splitImportName(p string) (string, string) {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}

// ValidateImportFolders returns an Invalid error unless folders is 1 to
// MaxImportUnitBatch paths, each by importpath.Check, in strictly rising
// importpath.Compare order.
func ValidateImportFolders(folders []string) error {
	if len(folders) == 0 || len(folders) > MaxImportUnitBatch {
		return invalid("a listing queues 1 to " + strconv.Itoa(MaxImportUnitBatch) + " folders at once")
	}
	for i, f := range folders {
		if err := validateImportPath("folder", f, false); err != nil {
			return err
		}
		if i > 0 && importpath.Compare(folders[i-1], f) >= 0 {
			return invalid("a listing's folders are in the walk's order, each once")
		}
	}
	return nil
}

// ImportPathUnder reports whether p lies under folder, segment by segment
// ("a" holds "a/b", not "ab"); every path lies under the empty folder, the
// whole root.
func ImportPathUnder(folder, p string) bool {
	return folder == "" || strings.HasPrefix(p, folder+"/")
}

// ImportPathKey returns the key a store orders paths by: p with each "/"
// as the byte 0. No segment holds NUL (importpath.Check), so plain byte
// order of keys, which an index of a byte column keeps (SQLite's BLOB and
// Postgres's bytea compare by memcmp, then length), is importpath.Compare's
// order: "/" taken as lower than every other byte. The key is the path
// itself, reversibly (ImportPathOfKey), so no second column holds it.
func ImportPathKey(p string) []byte {
	b := []byte(p)
	for i, c := range b {
		if c == '/' {
			b[i] = 0
		}
	}
	return b
}

// ImportPathOfKey returns the path a key was made from (ImportPathKey).
func ImportPathOfKey(k []byte) string {
	return string(bytes.ReplaceAll(k, []byte{0}, []byte{'/'}))
}
