package engine

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/core/naming"
)

// The floor: attributes that name or identify the person, which the engine
// never keeps, at any depth, whatever the profile decides. A profile that
// would keep one (keep; clean, which leaves what it does not recognize;
// redact, which keeps what it does not cut) gets remove-unless-required with a dummy instead, so the file stays valid
// by its IOD and no original value stays; an action that replaces the value
// (hash, lookup, replace, shift, generalize, dummy, empty, remove) is left
// as the profile says. Each one's original value is a needle whenever it is
// removed or changed, so the check after searches for it whatever the
// profile decided; a value an action leaves as it was (a birth date already
// the first of January, generalized) stays, and is no needle.
//
// The reason is defense in depth: a profile, or the code that decides from
// it, that wrongly answers keep for one of these would leak the person's
// identity with nothing to catch it, since needles otherwise come only from
// values a rule removed. A floor the profile cannot lower makes one layer's
// mistake not a leak.
//
// PS3.15 Table E.1-1 (2026d) gives every attribute here an action that
// removes or replaces its value in the basic profile, and no option column
// keeps it: none of the eleven options (Retain Patient Characteristics,
// Retain Longitudinal Temporal Information among them) names Patient's
// Name, Patient ID, birth date or time, addresses or telephone numbers.
// So no profile built on Annex E has a retain option that would lower the
// floor, and the engine reads none.

// floorTags are the attributes of the floor besides every person's name
// (needleVR), with the code PS3.15 Table E.1-1's basic profile gives each.
var floorTags = map[uint32]string{
	0x00100010: "Patient's Name (0010,0010), Z in PS3.15 Table E.1-1",
	0x00100020: "Patient ID (0010,0020), Z/D in PS3.15 Table E.1-1",
	0x00101000: "Other Patient IDs (0010,1000), X in PS3.15 Table E.1-1",
	0x00101001: "Other Patient Names (0010,1001), X in PS3.15 Table E.1-1",
	0x00101002: "Other Patient IDs Sequence (0010,1002), X in PS3.15 Table E.1-1",
	0x00101005: "Patient's Birth Name (0010,1005), X in PS3.15 Table E.1-1",
	0x00100030: "Patient's Birth Date (0010,0030), Z in PS3.15 Table E.1-1",
	0x00100032: "Patient's Birth Time (0010,0032), X in PS3.15 Table E.1-1",
	0x00101040: "Patient's Address (0010,1040), X in PS3.15 Table E.1-1",
	0x00101060: "Patient's Mother's Birth Name (0010,1060), X in PS3.15 Table E.1-1",
	0x00102154: "Patient's Telephone Numbers (0010,2154), X in PS3.15 Table E.1-1",
	0x00102155: "Patient's Telecom Information (0010,2155), X in PS3.15 Table E.1-1",
	0x00401102: "Person's Address (0040,1102), X in PS3.15 Table E.1-1",
	0x00401103: "Person's Telephone Numbers (0040,1103), X in PS3.15 Table E.1-1",
	0x00401104: "Person's Telecom Information (0040,1104), X in PS3.15 Table E.1-1",
	0x00080092: "Referring Physician's Address (0008,0092), X in PS3.15 Table E.1-1",
	0x00080094: "Referring Physician's Telephone Numbers (0008,0094), X in PS3.15 Table E.1-1",
	0x00102150: "Country of Residence (0010,2150), X in PS3.15 Table E.1-1",
	0x00102152: "Region of Residence (0010,2152), X in PS3.15 Table E.1-1",
	0x00101090: "Medical Record Locator (0010,1090), X in PS3.15 Table E.1-1",
	0x00380400: "Patient's Institution Residence (0038,0400), X in PS3.15 Table E.1-1",
	0x00401101: "Person Identification Code Sequence (0040,1101), D in PS3.15 Table E.1-1",
}

// floorReason returns why the floor covers an element of tag, of VR vr
// as the engine reads it and nvr as its values are needles (needleVR), and
// "" when it does not. Every person's name is covered: PS3.15 Table E.1-1
// removes or replaces every attribute of VR PN it lists.
func floorReason(tag uint32, vr, nvr string) string {
	if r, ok := floorTags[tag]; ok {
		return r
	}
	switch {
	case nvr != "PN":
		return ""
	case vr == "PN":
		return "a person's name (VR PN), which PS3.15 Table E.1-1 removes or replaces wherever it lists one"
	case vr == "UN":
		return "a private value of unknown VR that reads as a person's name, which PS3.15 Table E.1-1 removes or replaces wherever it lists one"
	}
	return "a person's name (VR PN in the data dictionary, whatever VR the file wrote it with), which PS3.15 Table E.1-1 removes or replaces wherever it lists one"
}

