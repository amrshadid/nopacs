package sidecar

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/uid"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Version is the format version this package writes for a scan it knows the
// catalog's values of: it adds a header of the scan and, to each line, what
// a catalog made again from the storage needs. It reads it, VersionFrames
// and version 1, the form before frame tables, whose lines need not have one
// (see the package documentation).
const Version = 3

// VersionFrames is the form that has frame tables and neither the scan's
// header nor the catalog's values in its lines. A merge into a sidecar of
// this version, or a sidecar written without a header, stays in it until the
// rewrite (Upgrade) that gives it the rest.
const VersionFrames = 2

// MaxDataset is the longest data set text an instance's line holds; a longer
// one is omitted (see the package documentation).
const MaxDataset = 1 << 20

// MaxFrameRanges is the most byte ranges an instance's frame table holds,
// fragments of all its frames together; an instance with more has its
// table left empty (see the package documentation).
const MaxFrameRanges = 1 << 16

// maxRangeText is the longest one range's text may be in a frame table:
// two numbers of at most 19 digits, the brackets and commas around them,
// and its frame's brackets when it is a frame's only range.
const maxRangeText = 2*19 + 6

// maxSumText is the longest one frame's checksum may be in a line: 64 hex
// digits, its quotes and a comma.
const maxSumText = 64 + 3

// maxLine is the longest line the reader takes: the longest data set, the
// longest frame table with a checksum for each of its frames (a frame has
// at least one range), and the fixed fields around them.
const maxLine = MaxDataset + MaxFrameRanges*(maxRangeText+maxSumText) + 4096 + layout.MaxKeyLength + 2*uid.MaxLength + MaxUnnamed*maxUnnamedText

const (
	headerV1 = `{"version":1,"instances":[` + "\n"
	header   = `{"version":2,"instances":[` + "\n"
	footer   = "]}\n"
	// headV3 and tailV3 enclose the scan's header in version 3's first
	// line: headV3 + the header's JSON + tailV3.
	headV3 = `{"version":3,"scan":`
	tailV3 = `,"instances":[`
)

// HeadLength is how many first bytes of a sidecar say its version
// (VersionOf).
const HeadLength = len(header)

// VersionOf returns the version a sidecar's first HeadLength bytes state,
// or 0 when they are not the first line of a sidecar of a version this
// package reads. A reader that needs only to know whether a scan's frame
// tables are all there reads these bytes, not the sidecar.
func VersionOf(head []byte) int {
	switch {
	case string(head) == header:
		return VersionFrames
	case string(head) == headerV1:
		return 1
	case len(head) == HeadLength && strings.HasPrefix(string(head), headV3+"{"):
		return Version
	}
	return 0
}

// IndexEvery is how many instances lie between two entries of the index a
// sidecar of more than IndexEvery instances ends with: the byte offset of
// every IndexEvery-th instance's line, so a reader of one page of a large
// scan starts near it instead of at the first line.
const IndexEvery = 256

// maxIndexLine is the longest the index line may be: 80 million instances'
// worth of entries.
const maxIndexLine = 16 << 20

// IndexEntry is the place of one instance's line in the sidecar.
type IndexEntry struct {
	// UID is the instance's SOP Instance UID.
	UID string `json:"uid"`
	// Offset is the first byte of its line.
	Offset int64 `json:"offset"`
}

// indexLine is the line that follows the footer.
type indexLine struct {
	Index []IndexEntry `json:"index"`
}

// PixelData is where an instance's pixel data element is in its file.
type PixelData struct {
	// Offset is the first byte of the element's tag.
	Offset int64 `json:"offset"`
	// Length runs to the first byte after the element.
	Length int64 `json:"length"`
}

// Range is a run of bytes of an instance's file: Length bytes from Offset.
// A frame table writes it as the pair [offset,length].
type Range struct {
	// Offset is its first byte.
	Offset int64
	// Length is how many bytes it has.
	Length int64
}

