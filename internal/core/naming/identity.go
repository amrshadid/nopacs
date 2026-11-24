package naming

import (
	"encoding/binary"
	"strings"
)

// The attributes a subject identity is read from (PS3.3 C.7.1.1 and its
// Issuer of Patient ID Macro, Table 10-18).
const (
	// TagPatientID is Patient ID (0010,0020).
	TagPatientID Tag = 0x00100020
	// TagIssuerOfPatientID is Issuer of Patient ID (0010,0021).
	TagIssuerOfPatientID Tag = 0x00100021
	// TagIssuerQualifiers is Issuer of Patient ID Qualifiers Sequence
	// (0010,0024).
	TagIssuerQualifiers Tag = 0x00100024
)

// IdentityPlaceholder is the name a template's {#...} placeholder gives the
// keyed hash of a file's subject identity (SubjectIdentity.Key): the Patient
// ID together with the issuer that gave it, or the receiver's namespace when
// the file names no issuer. It is not a keyword of PS3.6, and is only ever
// hashed: the identity is not text a label may hold.
const IdentityPlaceholder = "SubjectIdentity"

// IdentityVersion is the version of the encoding SubjectIdentity.Key writes
// for an identity that names an issuer or a namespace. It is written into
// the encoding, so a later change of what an identity holds is a new
// version whose keys cannot equal this one's, and the subjects it made can
// be found and moved.
const IdentityVersion = 1

// identityMark starts every versioned encoding. A Patient ID is LO text,
// which holds no control character but ESC (PS3.5 6.1.2.3, 6.2), and
// ReadSubjectIdentity refuses one that holds a NUL, so no Patient ID alone,
// which is the encoding of an identity with no issuer and no namespace, can
// begin with it.
const identityMark = 0x00

// QualifierPaths are the values of the Issuer of Patient ID Qualifiers
// Sequence (0010,0024) that name an issuer, each a path through the first
// item of each sequence it passes (the standard permits one item, PS3.3
// Table 10-18), in the order SubjectIdentity.Qualifiers holds them: the
// Universal Entity ID (0040,0032) and its Type (0040,0033), the Identifier
// Type Code (0040,0035); the Assigning Facility Sequence's (0040,0036)
// Local Namespace Entity ID (0040,0031), Universal Entity ID and its Type
// (the HL7v2 Hierarchic Designator Macro, Table 10-17); and of the
// Assigning Jurisdiction Code Sequence (0040,0039) and the Assigning Agency
// or Department Code Sequence (0040,003A), the Code Value (0008,0100), the
// Coding Scheme Designator (0008,0102), the Long Code Value (0008,0119) and
// the URN Code Value (0008,0120). A Code Meaning is not among them: it is
// words, which two senders may write differently for one code, and one
// issuer would then become two.
var QualifierPaths = [...][]Tag{
	{TagIssuerQualifiers, 0x00400032},
	{TagIssuerQualifiers, 0x00400033},
	{TagIssuerQualifiers, 0x00400035},
	{TagIssuerQualifiers, 0x00400036, 0x00400031},
	{TagIssuerQualifiers, 0x00400036, 0x00400032},
	{TagIssuerQualifiers, 0x00400036, 0x00400033},
	{TagIssuerQualifiers, 0x00400039, 0x00080100},
	{TagIssuerQualifiers, 0x00400039, 0x00080102},
	{TagIssuerQualifiers, 0x00400039, 0x00080119},
	{TagIssuerQualifiers, 0x00400039, 0x00080120},
	{TagIssuerQualifiers, 0x0040003A, 0x00080100},
	{TagIssuerQualifiers, 0x0040003A, 0x00080102},
	{TagIssuerQualifiers, 0x0040003A, 0x00080119},
	{TagIssuerQualifiers, 0x0040003A, 0x00080120},
}