// needleVR returns the VR an element's values are taken as by the floor
// and as needles: PN for a person's name, whatever VR the file wrote it
// with, and the element's VR as the engine reads it (effectiveVR)
// otherwise. A person's name is an element of VR PN; a standard attribute
// the data dictionary gives VR PN, written with another VR (Referring
// Physician's Name as LO), since the VR a file writes does not change what
// an attribute holds (PS3.6); and a private element of VR UN whose value
// reads as a name (looksLikeName).
//
// A private element's VR is not known from the data dictionary, and an
// implicit VR file gives every private element VR UN (PS3.5 6.2.2), so its
// VR in the file says nothing of what it holds. A private value of VR UN
// is taken as text for the needles, and as a name, which the floor
// removes, whenever it reads as one, PS3.15 Table E.3.10-1 listing its
// attribute or not: the table says which attributes are safe, not what a
// file wrote into them (keepSafePrivate gives a listed one its VR only
// when it is kept and is not taken for a name). That errs toward removing:
// a private element no rule keeps is removed anyway.
func needleVR(t tag.Tag, vr, raw string) string {
	switch {
	case vr == "PN":
		return "PN"
	case t.Group()%2 == 0:
		if slices.Contains(strings.Split(t.GetVR(), " or "), "PN") {
			return "PN"
		}
	case vr == "UN":
		if v, ok := unknownText(naming.TrimValue("UN", raw)); ok && looksLikeName(v) {
			return "PN"
		}
	}
	return vr
}

// unknownText returns a value of VR UN as text, and whether it reads as
// text at all: every character printable once decoded. Its bytes are as
// the file wrote them, never decoded by the DICOM library, which declares
// ISO_IR 192 over the data set once it has decoded the rest, so the source
// character set is no longer known here: bytes that are valid UTF-8 (ASCII
// among them) are read as UTF-8, and others as ISO 8859-1, the single-byte
// set most files that are not UTF-8 use, in which every byte is a
// character. The raw bytes are searched by the check after as well.
func unknownText(v string) (string, bool) {
	if !utf8.ValidString(v) {
		runes := make([]rune, len(v))
		for i := range len(v) {
			runes[i] = rune(v[i])
		}
		v = string(runes)
	}
	for _, r := range v {
		if !unicode.IsPrint(r) {
			return "", false
		}
	}
	return v, v != ""
}

// looksLikeName reports whether a text value has the form of a person's
// name (PS3.5 6.2.1): each of its values holds a component separator "^"
// and only letters, marks, spaces and the punctuation names are written
// with, with at least two letters. A name written without "^" ("John
// Doe") is not recognized: it is as likely a protocol's name.
func looksLikeName(v string) bool {
	for _, part := range strings.Split(v, `\`) {
		if part == "" {
			continue
		}
		if !strings.Contains(part, "^") {
			return false
		}
		letters := 0
		for _, r := range part {
			switch {
			case unicode.IsLetter(r):
				letters++
			case unicode.IsMark(r), strings.ContainsRune(" ^=.,'-", r):
			default:
				return false
			}
		}
		if letters < 2 {
			return false
		}
	}
	return strings.Contains(v, "^")
}

// floorAction returns the action the engine takes instead of a, for an
// element the floor covers, and whether it differs: a itself when a
// replaces or removes the value; when a would keep it, removal for a
// private element (no IOD requires one) and remove-unless-required with a
// dummy for a standard one, so the file stays valid by its IOD.
func floorAction(a Action, private bool) (Action, bool) {
	instead := Action{Kind: ActRemoveUnlessRequired, Fallback: FallbackDummy}
	if private {
		instead = Action{Kind: ActRemove}
	}
	switch a.Kind {
	case ActKeep, ActClean, ActRedact:
		// Each leaves what it does not recognize or cut.
		return instead, true
	case ActRemoveUnlessRequired:
		// A UID fallback keeps a sequence for the UIDs inside, which for
		// these attributes would keep what they hold.
		if a.Fallback == FallbackUID || private {
			return instead, true
		}
	}
	return a, false
}