// MarshalJSON writes the range as [offset,length].
func (r Range) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, 40)
	b = append(b, '[')
	b = strconv.AppendInt(b, r.Offset, 10)
	b = append(b, ',')
	b = strconv.AppendInt(b, r.Length, 10)
	return append(b, ']'), nil
}

// UnmarshalJSON reads a range written as [offset,length], and nothing
// else.
func (r *Range) UnmarshalJSON(b []byte) error {
	var pair []int64
	if err := json.Unmarshal(b, &pair); err != nil || len(pair) != 2 {
		return fmt.Errorf("%w: a range is [offset,length]", ErrFormat)
	}
	r.Offset, r.Length = pair[0], pair[1]
	return nil
}

// Instance is one instance's line.
type Instance struct {
	// SOPInstanceUID names the instance.
	SOPInstanceUID string `json:"sop_instance_uid"`
	// Key is the instance's key on the storage: its whole key, ending in
	// ".r<n>.dcm" for revision n from 2 (layout.RevisionKey).
	Key string `json:"key"`
	// Revision is the revision the key holds, 2 or more; left out (0) for
	// revision 1, so a line names a revision one way only. Version 3 only.
	Revision int `json:"revision,omitempty"`
	// Size is the instance file's size in bytes.
	Size int64 `json:"size"`
	// SHA256 is the checksum of the file as archived, in lowercase hex.
	SHA256 string `json:"sha256"`
	// ReceivedSHA256 is the checksum of the file as received, written only
	// when it differs from SHA256 (a de-identified instance, whose received
	// checksum is the catalog's keyed one). Version 3 only.
	ReceivedSHA256 string `json:"received_sha256,omitempty"`
	// TransferSyntax is the Transfer Syntax UID of the instance's file,
	// empty in a line written before the field existed.
	TransferSyntax string `json:"transfer_syntax,omitempty"`
	// Profile is the profile version that de-identified the instance,
	// "name@version", as its catalog row records it; empty for one
	// archived as received, and in a line written before the field
	// existed.
	Profile string `json:"profile,omitempty"`
	// ProfileSHA256 is the checksum of that profile version's expanded
	// text, which names the version whatever database holds it. Version 3
	// only, and only with Profile; empty when the writer did not know it.
	ProfileSHA256 string `json:"profile_sha256,omitempty"`
	// BaseProfile is the tenant's base profile version, "name@version", the
	// profile above was stacked over when the instance was de-identified; left
	// out when the tenant had none. Version 3 only, and only with Profile. The
	// instance then counts as having full profile provenance: Profile names the
	// version of the receiver or route, as before.
	BaseProfile string `json:"base_profile,omitempty"`
	// PixelRiskAccepted says the instance's image was at risk of burned-in
	// text and a pixel rule of Profile accepted it as it is, as its catalog
	// row records it, so a catalog made again from the metadata refuses to
	// send it as the one before did. Only with Profile; left out (false)
	// otherwise. Kept in a sidecar of any version, never dropped as a field
	// of a later version is when a line joins an older sidecar: dropping it
	// would let a catalog made again send what the archive refused.
	PixelRiskAccepted bool `json:"pixel_risk_accepted,omitempty"`
	// Reception is the id of the reception that confirmed this revision of
	// the instance. Version 3 only, and always there in it.
	Reception string `json:"reception,omitempty"`
	// HeldAt is when the instance was first held, in UTC: when its first
	// revision's reception confirmed it, which a replace keeps, as the
	// catalog does. Version 3 only, and always there in it.
	HeldAt time.Time `json:"held_at,omitzero"`
	// ReceivedForm is how the file arrived: FormDIMSE or FormPart10; empty
	// when unknown. Version 3 only.
	ReceivedForm string `json:"received_form,omitempty"`
	// ReceivedSyntax is the Transfer Syntax UID the file arrived in, which
	// differs from TransferSyntax when de-identification changed it; empty
	// when unknown. Version 3 only.
	ReceivedSyntax string `json:"received_syntax,omitempty"`
	// NopacsVersion is the version of the build that wrote the line; empty
	// when unknown. Version 3 only.
	NopacsVersion string `json:"nopacs_version,omitempty"`
	// Unnamed lists the elements of the file as received that no rule of its
	// profile named, with their VRs and counts and never a value (see
	// UnnamedElement); empty but present for a de-identified file none were
	// found in, and left out (nil) for one archived as received and in a line
	// written before the field existed. Version 3 only.
	Unnamed []UnnamedElement `json:"unnamed,omitzero"`
	// UnnamedOver counts the occurrences of distinct elements past MaxUnnamed
	// that Unnamed does not list; left out (0) when none. Version 3 only,
	// and only with Unnamed.
	UnnamedOver int `json:"unnamed_over,omitempty"`
	// PixelData is where its pixel data is, nil when it has none or when the
	// file's data set is deflated, where no offset in the file names it.
	PixelData *PixelData `json:"pixel_data"`
	// Frames are where each frame of its pixel data lies in its file,
	// frame 1 first, each frame the ranges that joined in order are its
	// bytes (Frame). Nil in a line written before frame tables existed;
	// empty when it has pixel data whose frames are not byte ranges of the
	// file, or more than MaxFrameRanges ranges.
	Frames [][]Range `json:"frames,omitzero"`
	// FrameSHA256 is the SHA-256 of each frame of Frames in lowercase hex,
	// frame 1 first: of the frame's ranges' bytes joined in order, as the
	// file was archived. Every line with a frame table that is not empty
	// has one for each of its frames, and no other line has any.
	FrameSHA256 []string `json:"frame_sha256,omitempty"`
	// Dataset is its data set's DICOM JSON, nil when omitted.
	Dataset json.RawMessage `json:"dataset"`
	// DatasetOmitted says the data set was too long to keep.
	DatasetOmitted bool `json:"dataset_omitted,omitempty"`
}