// QualifierNames are the names of QualifierPaths, in their order, as a
// person writes them where a qualifier is given by name: the keyword of the
// value, after the keyword of the sequence it is in when that is not the
// qualifiers sequence itself.
var QualifierNames = [len(QualifierPaths)]string{
	"UniversalEntityID",
	"UniversalEntityIDType",
	"IdentifierTypeCode",
	"AssigningFacilitySequence.LocalNamespaceEntityID",
	"AssigningFacilitySequence.UniversalEntityID",
	"AssigningFacilitySequence.UniversalEntityIDType",
	"AssigningJurisdictionCodeSequence.CodeValue",
	"AssigningJurisdictionCodeSequence.CodingSchemeDesignator",
	"AssigningJurisdictionCodeSequence.LongCodeValue",
	"AssigningJurisdictionCodeSequence.URNCodeValue",
	"AssigningAgencyOrDepartmentCodeSequence.CodeValue",
	"AssigningAgencyOrDepartmentCodeSequence.CodingSchemeDesignator",
	"AssigningAgencyOrDepartmentCodeSequence.LongCodeValue",
	"AssigningAgencyOrDepartmentCodeSequence.URNCodeValue",
}

// ItemAttributes is Attributes that also reads values inside sequences, and
// says when a value is there but cannot be read.
type ItemAttributes interface {
	Attributes
	// Item returns the value at path as text with its padding removed:
	// path is one tag for a top-level value, or a sequence's tag, then the
	// tags of the sequences inside it, each in the first item of the one
	// before, and last the tag of the value in the first item of the
	// innermost. A value inside an item is decoded under the item's own
	// Specific Character Set (0008,0005) when it declares one, else the
	// one it inherits (PS3.5 6.1.2.3, PS3.3 C.12.1.1.2). It reports false
	// when a sequence on the way, its first item, or the value is absent,
	// and an error when the value is there but is not text it holds in
	// memory: left in the file unread, a sequence, or longer than
	// MaxValueLength. A value that cannot be read is never answered as
	// empty: an issuer read as empty would be no issuer, and its patient
	// another population's.
	Item(path []Tag) (value string, present bool, err error)
}

// SubjectIdentity is who a file's subject is, as the archive tells two
// subjects apart: the Patient ID and the issuer that gave it, since two
// hospitals may give one Patient ID to two patients (PS3.3 C.7.1.1: a
// Patient ID is unique only within its issuer). A file that names no
// issuer takes the namespace of the receiver it came through, so two
// receivers whose senders share no Patient IDs can be kept apart, and one
// receiver's files stay one population.
type SubjectIdentity struct {
	// PatientID is the Patient ID (0010,0020), its padding removed.
	PatientID string
	// Issuer is the Issuer of Patient ID (0010,0021), its padding
	// removed; empty when the file names none.
	Issuer string
	// Qualifiers are the values of QualifierPaths, in their order, each
	// its padding removed; empty where the file has none.
	Qualifiers [len(QualifierPaths)]string
	// Namespace is the receiver's namespace, empty for the tenant's
	// default. It is part of the identity only when the file names no
	// issuer (HasIssuer): an issuer names the population itself, so one
	// issuer's files through two receivers are one subject.
	Namespace string
}

// HasIssuer reports whether the identity names an issuer: an Issuer of
// Patient ID or any qualifier with a value.
func (s SubjectIdentity) HasIssuer() bool {
	if s.Issuer != "" {
		return true
	}
	for _, q := range s.Qualifiers {
		if q != "" {
			return true
		}
	}
	return false
}

