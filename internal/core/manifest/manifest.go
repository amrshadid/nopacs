package manifest

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/uid"
)

// Version is the format version this package writes and the only one it
// reads.
const Version = 1

// MaxFiles is the most files one manifest may list. One scan holding more
// than 131072 instance files is far past any acquisition stored one file per
// instance; the bound keeps a manifest's size, and the memory a read of one
// takes, known.
const MaxFiles = 1 << 17

// maxLine is the longest line of one file in the encoded form: its fixed
// text, the longest key, three numbers of 19 digits, a SHA-256 in hex and
// the longest UID.
const maxLine = len(`{"key":"","offset":,"length":,"size":,"sha256":"","sop_instance_uid":""},`+"\n") +
	layout.MaxKeyLength + 3*19 + 64 + uid.MaxLength

// MaxBytes is the most Decode reads: MaxFiles of the longest line and the
// text around them, about 90 MiB.
const MaxBytes = MaxFiles*maxLine + len(header) + len(footer)

const (
	header = `{"version":1,"files":[` + "\n"
	footer = "]}\n"
)

// File is one file of a scan.
type File struct {
	// Key is the key of the object that holds the file, in the layout's
	// data/ tree.
	Key string `json:"key"`
	// Offset is where the file's bytes start in the object.
	Offset int64 `json:"offset"`
	// Length is how many bytes the file has.
	Length int64 `json:"length"`
	// Size is the size of the whole object.
	Size int64 `json:"size"`
	// SHA256 is the SHA-256 of the file's bytes, in lower case hex.
	SHA256 string `json:"sha256"`
	// SOPInstanceUID is the UID of the instance the file holds.
	SOPInstanceUID string `json:"sop_instance_uid"`
}

// Manifest is the files of one scan.
type Manifest struct {
	// Files is the scan's files. Encode sorts a copy; Decode returns them
	// sorted.
	Files []File
}

// The errors of this package, each wrapped with detail.
var (
	// ErrVersion is a manifest of a version this package does not read.
	ErrVersion = errors.New("manifest: unknown version")
	// ErrTooLarge is a manifest past MaxBytes or MaxFiles.
	ErrTooLarge = errors.New("manifest: too large")
	// ErrInvalid is a manifest whose content breaks a rule of the format.
	ErrInvalid = errors.New("manifest: invalid")
	// ErrNotCanonical is a manifest whose content is valid but whose bytes
	// are not the form Encode writes.
	ErrNotCanonical = errors.New("manifest: not in the one form a manifest is written in")
	// ErrConflict is a file given to Merge whose SOP Instance UID the
	// manifest already lists with a different entry.
	ErrConflict = errors.New("manifest: a different file under a UID already listed")
)

// Path returns the scan the manifest's files belong to, from its first
// file's key, or an error if it has no files or its first key is not one of
// the layout's.
func (m Manifest) Path() (label.Path, error) {
	if len(m.Files) == 0 {
		return label.Path{}, fmt.Errorf("%w: no files", ErrInvalid)
	}
	k, err := layout.Parse(m.Files[0].Key)
	if err != nil {
		return label.Path{}, fmt.Errorf("%w: file 0: %w", ErrInvalid, err)
	}
	return k.Path, nil
}

// checkFile returns nil if f obeys the format and belongs to the scan at p.
func checkFile(f File, p label.Path) error {
	k, err := layout.Parse(f.Key)
	switch {
	case err != nil:
		return err
	case k.Kind != layout.Instance:
		return errors.New("key is not an instance's file")
	case k.Path != p:
		return errors.New("key is in another scan")
	}
	if err := uid.Check(f.SOPInstanceUID); err != nil {
		return err
	}
	switch {
	case k.SOPInstanceUID != f.SOPInstanceUID:
		return errors.New("key names another instance than sop_instance_uid")
	case f.Offset != 0:
		// Version 1 keeps one instance per object.
		return errors.New("offset is not 0")
	case f.Length <= 0:
		return errors.New("length is not positive")
	case f.Size != f.Length:
		return errors.New("size is not length, and version 1 keeps one instance per object")
	case !isSHA256(f.SHA256):
		return errors.New("sha256 is not 64 lower case hexadecimal digits")
	}
	return nil
}

func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// compareFiles orders files by key, then offset: the order of the encoded
// form.
func compareFiles(a, b File) int {
	return cmp.Or(cmp.Compare(a.Key, b.Key), cmp.Compare(a.Offset, b.Offset))
}

