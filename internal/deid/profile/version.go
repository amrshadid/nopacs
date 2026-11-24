package profile

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/pathname"
)

// MaxVersions is the most versions a profile may have.
const MaxVersions = 10000

// ErrChecksum is the cause of a stored version whose expanded text does not
// match its checksum: it was changed after it was made.
var ErrChecksum = errors.New("profile: the expanded form does not match its checksum")

// Version is one version of a profile: its name and number, the document as
// given and its expanded form, each kept as its canonical text, and the
// checksum of the expanded text. A Version cannot be changed: it holds its
// texts, and every accessor answers a copy, so what a version does is fixed
// from the moment it is made. A change is a new Version with the next
// number, and the earlier one stays as it was.
type Version struct {
	name     string
	number   int32
	source   []byte
	expanded []byte
	checksum string
}

// ValidName reports whether name is a profile's name: the rule of a
// tenant's name, 1 to 63 of a-z, 0-9 and -, starting with a letter.
func ValidName(name string) bool { return pathname.ValidName(name) }

// New checks and expands doc against cat and returns it as version number
// of the profile name. A document with problems is refused with Problems.
func New(name string, number int32, doc *v1.ProfileDocument, cat *Catalog) (*Version, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("profile: %q is not a profile's name: 1 to 63 of a-z, 0-9 and -, starting with a letter", clipText(name))
	}
	if number < 1 || number > MaxVersions {
		return nil, fmt.Errorf("profile: a version is 1 to %d", MaxVersions)
	}
	e, problems := Expand(doc, cat)
	if len(problems) > 0 {
		return nil, problems
	}
	source, err := Canonical(doc)
	if err != nil {
		return nil, err
	}
	text, sum, err := Checksum(e)
	if err != nil {
		return nil, err
	}
	return &Version{name: name, number: number, source: source, expanded: text, checksum: sum}, nil
}

// Next returns doc as the version after v. Earlier versions are not touched.
func (v *Version) Next(doc *v1.ProfileDocument, cat *Catalog) (*Version, error) {
	if v.number >= MaxVersions {
		return nil, fmt.Errorf("profile: %s already has %d versions, the most a profile may have", v.name, MaxVersions)
	}
	return New(v.name, v.number+1, doc, cat)
}

// Load returns a stored version from its texts, after checking that the
// expanded text has the checksum stored with it and that both texts read.
// The bases are not expanded again: a stored version does what its
// expanded text says, whatever this build ships.
func Load(name string, number int32, source, expanded []byte, checksum string) (*Version, error) {
	if !ValidName(name) || number < 1 || number > MaxVersions {
		return nil, fmt.Errorf("profile: a stored version has a malformed name or number")
	}
	h := sha256.Sum256(expanded)
	got := hex.EncodeToString(h[:])
	if subtle.ConstantTimeCompare([]byte(got), []byte(checksum)) != 1 {
		return nil, fmt.Errorf("%w: %s@%d", ErrChecksum, name, number)
	}
	v := &Version{name: name, number: number, source: bytes.Clone(source), expanded: bytes.Clone(expanded), checksum: checksum}
	if _, err := v.decode(); err != nil {
		return nil, err
	}
	if _, err := ParseDocument(source); err != nil {
		return nil, err
	}
	return v, nil
}

// Name returns the profile's name.
func (v *Version) Name() string { return v.name }

// Number returns the version's number.
func (v *Version) Number() int32 { return v.number }

// Ref returns the version as a reference, "name@number".
func (v *Version) Ref() string { return Ref{Name: v.name, Version: v.number}.String() }

// Checksum returns the SHA-256 of the expanded text, in lowercase
// hexadecimal.
func (v *Version) Checksum() string { return v.checksum }

// Source returns a copy of the document's canonical text, as stored.
func (v *Version) Source() []byte { return bytes.Clone(v.source) }

// ExpandedText returns a copy of the expanded form's canonical text, as
// stored and as the checksum covers it.
func (v *Version) ExpandedText() []byte { return bytes.Clone(v.expanded) }

// Document returns a new copy of the document as given.
func (v *Version) Document() *v1.ProfileDocument {
	doc, err := ParseDocument(v.source)
	if err != nil {
		// The source was read when the version was made or loaded.
		panic(err)
	}
	return doc
}

// Expanded returns a new copy of the expanded form.
func (v *Version) Expanded() *v1.ExpandedProfile {
	e, err := v.decode()
	if err != nil {
		// The expanded text was read when the version was made or loaded.
		panic(err)
	}
	return e
}

// SameDocument reports whether doc is, field by field, the document of
// this version: a request to store it again makes no new version.
func (v *Version) SameDocument(doc *v1.ProfileDocument) bool {
	c, err := Canonical(doc)
	return err == nil && bytes.Equal(c, v.source)
}

// decode reads the expanded text.
func (v *Version) decode() (*v1.ExpandedProfile, error) {
	e := &v1.ExpandedProfile{}
	if err := protojson.Unmarshal(v.expanded, e); err != nil {
		return nil, fmt.Errorf("profile: a stored expanded form does not read: %w", err)
	}
	return e, nil
}

// Ref names a version of a profile: "name@number", or "name" for its latest
// version.
type Ref struct {
	// Name is the profile's name.
	Name string
	// Version is the version's number; zero for the latest.
	Version int32
}

// ParseRef reads "name@number" or "name".
func ParseRef(s string) (Ref, error) {
	name, num, hasNum := strings.Cut(s, "@")
	if !ValidName(name) {
		return Ref{}, fmt.Errorf("profile: %q does not start with a profile's name", clipText(s))
	}
	if !hasNum {
		return Ref{Name: name}, nil
	}
	n, err := strconv.ParseInt(num, 10, 32)
	if err != nil || n < 1 || n > MaxVersions || strconv.FormatInt(n, 10) != num {
		return Ref{}, fmt.Errorf("profile: %q: a version is a number from 1 to %d, written without leading zeros", clipText(s), MaxVersions)
	}
	return Ref{Name: name, Version: int32(n)}, nil
}

// String writes the reference as ParseRef reads it.
func (r Ref) String() string {
	if r.Version == 0 {
		return r.Name
	}
	return r.Name + "@" + strconv.Itoa(int(r.Version))
}