// Key returns the bytes the identity's keyed hash and date shift are taken
// over, the same bytes for the same identity and different bytes for two
// different ones.
//
// An identity that names no issuer, under the tenant's default namespace,
// is its Patient ID alone, exactly the bytes a Patient ID was keyed by
// before issuers were told apart: so such a subject keeps its label, its
// date shift and its lookup codes. Any other identity is identityMark, the
// IdentityVersion byte, then each field length-prefixed (an unsigned
// varint of its length, then its bytes): the Patient ID, the issuer, each
// qualifier in its order, and the namespace when there is no issuer. The
// count of fields is fixed for a version and every field carries its
// length, so no two identities share an encoding, whatever bytes their
// values hold; and the mark keeps every such encoding apart from a bare
// Patient ID.
func (s SubjectIdentity) Key() []byte {
	issuer := s.HasIssuer()
	if !issuer && s.Namespace == "" {
		return []byte(s.PatientID)
	}
	ns := s.Namespace
	if issuer {
		ns = ""
	}
	n := 2 + len(s.PatientID) + len(s.Issuer) + len(ns) + (3+len(s.Qualifiers))*binary.MaxVarintLen64
	for _, q := range s.Qualifiers {
		n += len(q)
	}
	out := make([]byte, 0, n)
	out = append(out, identityMark, IdentityVersion)
	field := func(v string) {
		out = binary.AppendUvarint(out, uint64(len(v)))
		out = append(out, v...)
	}
	field(s.PatientID)
	field(s.Issuer)
	for _, q := range s.Qualifiers {
		field(q)
	}
	field(ns)
	return out
}

// ReadSubjectIdentity reads the subject identity of the data set a gives,
// under the receiver's namespace ns (empty for the tenant's default). An
// *Error with Level "subject" names the attribute and the fault: a Patient
// ID missing or empty (a file that names no patient has no subject), a
// value longer than MaxValueLength, or a Patient ID holding a NUL, which LO
// text never holds and which would let a Patient ID pose as another
// identity's encoding. A value that is there but cannot be read (left in
// the file, a sequence, too long: ItemAttributes.Item's error) is the fault
// TooLong on its attribute, never an absent or empty value. The issuer and
// the qualifiers are read only from an a that implements ItemAttributes;
// one that does not, while the data set holds the Issuer of Patient ID or
// the qualifiers sequence, is refused with the fault Missing on it, so an
// issuer is never dropped because the caller could not read it.
func ReadSubjectIdentity(a Attributes, ns string) (SubjectIdentity, error) {
	id, err := readIdentity(a, ns)
	if err != nil {
		return id, err
	}
	return id, nil
}

// readIdentity is ReadSubjectIdentity, its error an *Error.
func readIdentity(a Attributes, ns string) (SubjectIdentity, *Error) {
	fail := func(attr string, f Fault) *Error { return &Error{Level: "subject", Attribute: attr, Fault: f} }
	id := SubjectIdentity{Namespace: ns}
	items, nested := a.(ItemAttributes)
	if !nested {
		v, ok := a.Lookup(TagPatientID)
		if err := checkPatientID(v, ok); err != nil {
			return id, err
		}
		id.PatientID = v
		for _, t := range []Tag{TagIssuerOfPatientID, TagIssuerQualifiers} {
			if _, ok := a.Lookup(t); ok {
				return id, fail(t.String(), Missing)
			}
		}
		return id, nil
	}
	v, ok, err := items.Item([]Tag{TagPatientID})
	if err != nil {
		return id, fail("PatientID", TooLong)
	}
	if err := checkPatientID(v, ok); err != nil {
		return id, err
	}
	id.PatientID = v
	v, _, err = items.Item([]Tag{TagIssuerOfPatientID})
	if err != nil || len(v) > MaxValueLength {
		return id, fail("IssuerOfPatientID", TooLong)
	}
	id.Issuer = v
	for i, p := range QualifierPaths {
		v, _, err := items.Item(p)
		if err != nil || len(v) > MaxValueLength {
			return id, fail(TagIssuerQualifiers.String(), TooLong)
		}
		id.Qualifiers[i] = v
	}
	return id, nil
}

// checkPatientID refuses a Patient ID that cannot be a subject's: absent,
// empty, longer than MaxValueLength, or holding a NUL, which LO text never
// holds (PS3.5 6.2) and which would let a Patient ID pose as another
// identity's encoding (identityMark).
func checkPatientID(v string, present bool) *Error {
	fail := func(f Fault) *Error { return &Error{Level: "subject", Attribute: "PatientID", Fault: f} }
	switch {
	case !present:
		return fail(Missing)
	case v == "":
		return fail(Empty)
	case len(v) > MaxValueLength:
		return fail(TooLong)
	case strings.IndexByte(v, 0) >= 0:
		return fail(BadCharacter)
	}
	return nil
}