// The errors of this package.
var (
	// ErrFormat is a sidecar that is not in the one form this package
	// writes, or an instance that breaks a rule of it.
	ErrFormat = errors.New("sidecar: not a valid metadata sidecar")
	// ErrOrder is an instance added out of the order of SOP Instance UIDs,
	// or twice.
	ErrOrder = errors.New("sidecar: instances out of order or repeated")
	// ErrIncomplete is a line with pixel data and no frame table where the
	// form requires one: in a sidecar of this version.
	ErrIncomplete = errors.New("sidecar: an instance with pixel data has no frame table")
	// ErrNoFrames is a frame asked of a line that holds no frame table.
	ErrNoFrames = errors.New("sidecar: the instance's line holds no frame table")
	// ErrNoSuchFrame is a frame number that is not one of the instance's.
	ErrNoSuchFrame = errors.New("sidecar: no such frame")
)

// Provenance returns how the line says its instance was archived: the
// profile version that de-identified it (zero for one archived as received,
// and for a profile that is not name@version, which a line read or written
// here never holds) and its accepted pixel risk.
func (in Instance) Provenance() tenant.InstanceProvenance {
	return tenant.InstanceProvenance{Profile: profileRef(in.Profile), PixelRiskAccepted: in.PixelRiskAccepted}
}

// NeedsFrames reports whether the line has pixel data and no frame table,
// as a line written before frame tables existed has.
func (in Instance) NeedsFrames() bool { return in.PixelData != nil && in.Frames == nil }

// Frame returns where frame n of the instance lies in its file, counted
// from 1 as a request names frames (PS3.18 10.4.1.1.4): the ranges whose
// bytes, joined in order, are the frame as stored. A line without a frame
// table, or with an empty one, is ErrNoFrames; n outside 1 to the number
// of frames is ErrNoSuchFrame.
func (in Instance) Frame(n int) ([]Range, error) {
	if len(in.Frames) == 0 {
		return nil, ErrNoFrames
	}
	if n < 1 || n > len(in.Frames) {
		return nil, ErrNoSuchFrame
	}
	return in.Frames[n-1], nil
}

