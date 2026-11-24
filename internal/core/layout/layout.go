package layout

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/uid"
)

// The roots of the two trees, with their trailing slash, as a storage lists
// them by prefix.
const (
	DataRoot = "data/"
	MetaRoot = "meta/"
)

// The names of the two files of a scan's meta directory, and the extension
// of an instance's file.
const (
	ManifestName = "manifest.json"
	MetadataName = "metadata.json"
	InstanceExt  = ".dcm"
)

// RevisionMark and MaxRevision form an instance's revision key: revision 1
// keeps the plain key, and revision n from 2 to MaxRevision is
// <UID>.r<n>.dcm. A key is written once and never again, so a replaced
// instance's new bytes go to a key of their own and a reader that opened
// the old key finishes on the old bytes.
const (
	RevisionMark = ".r"
	MaxRevision  = 1000000
)

// MaxKeyLength is the longest key this package builds: a data key with
// three labels of 128 bytes and a UID of 64 and a revision suffix is 469
// bytes. Parse refuses a longer string before looking into it.
const MaxKeyLength = len(DataRoot) + 3*(label.MaxLength+1) + uid.MaxLength + len(RevisionMark) + len("1000000") + len(InstanceExt)

// Kind says which of the three files of a scan a key names.
type Kind int

// The kinds.
const (
	// Instance is an instance's DICOM file under data/.
	Instance Kind = iota + 1
	// Manifest is a scan's manifest under meta/.
	Manifest
	// Metadata is a scan's sidecar of headers under meta/.
	Metadata
)

// String returns the kind's name.
func (k Kind) String() string {
	switch k {
	case Instance:
		return "instance"
	case Manifest:
		return "manifest"
	case Metadata:
		return "metadata"
	}
	return "unknown"
}

// Key is a key of the layout in parts.
type Key struct {
	// Kind is which file it names.
	Kind Kind
	// Path is the scan the file belongs to.
	Path label.Path
	// SOPInstanceUID is the instance's UID for Instance, and empty
	// otherwise.
	SOPInstanceUID string
	// Revision is the instance's revision for Instance: 0 and 1 name the
	// first, whose key has no suffix (Parse gives 0 for it), and 2 to
	// MaxRevision name a later one. Zero for the other kinds.
	Revision int
}

// Rev returns the revision the key names, at least 1.
func (k Key) Rev() int { return max(k.Revision, 1) }

// ErrKey is the error Parse returns, wrapped, for any string that is not a
// key of this layout.
var ErrKey = errors.New("layout: not a key of the layout")

// String builds the key, or returns an error if a part is not valid.
func (k Key) String() (string, error) {
	if err := k.Path.Check(); err != nil {
		return "", fmt.Errorf("%w: %w", ErrKey, err)
	}
	dir := k.Path.Subject + "/" + k.Path.Session + "/" + k.Path.Scan + "/"
	switch k.Kind {
	case Instance:
		if err := uid.Check(k.SOPInstanceUID); err != nil {
			return "", fmt.Errorf("%w: %w", ErrKey, err)
		}
		if k.Revision < 0 || k.Revision > MaxRevision {
			return "", fmt.Errorf("%w: a revision is 1 to %d", ErrKey, MaxRevision)
		}
		suffix := ""
		if k.Revision >= 2 {
			suffix = RevisionMark + strconv.Itoa(k.Revision)
		}
		return DataRoot + dir + k.SOPInstanceUID + suffix + InstanceExt, nil
	case Manifest, Metadata:
		if k.SOPInstanceUID != "" || k.Revision != 0 {
			return "", fmt.Errorf("%w: a %s key has no SOP Instance UID or revision", ErrKey, k.Kind)
		}
		name := ManifestName
		if k.Kind == Metadata {
			name = MetadataName
		}
		return MetaRoot + dir + name, nil
	}
	return "", fmt.Errorf("%w: unknown kind", ErrKey)
}

// InstanceKey returns the key of an instance's file in the scan at p.
func InstanceKey(p label.Path, sopInstanceUID string) (string, error) {
	return Key{Kind: Instance, Path: p, SOPInstanceUID: sopInstanceUID}.String()
}

// RevisionKey returns the key of revision n of an instance's file in the
// scan at p: the instance's key for revision 1, and <UID>.r<n>.dcm from 2.
func RevisionKey(p label.Path, sopInstanceUID string, n int) (string, error) {
	if n < 1 {
		return "", fmt.Errorf("%w: a revision is 1 to %d", ErrKey, MaxRevision)
	}
	if n == 1 {
		n = 0
	}
	return Key{Kind: Instance, Path: p, SOPInstanceUID: sopInstanceUID, Revision: n}.String()
}

// ManifestKey returns the key of the manifest of the scan at p.
func ManifestKey(p label.Path) (string, error) { return Key{Kind: Manifest, Path: p}.String() }

// MetadataKey returns the key of the sidecar of headers of the scan at p.
func MetadataKey(p label.Path) (string, error) { return Key{Kind: Metadata, Path: p}.String() }