// check returns nil if m is a valid manifest: 1 to MaxFiles files, each
// valid, all of one scan, no SOP Instance UID twice. m.Files must be sorted.
func check(m Manifest) error {
	if len(m.Files) > MaxFiles {
		return fmt.Errorf("%w: more than %d files", ErrTooLarge, MaxFiles)
	}
	p, err := m.Path()
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(m.Files))
	for i, f := range m.Files {
		if err := checkFile(f, p); err != nil {
			return fmt.Errorf("%w: file %d: %w", ErrInvalid, i, err)
		}
		if seen[f.SOPInstanceUID] {
			return fmt.Errorf("%w: file %d: SOP Instance UID listed twice", ErrInvalid, i)
		}
		seen[f.SOPInstanceUID] = true
	}
	return nil
}

// Encode returns m in the one form a manifest is written in, or an error if
// m is not a valid manifest. m is not changed.
func Encode(m Manifest) ([]byte, error) {
	files := slices.Clone(m.Files)
	slices.SortFunc(files, compareFiles)
	if err := check(Manifest{Files: files}); err != nil {
		return nil, err
	}
	return encode(files), nil
}

// encode writes files, already valid and sorted. Every string has been
// checked to hold only letters, digits and ".", "_", "-", "/", so none needs
// escaping in JSON.
func encode(files []File) []byte {
	b := make([]byte, 0, len(header)+len(footer)+len(files)*300)
	b = append(b, header...)
	for i, f := range files {
		b = append(b, `{"key":"`...)
		b = append(b, f.Key...)
		b = append(b, `","offset":`...)
		b = strconv.AppendInt(b, f.Offset, 10)
		b = append(b, `,"length":`...)
		b = strconv.AppendInt(b, f.Length, 10)
		b = append(b, `,"size":`...)
		b = strconv.AppendInt(b, f.Size, 10)
		b = append(b, `,"sha256":"`...)
		b = append(b, f.SHA256...)
		b = append(b, `","sop_instance_uid":"`...)
		b = append(b, f.SOPInstanceUID...)
		b = append(b, `"}`...)
		if i < len(files)-1 {
			b = append(b, ',')
		}
		b = append(b, '\n')
	}
	return append(b, footer...)
}

// Decode reads one manifest from r, reading at most MaxBytes+1 bytes, and
// returns it with its files sorted, or an error wrapping one of this
// package's errors.
func Decode(r io.Reader) (Manifest, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(MaxBytes)+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("manifest: read: %w", err)
	}
	if len(data) > MaxBytes {
		return Manifest{}, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, MaxBytes)
	}
	// The version first, read leniently, so a manifest of a later version
	// is reported as that and not as a field this version does not know.
	var v struct {
		Version json.RawMessage `json:"version"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if string(v.Version) != strconv.Itoa(Version) {
		return Manifest{}, fmt.Errorf("%w: %.20q", ErrVersion, v.Version)
	}
	var w struct {
		Version int    `json:"version"`
		Files   []File `json:"files"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	m := Manifest{Files: w.Files}
	if !slices.IsSortedFunc(m.Files, compareFiles) {
		return Manifest{}, fmt.Errorf("%w: files are not sorted by key", ErrNotCanonical)
	}
	if err := check(m); err != nil {
		return Manifest{}, err
	}
	if !bytes.Equal(encode(m.Files), data) {
		return Manifest{}, ErrNotCanonical
	}
	return m, nil
}

// Merge returns m with the files of a later arrival to the same scan added,
// and the files that were added, both sorted. A file already listed with
// the same entry is not added again, so a step that is run again after a
// crash merges to the same manifest. A file whose SOP Instance UID is
// listed with a different entry is refused with ErrConflict: the reception
// decides what a different file under a known UID means before it reaches
// here. m may have no files, for a new scan. Neither m nor late is changed.
func Merge(m Manifest, late []File) (Manifest, []File, error) {
	byUID := make(map[string]File, len(m.Files)+len(late))
	for _, f := range m.Files {
		byUID[f.SOPInstanceUID] = f
	}
	files := slices.Clone(m.Files)
	var added []File
	for i, f := range late {
		old, ok := byUID[f.SOPInstanceUID]
		switch {
		case ok && old == f:
			continue
		case ok:
			return Manifest{}, nil, fmt.Errorf("%w: late file %d", ErrConflict, i)
		}
		byUID[f.SOPInstanceUID] = f
		files = append(files, f)
		added = append(added, f)
	}
	slices.SortFunc(files, compareFiles)
	slices.SortFunc(added, compareFiles)
	out := Manifest{Files: files}
	if err := check(out); err != nil {
		return Manifest{}, nil, err
	}
	return out, added, nil
}