// frameRanges counts the ranges of a frame table.
func frameRanges(frames [][]Range) int {
	n := 0
	for _, f := range frames {
		n += len(f)
	}
	return n
}

// checkFrames returns nil if the frame table may be written: each frame
// has a range and a checksum, every range lies within the pixel data
// element, and each starts at or after the end of the one before.
func checkFrames(in Instance) error {
	if len(in.Frames) == 0 {
		if len(in.FrameSHA256) > 0 {
			return fmt.Errorf("%w: frame checksums without a frame table", ErrFormat)
		}
		if in.Frames == nil {
			return nil
		}
	}
	p := in.PixelData
	if p == nil {
		return fmt.Errorf("%w: frames without pixel data", ErrFormat)
	}
	if frameRanges(in.Frames) > MaxFrameRanges {
		return fmt.Errorf("%w: more than %d frame ranges", ErrFormat, MaxFrameRanges)
	}
	if len(in.FrameSHA256) != len(in.Frames) {
		return fmt.Errorf("%w: a frame table without a checksum for each frame", ErrFormat)
	}
	for _, sum := range in.FrameSHA256 {
		if !lowerHex(sum, 64) {
			return fmt.Errorf("%w: a frame's checksum is not 64 lowercase hex digits", ErrFormat)
		}
	}
	end := p.Offset + p.Length
	at := p.Offset
	for _, f := range in.Frames {
		if len(f) == 0 {
			return fmt.Errorf("%w: a frame with no range", ErrFormat)
		}
		for _, r := range f {
			if r.Offset < at || r.Length < 0 || r.Length > end-r.Offset {
				return fmt.Errorf("%w: a frame range lies outside the pixel data or before the one ahead of it", ErrFormat)
			}
			at = r.Offset + r.Length
		}
	}
	return nil
}

// lowerHex reports whether s is n lowercase hex digits.
func lowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validProfile reports whether s is empty or a profile version as
// tenant.ProfileRef writes it, "name@version": its name and version held to
// ProfileRef's rule, and the version written without leading zeros, so a
// line names a version one way only.
func validProfile(s string) bool {
	if s == "" {
		return true
	}
	name, version, ok := strings.Cut(s, "@")
	if !ok {
		return false
	}
	n, err := strconv.ParseInt(version, 10, 32)
	if err != nil {
		return false
	}
	ref := tenant.ProfileRef{Name: name, Version: int32(n)}
	return ref.Validate() == nil && ref.String() == s
}

// hasV3 reports whether the line holds a field only version 3 has.
func (in Instance) hasV3() bool {
	return in.Revision != 0 || in.ReceivedSHA256 != "" || in.ProfileSHA256 != "" || in.BaseProfile != "" || in.Reception != "" || !in.HeldAt.IsZero() ||
		in.ReceivedForm != "" || in.ReceivedSyntax != "" || in.NopacsVersion != "" || in.Unnamed != nil || in.UnnamedOver != 0
}

// withoutV3 returns the line without the fields only version 3 has, for a
// sidecar of an older version, which has no place for them. The revision is
// not touched: a line that holds one cannot be written to an older form.
func (in Instance) withoutV3() Instance {
	in.ReceivedSHA256, in.ProfileSHA256, in.BaseProfile, in.Reception, in.HeldAt = "", "", "", "", time.Time{}
	in.ReceivedForm, in.ReceivedSyntax, in.NopacsVersion = "", "", ""
	in.Unnamed, in.UnnamedOver = nil, 0
	return in
}