// InstancePrefix returns the prefix, ending in "/", under which a storage
// lists the instance files of the scan at p.
func InstancePrefix(p label.Path) (string, error) {
	if err := p.Check(); err != nil {
		return "", fmt.Errorf("%w: %w", ErrKey, err)
	}
	return DataRoot + p.Subject + "/" + p.Session + "/" + p.Scan + "/", nil
}

// Parse returns the parts of key, or an error wrapping ErrKey if key is not
// exactly what Key.String builds for some valid parts.
func Parse(key string) (Key, error) {
	if len(key) > MaxKeyLength {
		return Key{}, fmt.Errorf("%w: longer than any key", ErrKey)
	}
	f := strings.Split(key, "/")
	if len(f) != 5 {
		return Key{}, fmt.Errorf("%w: not five segments", ErrKey)
	}
	k := Key{Path: label.Path{Subject: f[1], Session: f[2], Scan: f[3]}}
	switch root, name := f[0]+"/", f[4]; {
	case root == DataRoot && strings.HasSuffix(name, InstanceExt):
		k.Kind, k.SOPInstanceUID = Instance, strings.TrimSuffix(name, InstanceExt)
		// A UID holds only digits and dots, so the first ".r" is the
		// revision's mark; the round trip below refuses ".r1", leading
		// zeros and anything else String would not write.
		if i := strings.Index(k.SOPInstanceUID, RevisionMark); i >= 0 {
			digits := k.SOPInstanceUID[i+len(RevisionMark):]
			n, err := strconv.Atoi(digits)
			if err != nil || digits == "" || digits[0] < '1' || digits[0] > '9' || len(digits) > 7 {
				return Key{}, fmt.Errorf("%w: not a revision suffix", ErrKey)
			}
			k.SOPInstanceUID, k.Revision = k.SOPInstanceUID[:i], n
		}
	case root == MetaRoot && name == ManifestName:
		k.Kind = Manifest
	case root == MetaRoot && name == MetadataName:
		k.Kind = Metadata
	default:
		return Key{}, fmt.Errorf("%w: neither an instance under data/ nor a scan file under meta/", ErrKey)
	}
	// The round trip is the check: whatever Parse took apart must build
	// back to the same bytes, so Parse can never accept a form String
	// would not produce.
	s, err := k.String()
	if err != nil {
		return Key{}, err
	}
	if s != key {
		return Key{}, fmt.Errorf("%w: does not build back to itself", ErrKey)
	}
	return k, nil
}

// The keys of the records a storage holds apart from the scans' trees. A
// label never starts with a dot (package label), so nothing under
// meta/.corrections/ or at meta/.owner.json is ever a subject's tree.
const (
	// CorrectionsPrefix is the prefix under which a storage lists the
	// correction records.
	CorrectionsPrefix = MetaRoot + ".corrections/"
	// CorrectionExt ends a correction record's key.
	CorrectionExt = ".json"
	// OwnerKey is the key of the owner record.
	OwnerKey = MetaRoot + ".owner.json"
	// OperationIDLength is the length of an operation id, in bytes: a ULID.
	OperationIDLength = 26
)

// crockford is Crockford's base 32 alphabet in lowercase: the digits and the
// letters but i, l, o and u.
const crockford = "0123456789abcdefghjkmnpqrstvwxyz"

// ValidOperationID returns nil if id is an operation id: a ULID written in
// lowercase, OperationIDLength characters of Crockford's base 32, the first
// at most "7" since a ULID holds 128 bits. Its first ten characters are its
// time of creation, so ids sort in the order operations were made, and the
// one spelling per value keeps one record per operation on a storage that
// compares keys by their bytes. It is safe as a key's last segment on every
// storage and as a file name.
func ValidOperationID(id string) error {
	if len(id) != OperationIDLength || id[0] > '7' {
		return fmt.Errorf("%w: an operation id is a ULID in lowercase", ErrKey)
	}
	for i := 0; i < len(id); i++ {
		if !strings.Contains(crockford, id[i:i+1]) {
			return fmt.Errorf("%w: an operation id is a ULID in lowercase", ErrKey)
		}
	}
	return nil
}

// CorrectionKey returns the key of the correction record of an operation.
func CorrectionKey(operationID string) (string, error) {
	if err := ValidOperationID(operationID); err != nil {
		return "", err
	}
	return CorrectionsPrefix + operationID + CorrectionExt, nil
}

// ParseCorrectionKey returns the operation id a correction record's key
// names, or an error wrapping ErrKey for any other string.
func ParseCorrectionKey(key string) (string, error) {
	id, ok := strings.CutPrefix(key, CorrectionsPrefix)
	if ok {
		id, ok = strings.CutSuffix(id, CorrectionExt)
	}
	if !ok {
		return "", fmt.Errorf("%w: not a correction record's key", ErrKey)
	}
	if _, err := CorrectionKey(id); err != nil {
		return "", err
	}
	return id, nil
}