// checkV3 checks the fields only version 3 has, where they are present:
// version is the sidecar's, 0 for a reader that does not know it.
func checkV3(in Instance, version int) error {
	if version > 0 && version < Version {
		if in.hasV3() {
			return fmt.Errorf("%w: a field of a later version", ErrFormat)
		}
		return nil
	}
	if version == Version {
		if in.Reception == "" || in.HeldAt.IsZero() {
			return fmt.Errorf("%w: a line of version %d has its reception and held_at", ErrFormat, Version)
		}
	}
	if in.Reception != "" && !validReception(in.Reception) {
		return fmt.Errorf("%w: reception is not an id", ErrFormat)
	}
	if !in.HeldAt.IsZero() && !utcTime(in.HeldAt) {
		return fmt.Errorf("%w: held_at is a time in UTC", ErrFormat)
	}
	if in.ReceivedSHA256 != "" && (!lowerHex(in.ReceivedSHA256, 64) || in.ReceivedSHA256 == in.SHA256) {
		return fmt.Errorf("%w: received_sha256 is 64 lowercase hex digits and differs from sha256", ErrFormat)
	}
	if in.ProfileSHA256 != "" && (in.Profile == "" || !lowerHex(in.ProfileSHA256, 64)) {
		return fmt.Errorf("%w: profile_sha256 is 64 lowercase hex digits and goes with a profile", ErrFormat)
	}
	if in.ReceivedForm != "" && in.ReceivedForm != FormDIMSE && in.ReceivedForm != FormPart10 {
		return fmt.Errorf("%w: received_form is %s or %s", ErrFormat, FormDIMSE, FormPart10)
	}
	if in.ReceivedSyntax != "" {
		if err := uid.Check(in.ReceivedSyntax); err != nil {
			return fmt.Errorf("%w: received_syntax: %w", ErrFormat, err)
		}
	}
	if in.NopacsVersion != "" && !validVersionText(in.NopacsVersion) {
		return fmt.Errorf("%w: nopacs_version", ErrFormat)
	}
	if in.BaseProfile != "" && (in.Profile == "" || !validProfile(in.BaseProfile)) {
		return fmt.Errorf("%w: base_profile is name@version and goes with a profile", ErrFormat)
	}
	if in.Unnamed != nil && in.Profile == "" {
		return fmt.Errorf("%w: unnamed goes with a profile", ErrFormat)
	}
	return checkUnnamed(in)
}

// check returns nil if in may be written to a sidecar of version, or read
// from one (0 for a reader that began at a line and does not know it).
func check(in Instance, version int) error {
	if err := uid.Check(in.SOPInstanceUID); err != nil {
		return fmt.Errorf("%w: sop_instance_uid: %w", ErrFormat, err)
	}
	k, err := layout.Parse(in.Key)
	if err != nil || k.Kind != layout.Instance || k.SOPInstanceUID != in.SOPInstanceUID {
		return fmt.Errorf("%w: key is not the instance's key of the layout", ErrFormat)
	}
	if in.Revision < 0 || in.Revision == 1 || k.Rev() != max(in.Revision, 1) {
		return fmt.Errorf("%w: revision is left out for 1 and is the key's otherwise", ErrFormat)
	}
	if in.Size < 0 || len(in.SHA256) != 64 {
		return fmt.Errorf("%w: size or sha256", ErrFormat)
	}
	if err := checkV3(in, version); err != nil {
		return err
	}
	if in.TransferSyntax != "" {
		if err := uid.Check(in.TransferSyntax); err != nil {
			return fmt.Errorf("%w: transfer_syntax: %w", ErrFormat, err)
		}
	}
	if !validProfile(in.Profile) {
		return fmt.Errorf("%w: profile is not name@version", ErrFormat)
	}
	if in.PixelRiskAccepted && in.Profile == "" {
		return fmt.Errorf("%w: pixel_risk_accepted goes with a profile", ErrFormat)
	}
	if p := in.PixelData; p != nil && (p.Offset < 0 || p.Length < 0 || p.Offset+p.Length > in.Size) {
		return fmt.Errorf("%w: pixel_data lies outside the file", ErrFormat)
	}
	if in.DatasetOmitted != (in.Dataset == nil) {
		return fmt.Errorf("%w: a data set is either there or said to be omitted", ErrFormat)
	}
	return checkFrames(in)
}
